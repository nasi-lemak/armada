package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

const historySchema = `
CREATE TABLE IF NOT EXISTS series (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	unit TEXT NOT NULL DEFAULT '',
	UNIQUE(device_id, name)
);
CREATE TABLE IF NOT EXISTS samples (
	series_id INTEGER NOT NULL REFERENCES series(id) ON DELETE CASCADE,
	ts INTEGER NOT NULL,
	value REAL,
	status INTEGER NOT NULL,
	PRIMARY KEY (series_id, ts)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS samples_ts ON samples(ts);
CREATE TABLE IF NOT EXISTS samples_hourly (
	series_id INTEGER NOT NULL REFERENCES series(id) ON DELETE CASCADE,
	hour INTEGER NOT NULL,
	vmin REAL, vavg REAL, vmax REAL,
	worst INTEGER NOT NULL,
	n INTEGER NOT NULL,
	ok INTEGER NOT NULL,
	PRIMARY KEY (series_id, hour)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS samples_hourly_hour ON samples_hourly(hour);
CREATE TABLE IF NOT EXISTS fleet_samples (
	ts INTEGER PRIMARY KEY,
	online INTEGER NOT NULL, offline INTEGER NOT NULL,
	warning INTEGER NOT NULL, critical INTEGER NOT NULL, alerts INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// Status ranks stored in samples (higher is worse).
var statusNames = []string{api.HealthOK, api.HealthUnknown, api.HealthWarning, api.HealthCritical}

func statusName(rank int) string {
	if rank < 0 || rank >= len(statusNames) {
		return api.HealthUnknown
	}
	return statusNames[rank]
}

// Sample is one measurement of one series.
type Sample struct {
	DeviceID, Name, Kind, Unit string
	At                         time.Time
	Value                      *float64
	Status                     string
}

// AddSamples records measurements, creating series as needed. A sample for
// a series and second that already exists is ignored.
func (s *Store) AddSamples(ctx context.Context, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sm := range samples {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM series WHERE device_id=? AND name=?`, sm.DeviceID, sm.Name).Scan(&id)
		if err == sql.ErrNoRows {
			res, err := tx.ExecContext(ctx, `INSERT INTO series (device_id, name, kind, unit) VALUES (?,?,?,?)`, sm.DeviceID, sm.Name, sm.Kind, sm.Unit)
			if err != nil {
				return err
			}
			if id, err = res.LastInsertId(); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		var v any
		if sm.Value != nil {
			v = *sm.Value
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO samples (series_id, ts, value, status) VALUES (?,?,?,?)`,
			id, sm.At.Unix(), v, api.HealthRank(sm.Status)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddFleetSample records the fleet's state.
func (s *Store) AddFleetSample(ctx context.Context, at time.Time, online, offline, warning, critical, alerts int) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO fleet_samples (ts, online, offline, warning, critical, alerts) VALUES (?,?,?,?,?,?)`,
		at.Unix(), online, offline, warning, critical, alerts)
	return err
}

