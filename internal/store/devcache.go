package store

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/nasi-lemak/armada/internal/api"
)

// deviceCache keeps every device parsed in memory. Fleet-wide pages read
// the whole device list; decoding thousands of rows of JSON each time was
// most of their cost.
//
// Devices in the cache are never modified: a change installs a new copy.
// Check-ins and health reports (frequent) update their device's entry as
// part of the write; other changes (rare: renames, labels, lifecycle,
// enrollment, deletion) drop the cache, and the next read loads it again.
// Readers get deep copies, so they may change what they get.
//
// Every write to the devices table must go through a function that keeps
// this up to date; see the device functions in store.go.
type deviceCache struct {
	loadMu sync.Mutex // one load at a time

	mu     sync.Mutex
	byID   map[string]*api.Device // nil: not loaded
	sorted []*api.Device          // by name, then ID; nil: sort again
	pos    map[string]int         // index in sorted
	gen    uint64                 // bumped when the cache is dropped
	// While a load runs, the devices written meanwhile; the load reads
	// them again before installing what it read.
	loading bool
	touched map[string]bool
}

// invalidate drops the cache after a change it doesn't track in place.
func (c *deviceCache) invalidate() {
	c.mu.Lock()
	c.gen++
	c.byID, c.sorted, c.pos = nil, nil, nil
	c.mu.Unlock()
}

// sortLocked rebuilds the sorted list. c.mu is held.
func (c *deviceCache) sortLocked() {
	if c.sorted != nil {
		return
	}
	c.sorted = make([]*api.Device, 0, len(c.byID))
	for _, d := range c.byID {
		c.sorted = append(c.sorted, d)
	}
	sortDevices(c.sorted)
	c.pos = make(map[string]int, len(c.sorted))
	for i, d := range c.sorted {
		c.pos[d.ID] = i
	}
}

// sortDevices orders devices like the database's ORDER BY name, id.
func sortDevices(devs []*api.Device) {
	slices.SortFunc(devs, func(a, b *api.Device) int {
		if n := strings.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// writeDevice runs a single-device write and, if it succeeds, applies the
// same change to the cached copy. Holding the lock across both keeps the
// cache in the order the database saw the writes.
func (s *Store) writeDevice(id string, write func() error, apply func(d *api.Device)) error {
	c := &s.devs
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := write(); err != nil {
		return err
	}
	switch {
	case c.byID != nil:
		old := c.byID[id]
		if old == nil {
			return nil
		}
		nd := cloneDevice(old)
		apply(nd)
		c.byID[id] = nd
		if c.sorted != nil {
			c.sorted[c.pos[id]] = nd
		}
	case c.loading:
		c.touched[id] = true
	}
	return nil
}

// devicesSnapshot returns the cached devices in order. The devices are
// shared: callers must copy any they return.
func (s *Store) devicesSnapshot(ctx context.Context) ([]*api.Device, error) {
	c := &s.devs
	c.mu.Lock()
	if c.byID != nil {
		c.sortLocked()
		out := slices.Clone(c.sorted)
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	c.loadMu.Lock()
	defer c.loadMu.Unlock()
	c.mu.Lock()
	if c.byID != nil { // another reader loaded it
		c.sortLocked()
		out := slices.Clone(c.sorted)
		c.mu.Unlock()
		return out, nil
	}
	gen := c.gen
	c.loading, c.touched = true, map[string]bool{}
	c.mu.Unlock()
	done := func() { c.loading, c.touched = false, nil }

	all, err := s.listDevices(ctx, `SELECT `+deviceCols+` FROM devices ORDER BY name, id`)
	if err != nil {
		c.mu.Lock()
		done()
		c.mu.Unlock()
		return nil, err
	}
	byID := make(map[string]*api.Device, len(all))
	for _, d := range all {
		byID[d.ID] = d
	}
	for {
		c.mu.Lock()
		if c.gen != gen {
			// Dropped while loading: what we read may be stale for the
			// change that dropped it. Answer this read from it, but
			// don't keep it.
			done()
			c.mu.Unlock()
			sortDevices(all)
			return all, nil
		}
		if len(c.touched) == 0 {
			c.byID = byID
			c.sorted, c.pos = nil, nil
			done()
			c.sortLocked()
			out := slices.Clone(c.sorted)
			c.mu.Unlock()
			return out, nil
		}
		ids := slices.Collect(maps.Keys(c.touched))
		clear(c.touched)
		c.mu.Unlock()
		// Written while we were reading: read them again.
		for _, id := range ids {
			d, err := s.GetDevice(ctx, id)
			switch {
			case errors.Is(err, ErrNotFound):
				delete(byID, id)
			case err != nil:
				c.mu.Lock()
				done()
				c.mu.Unlock()
				return nil, err
			default:
				byID[id] = d
			}
		}
		all = all[:0]
		for _, d := range byID {
			all = append(all, d)
		}
	}
}

// listCached returns copies of the cached devices that keep reports true.
func (s *Store) listCached(ctx context.Context, keep func(d *api.Device) bool) ([]*api.Device, error) {
	all, err := s.devicesSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	var out []*api.Device
	for _, d := range all {
		if keep(d) {
			out = append(out, cloneDevice(d))
		}
	}
	return out, nil
}

// applyHeartbeat sets what a check-in stores, decoded from the same JSON
// the database got, exactly as scanDevice would read it back.
func applyHeartbeat(d *api.Device, info, metrics, pkgs, status string, seen int64) {
	d.Info, d.Metrics, d.Packages = api.DeviceInfo{}, api.Metrics{}, nil
	_ = json.Unmarshal([]byte(info), &d.Info)
	_ = json.Unmarshal([]byte(metrics), &d.Metrics)
	_ = json.Unmarshal([]byte(pkgs), &d.Packages)
	if d.Packages == nil {
		d.Packages = map[string]string{}
	}
	d.AgentStatus = status
	d.LastSeenAt = fromTS(seen)
}

func applyHealth(d *api.Device, health string) {
	d.Checks = nil
	_ = json.Unmarshal([]byte(health), &d.Checks)
	d.Health = api.WorstHealth(d.Checks)
}

// cloneDevice copies a device and everything it refers to.
// TestCloneDeviceIsDeep fails if a field is added that this misses.
func cloneDevice(d *api.Device) *api.Device {
	c := *d
	c.Labels = maps.Clone(d.Labels)
	c.Packages = maps.Clone(d.Packages)
	c.Info.IPs = slices.Clone(d.Info.IPs)
	if d.Checks != nil {
		c.Checks = make([]api.CheckResult, len(d.Checks))
		for i, ck := range d.Checks {
			ck.Details = maps.Clone(ck.Details)
			if ck.Value != nil {
				v := *ck.Value
				ck.Value = &v
			}
			c.Checks[i] = ck
		}
	}
	if d.Attestation != nil {
		a := *d.Attestation
		c.Attestation = &a
	}
	if d.Maintenance != nil {
		m := *d.Maintenance
		if m.Until != nil {
			u := *m.Until
			m.Until = &u
		}
		c.Maintenance = &m
	}
	if d.Inventory != nil {
		inv := *d.Inventory
		c.Inventory = &inv
	}
	if d.LifecycleInfo != nil {
		li := *d.LifecycleInfo
		c.LifecycleInfo = &li
	}
	return &c
}
