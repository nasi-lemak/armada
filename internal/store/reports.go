package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// OnlineCoverage measures how much of [from, to) each device was checking
// in. The "online" history series has a sample in each resolution period a
// device checked in. Per hour, a device scores the samples it has, up to
// perHour (the number one fully online hour has); the result is the score
// summed over the range. Hours already rolled up come from the hourly
// table, later ones from raw samples.
func (s *Store) OnlineCoverage(ctx context.Context, from, to time.Time, perHour int) (map[string]int, error) {
	until, err := s.rolledUntil(ctx)
	if err != nil {
		return nil, err
	}
	f, t := from.Unix(), to.Unix()
	split := min(max(until, f), t)
	rows, err := s.db.QueryContext(ctx, `SELECT device_id, SUM(MIN(n, ?)) FROM (
		SELECT se.device_id AS device_id, h.n AS n FROM samples_hourly h JOIN series se ON se.id = h.series_id
			WHERE se.name = 'online' AND h.hour >= ? AND h.hour < ?
		UNION ALL
		SELECT se.device_id AS device_id, COUNT(*) AS n FROM samples r JOIN series se ON se.id = r.series_id
			WHERE se.name = 'online' AND r.ts >= ? AND r.ts < ? GROUP BY se.device_id, r.ts / 3600
	) GROUP BY device_id`, perHour, f, split, split, t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
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