func (s *Store) getMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) setMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES (?,?)`, key, value)
	return err
}

// rolledUntil is the start of the first hour not yet rolled up.
func (s *Store) rolledUntil(ctx context.Context) (int64, error) {
	v, err := s.getMeta(ctx, "history_rolled_until")
	if err != nil || v == "" {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// CompactHistory rolls complete hours of raw samples into hourly summaries
// and deletes data past its retention.
func (s *Store) CompactHistory(ctx context.Context, now time.Time, rawRetention, retention time.Duration) error {
	until, err := s.rolledUntil(ctx)
	if err != nil {
		return err
	}
	cur := now.Truncate(time.Hour).Unix()
	if until < cur {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO samples_hourly (series_id, hour, vmin, vavg, vmax, worst, n, ok)
			SELECT series_id, (ts/3600)*3600 AS h, MIN(value), AVG(value), MAX(value), MAX(status), COUNT(*), SUM(status = 0)
			FROM samples WHERE ts >= ? AND ts < ? GROUP BY series_id, h
			ON CONFLICT(series_id, hour) DO UPDATE SET vmin=excluded.vmin, vavg=excluded.vavg, vmax=excluded.vmax,
				worst=excluded.worst, n=excluded.n, ok=excluded.ok`, until, cur); err != nil {
			return fmt.Errorf("roll up history: %w", err)
		}
		if err := s.setMeta(ctx, "history_rolled_until", strconv.FormatInt(cur, 10)); err != nil {
			return err
		}
	}
	stmts := []struct {
		q   string
		arg int64
	}{
		{`DELETE FROM samples WHERE ts < ?`, now.Add(-rawRetention).Unix()},
		{`DELETE FROM samples_hourly WHERE hour < ?`, now.Add(-retention).Unix()},
		{`DELETE FROM fleet_samples WHERE ts < ?`, now.Add(-retention).Unix()},
	}
	for _, st := range stmts {
		if _, err := s.db.ExecContext(ctx, st.q, st.arg); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM series WHERE NOT EXISTS (SELECT 1 FROM samples WHERE series_id = series.id)
		AND NOT EXISTS (SELECT 1 FROM samples_hourly WHERE series_id = series.id)`)
	return err
}

// seriesData is a series with its points and status counts in a range.
type seriesData struct {
	api.HistorySeries
	ok, n int
}

// DeviceHistory returns every series of a device from `from` on: raw samples,
// or hourly summaries when hourly is set (including hours not yet rolled up).
// For each series it also returns how many samples were ok and in total.
func (s *Store) DeviceHistory(ctx context.Context, deviceID string, from time.Time, hourly bool) ([]api.HistorySeries, []int, []int, error) {
	return s.history(ctx, "device_id", deviceID, from, hourly)
}

// CheckHistory returns the series named `name` on every device that has one
// (for comparing one check across the fleet).
func (s *Store) CheckHistory(ctx context.Context, name string, from time.Time, hourly bool) ([]api.HistorySeries, []int, []int, error) {
	return s.history(ctx, "name", name, from, hourly)
}

// SeriesNames lists distinct series names with how many devices have each.
func (s *Store) SeriesNames(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, COUNT(*) FROM series GROUP BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var n string
		var c int
		if err := rows.Scan(&n, &c); err != nil {
			return nil, err
		}
		out[n] = c
	}
	return out, rows.Err()
}

func (s *Store) history(ctx context.Context, col, val string, from time.Time, hourly bool) ([]api.HistorySeries, []int, []int, error) {
	if col != "device_id" && col != "name" {
		return nil, nil, nil, fmt.Errorf("bad history column %q", col)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, kind, unit, device_id FROM series WHERE `+col+`=? ORDER BY name, device_id`, val)
	if err != nil {
		return nil, nil, nil, err
	}
	var list []*seriesData
	byID := map[int64]*seriesData{}
	for rows.Next() {
		var id int64
		sd := &seriesData{}
		if err := rows.Scan(&id, &sd.Name, &sd.Kind, &sd.Unit, &sd.DeviceID); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		sd.Points = []api.HistoryPoint{}
		list = append(list, sd)
		byID[id] = sd
	}
	rows.Close()
	if len(list) == 0 {
		return []api.HistorySeries{}, nil, nil, nil
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	in := strings.Join(ids, ",") // integers from our own table
	f := from.Unix()

	scanPoints := func(q string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, t int64
			var vmin, vavg, vmax sql.NullFloat64
			var worst, n, ok int
			if err := rows.Scan(&id, &t, &vmin, &vavg, &vmax, &worst, &n, &ok); err != nil {
				return err
			}
			sd := byID[id]
			p := api.HistoryPoint{T: t, S: statusName(worst)}
			if vavg.Valid {
				// Averages pick up float noise (89.90000000000002); 4 decimals is plenty.
				avg, lo, hi := round4(vavg.Float64), round4(vmin.Float64), round4(vmax.Float64)
				p.V = &avg
				sd.Numeric = true
				if hourly {
					p.Min, p.Max = &lo, &hi
				}
			}
			sd.Points = append(sd.Points, p)
			sd.n += n
			sd.ok += ok
		}
		return rows.Err()
	}
	if !hourly {
		err = scanPoints(`SELECT series_id, ts, value, value, value, status, 1, status = 0 FROM samples
			WHERE series_id IN (`+in+`) AND ts >= ? ORDER BY series_id, ts`, f)
	} else {
		until, err2 := s.rolledUntil(ctx)
		if err2 != nil {
			return nil, nil, nil, err2
		}
		err = scanPoints(`SELECT series_id, hour, vmin, vavg, vmax, worst, n, ok FROM samples_hourly
			WHERE series_id IN (`+in+`) AND hour >= ? AND hour < ? ORDER BY series_id, hour`, f, until)
		if err == nil {
			err = scanPoints(`SELECT series_id, (ts/3600)*3600 AS h, MIN(value), AVG(value), MAX(value), MAX(status), COUNT(*), SUM(status = 0)
				FROM samples WHERE series_id IN (`+in+`) AND ts >= ? GROUP BY series_id, h ORDER BY series_id, h`, max(until, f))
		}
	}
	if err != nil {
		return nil, nil, nil, err
	}
	out := make([]api.HistorySeries, len(list))
	oks, ns := make([]int, len(list)), make([]int, len(list))
	for i, sd := range list {
		out[i], oks[i], ns[i] = sd.HistorySeries, sd.ok, sd.n
	}
	return out, oks, ns, nil
}

// FleetHistory averages fleet samples into buckets of `bucket` seconds.
func (s *Store) FleetHistory(ctx context.Context, from time.Time, bucket int64) ([]api.FleetPoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT (ts/?)*? AS b, AVG(online), AVG(offline), AVG(warning), AVG(critical), AVG(alerts)
		FROM fleet_samples WHERE ts >= ? GROUP BY b ORDER BY b`, bucket, bucket, from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.FleetPoint{}
	for rows.Next() {
		var p api.FleetPoint
		if err := rows.Scan(&p.T, &p.Online, &p.Offline, &p.Warning, &p.Critical, &p.Alerts); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DB exposes the database handle to tests.
func (s *Store) DB() *sql.DB { return s.db }

// Snapshot writes a consistent copy of the database to path while the
// server keeps running (SQLite VACUUM INTO).
func (s *Store) Snapshot(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }
