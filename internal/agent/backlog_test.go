package agent

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestBacklog(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "backlog.json")
	b := loadBacklog(path, log)
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	b.add(api.BacklogSample{At: t0})
	b.add(api.BacklogSample{At: t0.Add(time.Minute)}) // too soon: one reading per 5 minutes
	for i := 1; i <= 4; i++ {
		b.add(api.BacklogSample{At: t0.Add(time.Duration(i) * backlogEvery)})
	}
	if b.pending() != 5 {
		t.Fatalf("pending %d", b.pending())
	}
	// It survives a restart.
	b = loadBacklog(path, log)
	if b.pending() != 5 {
		t.Fatalf("after reload: %d", b.pending())
	}
	batch := b.batch(3)
	if len(batch) != 3 || !batch[0].At.Equal(t0) {
		t.Fatalf("batch: %+v", batch)
	}
	b.sent(len(batch))
	if got := b.batch(10); len(got) != 2 || !got[0].At.Equal(t0.Add(3*backlogEvery)) {
		t.Fatalf("rest: %+v", got)
	}
	b.sent(2)
	if loadBacklog(path, log).pending() != 0 {
		t.Fatal("sent readings came back after a reload")
	}
	// Capped at a week: the oldest go.
	for i := 0; i < backlogMax+10; i++ {
		b.add(api.BacklogSample{At: t0.Add(time.Duration(i) * backlogEvery)})
	}
	if got := b.batch(1); b.pending() != backlogMax || !got[0].At.Equal(t0.Add(10*backlogEvery)) {
		t.Fatalf("cap: %d, oldest %v", b.pending(), got[0].At)
	}
}
