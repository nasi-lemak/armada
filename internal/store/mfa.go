package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

const mfaSchema = `
CREATE TABLE IF NOT EXISTS user_mfa (
	user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	secret     TEXT NOT NULL DEFAULT '',   -- encrypted TOTP secret, empty until enabled
	pending    TEXT NOT NULL DEFAULT '',   -- encrypted secret awaiting confirmation
	last_step  INTEGER NOT NULL DEFAULT 0, -- last accepted TOTP step (replay protection)
	recovery   TEXT NOT NULL DEFAULT '[]'  -- SHA-256 hashes of unused recovery codes
);
CREATE TABLE IF NOT EXISTS user_identities (
	issuer  TEXT NOT NULL,
	subject TEXT NOT NULL,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	PRIMARY KEY (issuer, subject)
);
`

// MFA is a user's second-factor state.
type MFA struct {
	Secret   string // encrypted
	Pending  string // encrypted
	LastStep int64
	Recovery []string
}

// GetMFA loads a user's second-factor state (zero value if none).
func (s *Store) GetMFA(ctx context.Context, userID string) (MFA, error) {
	var m MFA
	var rec string
	err := s.db.QueryRowContext(ctx, `SELECT secret, pending, last_step, recovery FROM user_mfa WHERE user_id=?`, userID).
		Scan(&m.Secret, &m.Pending, &m.LastStep, &rec)
	if errors.Is(err, sql.ErrNoRows) {
		return MFA{}, nil
	} else if err != nil {
		return m, err
	}
	_ = json.Unmarshal([]byte(rec), &m.Recovery)
	return m, nil
}

// PutMFA saves a user's second-factor state.
func (s *Store) PutMFA(ctx context.Context, userID string, m MFA) error {
	if m.Recovery == nil {
		m.Recovery = []string{}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_mfa (user_id, secret, pending, last_step, recovery) VALUES (?,?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET secret=excluded.secret, pending=excluded.pending, last_step=excluded.last_step, recovery=excluded.recovery`,
		userID, m.Secret, m.Pending, m.LastStep, mustJSON(m.Recovery))
	return err
}

// AdvanceMFAStep records a used TOTP step, failing if it isn't newer than
// the last one (a concurrent replay of the same code).
func (s *Store) AdvanceMFAStep(ctx context.Context, userID string, step int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE user_mfa SET last_step=? WHERE user_id=? AND last_step<?`, step, userID, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UseRecoveryCode removes a recovery code hash, reporting whether it was there.
func (s *Store) UseRecoveryCode(ctx context.Context, userID, hash string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var rec string
	if err := tx.QueryRowContext(ctx, `SELECT recovery FROM user_mfa WHERE user_id=?`, userID).Scan(&rec); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	var codes []string
	_ = json.Unmarshal([]byte(rec), &codes)
	kept := codes[:0]
	found := false
	for _, c := range codes {
		if c == hash && !found {
			found = true
			continue
		}
		kept = append(kept, c)
	}
	if !found {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE user_mfa SET recovery=? WHERE user_id=?`, mustJSON(kept), userID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// DeleteMFA removes a user's second factor.
func (s *Store) DeleteMFA(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_mfa WHERE user_id=?`, userID)
	return err
}

// IdentityUser finds the user linked to an SSO identity.
func (s *Store) IdentityUser(ctx context.Context, issuer, subject string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM user_identities WHERE issuer=? AND subject=?`, issuer, subject).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// LinkIdentity links an SSO identity to a user.
func (s *Store) LinkIdentity(ctx context.Context, issuer, subject, userID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO user_identities (issuer, subject, user_id) VALUES (?,?,?)`, issuer, subject, userID)
	return err
}

// UnlinkIdentities removes a user's SSO links.
func (s *Store) UnlinkIdentities(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_identities WHERE user_id=?`, userID)
	return err
}
