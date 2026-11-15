package agent

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// While the agent can't reach the server it keeps one reading every
// backlogEvery (check results and metrics), up to backlogMax of them (a
// week), saved to disk so a restart doesn't lose them. Once the server is
// back they go up a batch per heartbeat, so the device's charts have no
// hole where the outage was.
const (
	backlogEvery = 5 * time.Minute
	backlogMax   = 7 * 24 * 12
)

type backlog struct {
	mu      sync.Mutex
	path    string
	log     *slog.Logger
	samples []api.BacklogSample
}

func loadBacklog(path string, log *slog.Logger) *backlog {
	b := &backlog{path: path, log: log}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &b.samples); err != nil {
			log.Warn("ignoring unreadable backlog", "file", path, "err", err)
			b.samples = nil
		}
	}
	return b
}

// add keeps a reading if the last one is old enough.
func (b *backlog) add(s api.BacklogSample) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n := len(b.samples); n > 0 && s.At.Sub(b.samples[n-1].At) < backlogEvery {
		return
	}
	b.samples = append(b.samples, s)
	if len(b.samples) > backlogMax {
		b.samples = append([]api.BacklogSample(nil), b.samples[len(b.samples)-backlogMax:]...)
	}
	b.saveLocked()
}

// batch returns up to n of the oldest readings, to send.
func (b *backlog) batch(n int) []api.BacklogSample {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]api.BacklogSample(nil), b.samples[:min(n, len(b.samples))]...)
}

// sent drops the n oldest readings once the server has them.
func (b *backlog) sent(n int) {
	if b == nil || n == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.samples = b.samples[min(n, len(b.samples)):]
	b.saveLocked()
}

func (b *backlog) pending() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.samples)
}

func (b *backlog) saveLocked() {
	if b.path == "" {
		return
	}
	if len(b.samples) == 0 {
		os.Remove(b.path)
		return
	}
	data, err := json.Marshal(b.samples)
	if err == nil {
		tmp := b.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, b.path)
		}
	}
	if err != nil {
		b.log.Warn("could not save backlog", "err", err)
	}
}
