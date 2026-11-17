package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func record(t *testing.T, s *Store, n int, at time.Time) []int64 {
	t.Helper()
	var ids []int64
	for i := 0; i < n; i++ {
		e := &api.Event{Time: at.Add(time.Duration(i) * time.Second), Type: "device.updated", DeviceID: fmt.Sprint("dev-", i), Message: fmt.Sprint("event ", i), Actor: "ops@example.com"}
		if err := s.RecordEvent(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		if e.Hash == "" {
			t.Fatal("no hash")
		}
		ids = append(ids, e.ID)
	}
	return ids
}

func verify(t *testing.T, s *Store) *api.EventChainStatus {
	t.Helper()
	st, err := s.VerifyEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestEventChain(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		tamper     string // SQL with %d for the target event's ID
		brokenAt   int    // index of the first event reported broken
		undetected bool
	}{
		"edited message":        {tamper: `UPDATE events SET message = 'nothing happened' WHERE id = %d`, brokenAt: 5},
		"edited actor":          {tamper: `UPDATE events SET actor = 'someone else' WHERE id = %d`, brokenAt: 5},
		"edited time":           {tamper: `UPDATE events SET time = time + 1 WHERE id = %d`, brokenAt: 5},
		"deleted":               {tamper: `DELETE FROM events WHERE id = %d`, brokenAt: 6},
		"inserted":              {tamper: `INSERT INTO events (time, type, message, actor) VALUES (0, 'x', 'forged %d', '')`, brokenAt: 10},
		"hash recomputed alone": {tamper: `UPDATE events SET message = 'x', hash = hash WHERE id = %d`, brokenAt: 5},
		"removed from the end":  {tamper: `DELETE FROM events WHERE id >= %d + 4`, undetected: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := openTest(t)
			ids := record(t, s, 10, time.Now())
			before := verify(t, s)
			if !before.Intact || before.Checked != 10 || before.HeadID != ids[9] {
				t.Fatalf("fresh chain: %+v", before)
			}
			if _, err := s.DB().ExecContext(ctx, fmt.Sprintf(tc.tamper, ids[5])); err != nil {
				t.Fatal(err)
			}
			st := verify(t, s)
			if tc.undetected {
				// The chain is intact; only a head kept elsewhere shows it.
				if !st.Intact || st.HeadHash == before.HeadHash {
					t.Fatalf("%+v", st)
				}
				return
			}
			if st.Intact {
				t.Fatalf("tampering not detected: %+v", st)
			}
			want := int64(0)
			if tc.brokenAt < len(ids) {
				want = ids[tc.brokenAt]
			}
			if want != 0 && st.BrokenAtID != want {
				t.Errorf("broken at %d, want %d (%s)", st.BrokenAtID, want, st.Problem)
			}
			if !strings.Contains(st.Problem, "outside Armada") {
				t.Errorf("problem: %q", st.Problem)
			}
		})
	}
}

func TestEventChainPruning(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	ids := record(t, s, 5, old)
	record(t, s, 5, time.Now())
	if err := s.PruneEvents(ctx, time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st := verify(t, s)
	if !st.Intact || st.Checked != 5 || st.StartsAfterID != ids[4] {
		t.Fatalf("after pruning: %+v", st)
	}
	// New events keep chaining from the pruned start.
	record(t, s, 2, time.Now())
	if st := verify(t, s); !st.Intact || st.Checked != 7 {
		t.Fatalf("after more events: %+v", st)
	}
	// Pruning everything keeps the head.
	head := verify(t, s).HeadHash
	if err := s.PruneEvents(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := verify(t, s); !st.Intact || st.Checked != 0 || st.HeadHash != head {
		t.Fatalf("after pruning everything: %+v (head was %s)", st, head)
	}
	if st := record(t, s, 1, time.Now()); len(st) != 1 {
		t.Fatal()
	}
	if st := verify(t, s); !st.Intact || st.Checked != 1 {
		t.Fatalf("after a new event: %+v", st)
	}
}

// Events recorded by an older version (no hash) come before the chain.
func TestEventChainAfterUpgrade(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.DB().ExecContext(ctx, `INSERT INTO events (time, type, message, actor) VALUES (?, 'old', 'from before', '')`, ts(time.Now()))
	}
	// As migration 18 does on an existing database.
	s.DB().ExecContext(ctx, `UPDATE event_chain SET after_id = (SELECT MAX(id) FROM events)`)
	record(t, s, 4, time.Now())
	st := verify(t, s)
	if !st.Intact || st.Unchained != 3 || st.Checked != 4 {
		t.Fatalf("%+v", st)
	}
}

func TestEventChainConcurrent(t *testing.T) {
	s := openTest(t)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := s.RecordEvent(context.Background(), &api.Event{Type: "t", Message: fmt.Sprint(w, i)}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if st := verify(t, s); !st.Intact || st.Checked != 200 {
		t.Fatalf("%+v", st)
	}
}
