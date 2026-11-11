package attestation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// GoogleStatusURL is Google's attestation certificate revocation list.
const GoogleStatusURL = "https://android.googleapis.com/attestation/status"

// RevocationList caches Google's attestation revocation list. Keys are
// certificate serial numbers in lowercase hex.
type RevocationList struct {
	mu        sync.RWMutex
	entries   map[string]string
	fetchedAt time.Time
}

type statusEntry struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// Load replaces the list from a status JSON document.
func (r *RevocationList) Load(rd io.Reader) error {
	var doc struct {
		Entries map[string]statusEntry `json:"entries"`
	}
	if err := json.NewDecoder(io.LimitReader(rd, 64<<20)).Decode(&doc); err != nil {
		return fmt.Errorf("parse revocation list: %w", err)
	}
	m := make(map[string]string, len(doc.Entries))
	for serial, e := range doc.Entries {
		reason := e.Reason
		if reason == "" {
			reason = e.Status
		}
		m[strings.ToLower(strings.TrimLeft(serial, "0"))] = reason
	}
	r.mu.Lock()
	r.entries, r.fetchedAt = m, time.Now()
	r.mu.Unlock()
	return nil
}

// Fetch downloads the list from url.
func (r *RevocationList) Fetch(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revocation list: HTTP %d", resp.StatusCode)
	}
	return r.Load(resp.Body)
}

// IsRevoked implements the Verifier's revocation callback.
func (r *RevocationList) IsRevoked(serialHex string) (bool, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reason, ok := r.entries[strings.TrimLeft(strings.ToLower(serialHex), "0")]
	return ok, reason
}

// Len reports how many revoked serials are loaded.
func (r *RevocationList) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
