package store

import (
	"context"
	"database/sql"
	"time"
)

const passkeySchema = `
CREATE TABLE IF NOT EXISTS user_passkeys (
	id           TEXT PRIMARY KEY,           -- credential ID, base64url
	user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	credential   TEXT NOT NULL,              -- the WebAuthn credential record (public key, counter, flags), JSON
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS user_passkeys_user ON user_passkeys(user_id);
`

// StoredPasskey is a passkey record. Credential is opaque JSON owned by the server package.
type StoredPasskey struct {
	ID         string
	UserID     string
	Name       string
	Credential string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// AddPasskey stores a new passkey.
func (s *Store) AddPasskey(ctx context.Context, p StoredPasskey) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_passkeys (id, user_id, name, credential, created_at) VALUES (?,?,?,?,?)`,
		p.ID, p.UserID, p.Name, p.Credential, ts(p.CreatedAt))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

func scanPasskeys(rows *sql.Rows) ([]StoredPasskey, error) {
	defer rows.Close()
	var out []StoredPasskey
	for rows.Next() {
		var p StoredPasskey
		var created, used int64
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Credential, &created, &used); err != nil {
			return nil, err
		}
		p.CreatedAt = fromTS(created)
		if used > 0 {
			p.LastUsedAt = fromTS(used)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const passkeyCols = `id, user_id, name, credential, created_at, last_used_at`

// ListPasskeys returns a user's passkeys, oldest first.
func (s *Store) ListPasskeys(ctx context.Context, userID string) ([]StoredPasskey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+passkeyCols+` FROM user_passkeys WHERE user_id=? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	return scanPasskeys(rows)
}

// UsePasskey records a sign-in: the updated credential (signature counter) and time.
func (s *Store) UsePasskey(ctx context.Context, id, credential string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_passkeys SET credential=?, last_used_at=? WHERE id=?`, credential, ts(now), id)
	return err
}

// DeletePasskey removes one of a user's passkeys.
func (s *Store) DeletePasskey(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_passkeys WHERE user_id=? AND id=?`, userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePasskeys removes all of a user's passkeys.
func (s *Store) DeletePasskeys(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_passkeys WHERE user_id=?`, userID)
	return err
}
