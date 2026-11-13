package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// OnlineHours streams, for each device, how many "online" history samples
// it has in each hour of [from, to): the series has one per resolution
// period the device checked in. Rows come ordered by device and hour, and
// hours with no samples are left out. Hours already rolled up come from the
// hourly table, later ones from raw samples.
func (s *Store) OnlineHours(ctx context.Context, from, to time.Time, fn func(deviceID string, hour time.Time, n int)) error {
	until, err := s.rolledUntil(ctx)
	if err != nil {
		return err
	}
	f, t := from.Truncate(time.Hour).Unix(), to.Unix()
	split := min(max(until, f), t)
	rows, err := s.db.QueryContext(ctx, `SELECT device_id, hr, n FROM (
		SELECT se.device_id AS device_id, h.hour AS hr, h.n AS n FROM samples_hourly h JOIN series se ON se.id = h.series_id
			WHERE se.name = 'online' AND h.hour >= ? AND h.hour < ?
		UNION ALL
		SELECT se.device_id AS device_id, (r.ts / 3600) * 3600 AS hr, COUNT(*) AS n FROM samples r JOIN series se ON se.id = r.series_id
			WHERE se.name = 'online' AND r.ts >= ? AND r.ts < ? GROUP BY se.device_id, hr
	) ORDER BY device_id, hr`, f, split, split, t)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var hr int64
		var n int
		if err := rows.Scan(&id, &hr, &n); err != nil {
			return err
		}
		fn(id, time.Unix(hr, 0).UTC(), n)
	}
	return rows.Err()
}

// ListAlertsBetween returns alerts that started in [from, to), oldest first.
func (s *Store) ListAlertsBetween(ctx context.Context, from, to time.Time) ([]*api.Alert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec FROM alerts WHERE started_at >= ? AND started_at < ? ORDER BY started_at`, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Alert
	for rows.Next() {
		var spec string
		if err := rows.Scan(&spec); err != nil {
			return nil, err
		}
		var a api.Alert
		if err := json.Unmarshal([]byte(spec), &a); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// ListEventsBetween returns events of the given types in [from, to), oldest first.
func (s *Store) ListEventsBetween(ctx context.Context, from, to time.Time, types []string, limit int) ([]*api.Event, error) {
	q := `SELECT id, time, type, device_id, deployment_id, message, actor FROM events WHERE time >= ? AND time < ?`
	args := []any{ts(from), ts(to)}
	if len(types) > 0 {
		q += ` AND type IN (?` + strings.Repeat(",?", len(types)-1) + `)`
		for _, t := range types {
			args = append(args, t)
		}
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY time, id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Event
	for rows.Next() {
		var e api.Event
		var t int64
		if err := rows.Scan(&e.ID, &t, &e.Type, &e.DeviceID, &e.DeploymentID, &e.Message, &e.Actor); err != nil {
			return nil, err
		}
		e.Time = fromTS(t)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// DeviceUpdatesBetween returns update progress last changed in [from, to).
func (s *Store) DeviceUpdatesBetween(ctx context.Context, from, to time.Time) ([]*api.DeviceUpdate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceUpdateCols+` FROM device_updates WHERE updated_at >= ? AND updated_at < ?`, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.DeviceUpdate
	for rows.Next() {
		u, err := scanDeviceUpdate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// PutReportSchedule creates or replaces a report schedule.
func (s *Store) PutReportSchedule(ctx context.Context, r *api.ReportSchedule) error {
	return s.putDoc(ctx, "report_schedules", r.ID, r.CreatedAt, r)
}

// GetReportSchedule loads a report schedule.
func (s *Store) GetReportSchedule(ctx context.Context, id string) (*api.ReportSchedule, error) {
	var r api.ReportSchedule
	return &r, s.getDoc(ctx, "report_schedules", id, &r)
}

// ListReportSchedules returns all report schedules.
func (s *Store) ListReportSchedules(ctx context.Context) ([]*api.ReportSchedule, error) {
	return listDocs[api.ReportSchedule](ctx, s, "report_schedules")
}

// DeleteReportSchedule removes a report schedule.
func (s *Store) DeleteReportSchedule(ctx context.Context, id string) error {
	return s.deleteDoc(ctx, "report_schedules", id)
}

// PutDeviceCert creates or replaces a device's managed certificate record.
func (s *Store) PutDeviceCert(ctx context.Context, c *api.DeviceCert) error {
	c.DaysLeft = nil // computed on read
	_, err := s.db.ExecContext(ctx, `INSERT INTO device_certs (id, device_id, spec, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET spec=excluded.spec, updated_at=excluded.updated_at`, c.ID, c.DeviceID, mustJSON(c), ts(time.Now()))
	return err
}

// GetDeviceCert loads one record by ID (device ID + "/" + target).
func (s *Store) GetDeviceCert(ctx context.Context, id string) (*api.DeviceCert, error) {
	var spec string
	err := s.db.QueryRowContext(ctx, `SELECT spec FROM device_certs WHERE id=?`, id).Scan(&spec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	var c api.DeviceCert
	return &c, json.Unmarshal([]byte(spec), &c)
}

// ListDeviceCerts returns the managed certificates of one device, or of
// every device when deviceID is empty.
func (s *Store) ListDeviceCerts(ctx context.Context, deviceID string) ([]*api.DeviceCert, error) {
	q, args := `SELECT spec FROM device_certs ORDER BY id`, []any{}
	if deviceID != "" {
		q, args = `SELECT spec FROM device_certs WHERE device_id=? ORDER BY id`, []any{deviceID}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.DeviceCert
	for rows.Next() {
		var spec string
		if err := rows.Scan(&spec); err != nil {
			return nil, err
		}
		var c api.DeviceCert
		if err := json.Unmarshal([]byte(spec), &c); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// DeleteDeviceCert removes a record.
func (s *Store) DeleteDeviceCert(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM device_certs WHERE id=?`, id)
	return err
}
