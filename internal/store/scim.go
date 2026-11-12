package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

const scimSchema = `
CREATE TABLE IF NOT EXISTS scim_tokens (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	token_hash   TEXT NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS group_mappings (
	id         TEXT PRIMARY KEY,
	group_name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	spec       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS scim_groups (
	id           TEXT PRIMARY KEY,
	display_name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	spec         TEXT NOT NULL
);
`

// ---- SCIM tokens ----

// CreateSCIMToken stores a SCIM bearer token hash.
func (s *Store) CreateSCIMToken(ctx context.Context, t *api.SCIMToken, hash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO scim_tokens (id, name, token_hash, created_at) VALUES (?,?,?,?)`, t.ID, t.Name, hash, ts(t.CreatedAt))
	return err
}

// SCIMTokenByHash resolves a token, recording its use at most once a minute.
func (s *Store) SCIMTokenByHash(ctx context.Context, hash string, now time.Time) (*api.SCIMToken, error) {
	var t api.SCIMToken
	var created, used int64
	err := s.db.QueryRowContext(ctx, `SELECT id, name, created_at, last_used_at FROM scim_tokens WHERE token_hash=?`, hash).Scan(&t.ID, &t.Name, &created, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	t.CreatedAt = fromTS(created)
	if now.Sub(fromTS(used)) > time.Minute {
		_, _ = s.db.ExecContext(ctx, `UPDATE scim_tokens SET last_used_at=? WHERE id=?`, ts(now), t.ID)
		used = ts(now)
	}
	if used > 0 {
		u := fromTS(used)
		t.LastUsedAt = &u
	}
	return &t, nil
}

// ListSCIMTokens returns all SCIM tokens (without secrets).
func (s *Store) ListSCIMTokens(ctx context.Context) ([]*api.SCIMToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, last_used_at FROM scim_tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.SCIMToken
	for rows.Next() {
		var t api.SCIMToken
		var created, used int64
		if err := rows.Scan(&t.ID, &t.Name, &created, &used); err != nil {
			return nil, err
		}
		t.CreatedAt = fromTS(created)
		if used > 0 {
			u := fromTS(used)
			t.LastUsedAt = &u
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// DeleteSCIMToken revokes a SCIM token.
func (s *Store) DeleteSCIMToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM scim_tokens WHERE id=?`, id)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// ---- SCIM groups ----

// PutSCIMGroup creates or replaces a group.
func (s *Store) PutSCIMGroup(ctx context.Context, g *api.SCIMGroup) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO scim_groups (id, display_name, spec) VALUES (?,?,?)
		ON CONFLICT(id) DO UPDATE SET display_name=excluded.display_name, spec=excluded.spec`, g.ID, g.DisplayName, mustJSON(g))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// GetSCIMGroup loads a group.
func (s *Store) GetSCIMGroup(ctx context.Context, id string) (*api.SCIMGroup, error) {
	var spec string
	err := s.db.QueryRowContext(ctx, `SELECT spec FROM scim_groups WHERE id=?`, id).Scan(&spec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	var g api.SCIMGroup
	return &g, json.Unmarshal([]byte(spec), &g)
}

// ListSCIMGroups returns all groups by name.
func (s *Store) ListSCIMGroups(ctx context.Context) ([]*api.SCIMGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec FROM scim_groups ORDER BY display_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.SCIMGroup
	for rows.Next() {
		var spec string
		if err := rows.Scan(&spec); err != nil {
			return nil, err
		}
		var g api.SCIMGroup
		if err := json.Unmarshal([]byte(spec), &g); err != nil {
			return nil, err
		}
		out = append(out, &g)
	}
	return out, rows.Err()
}

// DeleteSCIMGroup removes a group.
func (s *Store) DeleteSCIMGroup(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM scim_groups WHERE id=?`, id)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// ---- group mappings ----

// PutGroupMapping creates or replaces a mapping.
func (s *Store) PutGroupMapping(ctx context.Context, m *api.GroupMapping) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO group_mappings (id, group_name, spec) VALUES (?,?,?)
		ON CONFLICT(id) DO UPDATE SET group_name=excluded.group_name, spec=excluded.spec`, m.ID, m.Group, mustJSON(m))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// ListGroupMappings returns all mappings by group name.
func (s *Store) ListGroupMappings(ctx context.Context) ([]*api.GroupMapping, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec FROM group_mappings ORDER BY group_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.GroupMapping
	for rows.Next() {
		var spec string
		if err := rows.Scan(&spec); err != nil {
			return nil, err
		}
		var m api.GroupMapping
		if err := json.Unmarshal([]byte(spec), &m); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// DeleteGroupMapping removes a mapping.
func (s *Store) DeleteGroupMapping(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM group_mappings WHERE id=?`, id)
	if err != nil {
		return err
	}
	return expectOne(res)
}
