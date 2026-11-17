package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// The activity log is a hash chain: each event's hash covers its contents
// and the previous event's hash, so editing, inserting or deleting an event
// in the middle changes every hash after it. The event_chain row says where
// the chain starts: after the events an older version recorded (which have
// no hash) and after events removed by retention, with the hash of the last
// one removed.
//
// The chain detects changes made to the database file, not a person who
// rewrites the whole chain after changing it. Against that, keep a copy of
// the chain's head outside the server (Verify returns it) or send events
// off the server as they happen (event webhooks).

// eventHash computes an event's hash from the previous event's hash and the
// event's stored fields.
func eventHash(prev string, id, at int64, typ, device, deployment, message, actor string) string {
	h := sha256.New()
	for _, f := range []string{"armada-event-v1", prev, strconv.FormatInt(id, 10), strconv.FormatInt(at, 10), typ, device, deployment, message, actor} {
		// Length-prefixed, so no two different events encode the same.
		fmt.Fprintf(h, "%d:%s\n", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type chainAnchor struct {
	afterID int64
	hash    string
}

func readAnchor(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (chainAnchor, error) {
	var a chainAnchor
	err := q.QueryRowContext(ctx, `SELECT after_id, hash FROM event_chain WHERE id = 1`).Scan(&a.afterID, &a.hash)
	if errors.Is(err, sql.ErrNoRows) {
		return chainAnchor{}, nil
	}
	return a, err
}

// RecordEvent appends an event to the activity log and sets its ID and hash.
func (s *Store) RecordEvent(ctx context.Context, e *api.Event) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Insert first: that takes the write lock, so the previous hash read
	// next can't change under us.
	at := ts(e.Time)
	res, err := tx.ExecContext(ctx, `INSERT INTO events (time, type, device_id, deployment_id, message, actor) VALUES (?,?,?,?,?,?)`,
		at, e.Type, e.DeviceID, e.DeploymentID, e.Message, e.Actor)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	anchor, err := readAnchor(ctx, tx)
	if err != nil {
		return err
	}
	prev := anchor.hash
	err = tx.QueryRowContext(ctx, `SELECT hash FROM events WHERE id < ? AND id > ? ORDER BY id DESC LIMIT 1`, id, anchor.afterID).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	hash := eventHash(prev, id, at, e.Type, e.DeviceID, e.DeploymentID, e.Message, e.Actor)
	if _, err := tx.ExecContext(ctx, `UPDATE events SET hash = ? WHERE id = ?`, hash, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	e.ID, e.Hash = id, hash
	return nil
}

// PruneEvents deletes events older than before. It removes the oldest
// events only up to the first one it keeps (a contiguous start of the
// chain), and moves the chain's start past them.
func (s *Store) PruneEvents(ctx context.Context, before time.Time) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The newest event to remove: the one before the first that stays.
	var cutoff sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(id) FROM events WHERE id < COALESCE((SELECT MIN(id) FROM events WHERE time >= ?), (SELECT MAX(id) FROM events) + 1)`,
		ts(before)).Scan(&cutoff); err != nil {
		return err
	}
	if !cutoff.Valid {
		return nil // nothing to remove
	}
	anchor, err := readAnchor(ctx, tx)
	if err != nil {
		return err
	}
	if cutoff.Int64 > anchor.afterID {
		var hash string
		if err := tx.QueryRowContext(ctx, `SELECT hash FROM events WHERE id = ?`, cutoff.Int64).Scan(&hash); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_chain (id, after_id, hash, reason, at) VALUES (1, ?, ?, 'pruned', ?)
			ON CONFLICT(id) DO UPDATE SET after_id = excluded.after_id, hash = excluded.hash, reason = excluded.reason, at = excluded.at`,
			cutoff.Int64, hash, ts(time.Now())); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id <= ?`, cutoff.Int64); err != nil {
		return err
	}
	return tx.Commit()
}

// VerifyEvents checks the activity log's hash chain from its start to its
// newest event.
func (s *Store) VerifyEvents(ctx context.Context) (*api.EventChainStatus, error) {
	st := &api.EventChainStatus{CheckedAt: time.Now().UTC(), Intact: true}
	anchor, err := readAnchor(ctx, s.db)
	if err != nil {
		return nil, err
	}
	st.StartsAfterID = anchor.afterID
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE id <= ?`, anchor.afterID).Scan(&st.Unchained); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, time, type, device_id, deployment_id, message, actor, hash FROM events WHERE id > ? ORDER BY id`, anchor.afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prev := anchor.hash
	for rows.Next() {
		var id, at int64
		var typ, dev, dep, msg, actor, hash string
		if err := rows.Scan(&id, &at, &typ, &dev, &dep, &msg, &actor, &hash); err != nil {
			return nil, err
		}
		if want := eventHash(prev, id, at, typ, dev, dep, msg, actor); hash != want {
			st.Intact = false
			st.BrokenAtID = id
			st.Problem = fmt.Sprintf("event %d doesn't match the chain: it, or an event just before it, was changed, removed or added outside Armada", id)
			return st, rows.Err()
		}
		prev = hash
		st.Checked++
		st.HeadID, st.HeadHash = id, hash
	}
	if st.Checked == 0 {
		st.HeadHash = anchor.hash
	}
	return st, rows.Err()
}
