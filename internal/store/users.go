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

const usersSchema = `
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	email TEXT NOT NULL UNIQUE COLLATE NOCASE,
	password_hash TEXT NOT NULL,
	spec TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS user_tokens (
	id TEXT PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL DEFAULT 0
);
`

func scanUser(sc scanner) (*api.User, string, error) {
	var spec, hash string
	if err := sc.Scan(&spec, &hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	var u api.User
	if err := json.Unmarshal([]byte(spec), &u); err != nil {
		return nil, "", err
	}
	return &u, hash, nil
}

// CreateUser stores a new user with a password hash.
func (s *Store) CreateUser(ctx context.Context, u *api.User, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users (id, email, password_hash, spec) VALUES (?,?,?,?)`,
		u.ID, strings.ToLower(u.Email), passwordHash, mustJSON(u))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// UpdateUser saves a user's profile (not the password).
func (s *Store) UpdateUser(ctx context.Context, u *api.User) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET spec=? WHERE id=?`, mustJSON(u), u.ID)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// SetPassword replaces a user's password hash and ends their sessions.
func (s *Store) SetPassword(ctx context.Context, id, hash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, id)
	if err != nil {
		return err
	}
	if err := expectOne(res); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id)
	return err
}

// GetUser loads a user by ID.
func (s *Store) GetUser(ctx context.Context, id string) (*api.User, error) {
	u, _, err := scanUser(s.db.QueryRowContext(ctx, `SELECT spec, password_hash FROM users WHERE id=?`, id))
	return u, err
}

// UserByEmail loads a user and their password hash.
func (s *Store) UserByEmail(ctx context.Context, email string) (*api.User, string, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT spec, password_hash FROM users WHERE email=?`, strings.ToLower(email)))
}

// ListUsers returns all users by email.
func (s *Store) ListUsers(ctx context.Context) ([]*api.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec, password_hash FROM users ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.User
	for rows.Next() {
		u, _, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUser removes a user, their sessions and tokens.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// CreateSession stores a session token hash.
func (s *Store) CreateSession(ctx context.Context, id, tokenHash, userID string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, token_hash, user_id, expires_at) VALUES (?,?,?,?)`, id, tokenHash, userID, ts(expires))
	return err
}

// SessionUser resolves a session token hash to its user, extending the
// session (sliding expiry) when more than half of its lifetime has passed.
func (s *Store) SessionUser(ctx context.Context, tokenHash string, now time.Time, ttl time.Duration) (*api.User, error) {
	var userID string
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, expires_at FROM sessions WHERE token_hash=?`, tokenHash).Scan(&userID, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if now.After(fromTS(exp)) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
		return nil, ErrNotFound
	}
	if fromTS(exp).Sub(now) < ttl/2 {
		_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET expires_at=? WHERE token_hash=?`, ts(now.Add(ttl)), tokenHash)
	}
	return s.GetUser(ctx, userID)
}

// DeleteSession ends a session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

// PruneSessions deletes expired sessions.
func (s *Store) PruneSessions(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, ts(now))
	return err
}

// CreateUserToken stores a personal API token hash.
func (s *Store) CreateUserToken(ctx context.Context, userID string, t *api.APIToken, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_tokens (id, token_hash, user_id, name, created_at) VALUES (?,?,?,?,?)`,
		t.ID, tokenHash, userID, t.Name, ts(t.CreatedAt))
	return err
}

// TokenUser resolves a personal API token hash to its user and records its use.
func (s *Store) TokenUser(ctx context.Context, tokenHash string, now time.Time) (*api.User, error) {
	var userID string
	var last int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, last_used_at FROM user_tokens WHERE token_hash=?`, tokenHash).Scan(&userID, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if now.Sub(fromTS(last)) > time.Minute {
		_, _ = s.db.ExecContext(ctx, `UPDATE user_tokens SET last_used_at=? WHERE token_hash=?`, ts(now), tokenHash)
	}
	return s.GetUser(ctx, userID)
}

// ListUserTokens lists a user's personal API tokens.
func (s *Store) ListUserTokens(ctx context.Context, userID string) ([]*api.APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, last_used_at FROM user_tokens WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*api.APIToken{}
	for rows.Next() {
		var t api.APIToken
		var created, last int64
		if err := rows.Scan(&t.ID, &t.Name, &created, &last); err != nil {
			return nil, err
		}
		t.CreatedAt = fromTS(created)
		if last > 0 {
			l := fromTS(last)
			t.LastUsedAt = &l
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// DeleteUserToken revokes one of a user's tokens.
func (s *Store) DeleteUserToken(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_tokens WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return err
	}
	return expectOne(res)
}
