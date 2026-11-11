package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/nasi-lemak/armada/internal/api"
)

const configSchema = `
CREATE TABLE IF NOT EXISTS config_profiles (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
	spec       TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS device_config (
	device_id  TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	key        TEXT NOT NULL,
	hash       TEXT NOT NULL,
	status     TEXT NOT NULL,
	message    TEXT NOT NULL DEFAULT '',
	profile_id TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (device_id, key)
);
`

// UpsertConfigProfile creates or replaces a profile.
func (s *Store) UpsertConfigProfile(ctx context.Context, p *api.ConfigProfile) error {
	cp := *p
	cp.Compliance = nil
	_, err := s.db.ExecContext(ctx, `INSERT INTO config_profiles (id, name, spec, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, spec=excluded.spec, updated_at=excluded.updated_at`,
		p.ID, p.Name, mustJSON(cp), ts(p.UpdatedAt))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// GetConfigProfile loads a profile.
func (s *Store) GetConfigProfile(ctx context.Context, id string) (*api.ConfigProfile, error) {
	var spec string
	err := s.db.QueryRowContext(ctx, `SELECT spec FROM config_profiles WHERE id=?`, id).Scan(&spec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	var p api.ConfigProfile
	return &p, json.Unmarshal([]byte(spec), &p)
}

// ListConfigProfiles returns all profiles by name.
func (s *Store) ListConfigProfiles(ctx context.Context) ([]*api.ConfigProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec FROM config_profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.ConfigProfile
	for rows.Next() {
		var spec string
		if err := rows.Scan(&spec); err != nil {
			return nil, err
		}
		var p api.ConfigProfile
		if err := json.Unmarshal([]byte(spec), &p); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// DeleteConfigProfile removes a profile. Devices remove its configuration
// at their next check-in.
func (s *Store) DeleteConfigProfile(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM config_profiles WHERE id=?`, id)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// SetDeviceConfig replaces a device's reported configuration states.
func (s *Store) SetDeviceConfig(ctx context.Context, deviceID string, states []api.ConfigState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_config WHERE device_id=?`, deviceID); err != nil {
		return err
	}
	for _, c := range states {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO device_config (device_id, key, hash, status, message, profile_id, updated_at) VALUES (?,?,?,?,?,?,?)`,
			deviceID, c.Key, c.Hash, c.Status, c.Message, c.ProfileID, ts(c.UpdatedAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeviceConfig returns a device's reported configuration states.
func (s *Store) DeviceConfig(ctx context.Context, deviceID string) ([]api.ConfigState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, hash, status, message, profile_id, updated_at FROM device_config WHERE device_id=? ORDER BY key`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.ConfigState
	for rows.Next() {
		var c api.ConfigState
		var updated int64
		if err := rows.Scan(&c.Key, &c.Hash, &c.Status, &c.Message, &c.ProfileID, &updated); err != nil {
			return nil, err
		}
		c.UpdatedAt = fromTS(updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

// AllDeviceConfig returns every reported state, keyed by device ID.
func (s *Store) AllDeviceConfig(ctx context.Context) (map[string][]api.ConfigState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT device_id, key, hash, status, message, profile_id, updated_at FROM device_config`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]api.ConfigState{}
	for rows.Next() {
		var id string
		var c api.ConfigState
		var updated int64
		if err := rows.Scan(&id, &c.Key, &c.Hash, &c.Status, &c.Message, &c.ProfileID, &updated); err != nil {
			return nil, err
		}
		c.UpdatedAt = fromTS(updated)
		out[id] = append(out[id], c)
	}
	return out, rows.Err()
}
