package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
)

// Sites: several Armada servers watched from one central server.
//
// On the central server an admin adds a site and gets a token. The site
// server, started with -central-url and -central-token, pushes a snapshot
// (its overview, what needs attention, firing alerts) every few minutes,
// outbound only, so it works from behind NAT or a proxy and simply tries
// again later when the central server can't be reached. A site with no
// route out exports a snapshot file instead, which is imported by hand.

const (
	maxSnapshotBytes   = 4 << 20
	defaultCentralPush = 5 * time.Minute
	siteStaleAfter     = 3 // intervals without a report
)

// CentralConfig makes this server report to a central one.
type CentralConfig struct {
	URL   string // the central server, e.g. https://fleet.example.com
	Token string // this site's token there
	Every time.Duration
}

func (c CentralConfig) every() time.Duration {
	if c.Every <= 0 {
		return defaultCentralPush
	}
	return c.Every
}

// centralState is the push status, kept in memory.
type centralState struct {
	mu       sync.Mutex
	lastPush time.Time
	lastOK   time.Time
	lastErr  string
	running  bool
}

func (s *Server) keyFingerprint() string {
	sum := sha256.Sum256(s.pubKey)
	return hex.EncodeToString(sum[:8])
}

// snapshot is this server's report to a central one.
func (s *Server) snapshot(ctx context.Context, publicURL string) (*api.SiteSnapshot, error) {
	ctx = withPrincipal(ctx, &principal{name: "site report", role: api.RoleAdmin, grants: []api.Grant{{Role: api.RoleAdmin}}})
	p := principalFrom(ctx)
	sum, err := s.fleetSummary(ctx, p)
	if err != nil {
		return nil, err
	}
	in, err := s.computeInsights(ctx, p, 7*24*time.Hour)
	if err != nil {
		return nil, err
	}
	snap := &api.SiteSnapshot{GeneratedAt: s.now().UTC(), ServerVersion: s.cfg.Version, PublicURL: publicURL,
		KeyFingerprint: s.keyFingerprint(), PublicKey: base64.StdEncoding.EncodeToString(s.pubKey),
		Summary: *sum, Insights: *in, EverySeconds: int(s.cfg.Central.every().Seconds())}
	// Groupings (customers, projects) with their device counts.
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range s.cfg.GroupLabels {
		counts := map[string]int{}
		for _, d := range devs {
			if v := d.Labels[k]; v != "" && d.ParentID == "" {
				counts[v]++
			}
		}
		g := api.Grouping{Key: k, Values: []api.GroupingValue{}}
		for v, n := range counts {
			g.Values = append(g.Values, api.GroupingValue{Value: v, Devices: n})
		}
		sort.Slice(g.Values, func(i, j int) bool { return g.Values[i].Value < g.Values[j].Value })
		snap.Groupings = append(snap.Groupings, g)
	}
	firing, err := s.store.ListAlerts(ctx, true, 50)
	if err != nil {
		return nil, err
	}
	for _, a := range firing {
		snap.FiringAlerts = append(snap.FiringAlerts, api.SiteAlert{Rule: a.RuleName, Severity: a.Severity, Summary: a.Summary,
			DeviceName: a.DeviceName, StartedAt: a.StartedAt})
	}
	return snap, nil
}

// signedSnapshot is the snapshot signed with this server's key.
func (s *Server) signedSnapshot(ctx context.Context, publicURL string) (*api.SignedSnapshot, error) {
	snap, err := s.snapshot(ctx, publicURL)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(s.signKey, append([]byte(api.SnapshotSigningPrefix), raw...))
	return &api.SignedSnapshot{Snapshot: raw, Signature: base64.StdEncoding.EncodeToString(sig)}, nil
}

// verifySnapshot checks a signed snapshot against the key it carries
// (which the central server then pins to the site).
func verifySnapshot(signed *api.SignedSnapshot) (*api.SiteSnapshot, error) {
	var snap api.SiteSnapshot
	if len(signed.Snapshot) == 0 || json.Unmarshal(signed.Snapshot, &snap) != nil {
		return nil, errors.New("this isn't an Armada site snapshot")
	}
	pub, err := base64.StdEncoding.DecodeString(snap.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("the snapshot carries no valid public key (is the site server up to date?)")
	}
	sum := sha256.Sum256(pub)
	if hex.EncodeToString(sum[:8]) != snap.KeyFingerprint {
		return nil, errors.New("the snapshot's key fingerprint doesn't match its key")
	}
	sig, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(pub), append([]byte(api.SnapshotSigningPrefix), signed.Snapshot...), sig) {
		return nil, errors.New("the snapshot's signature doesn't verify")
	}
	return &snap, nil
}

// ---- site side ----

// centralTick pushes a snapshot to the central server when one is due.
func (s *Server) centralTick(ctx context.Context, now time.Time) {
	c := s.cfg.Central
	if c.URL == "" || c.Token == "" {
		return
	}
	st := &s.central
	st.mu.Lock()
	due := !st.running && (st.lastPush.IsZero() || now.Sub(st.lastPush) >= c.every())
	if due {
		st.running, st.lastPush = true, now
	}
	st.mu.Unlock()
	if !due {
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		err := s.pushSnapshot(context.WithoutCancel(ctx))
		st.mu.Lock()
		st.running = false
		if err != nil {
			st.lastErr = err.Error()
		} else {
			st.lastErr, st.lastOK = "", s.now()
		}
		st.mu.Unlock()
		if err != nil {
			s.log.Warn("report to central server failed; will retry", "central", c.URL, "err", err)
		}
	}()
}

func (s *Server) pushSnapshot(ctx context.Context) error {
	c := s.cfg.Central
	signed, err := s.signedSnapshot(ctx, strings.TrimRight(s.cfg.Android.PublicURL, "/"))
	if err != nil {
		return err
	}
	body, err := json.Marshal(signed)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/api/v1/sites/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "armada-server/"+s.cfg.Version)
	res, err := s.notifyClient.Do(req) // honours HTTPS_PROXY
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var er api.ErrorResponse
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		if json.Unmarshal(b, &er) == nil && er.Error != "" {
			return fmt.Errorf("%s (HTTP %d)", er.Error, res.StatusCode)
		}
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return nil
}

func (s *Server) handleCentralStatus(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Central
	if c.URL == "" {
		writeJSON(w, http.StatusOK, api.CentralStatus{})
		return
	}
	st := &s.central
	st.mu.Lock()
	out := api.CentralStatus{URL: c.URL, LastError: st.lastErr}
	if !st.lastPush.IsZero() {
		t := st.lastPush
		out.LastPushAt = &t
	}
	if !st.lastOK.IsZero() {
		t := st.lastOK
		out.LastOKAt = &t
	}
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// handleSiteSnapshot returns this server's snapshot, to carry to a
// central server by hand (armadactl site export).
func (s *Server) handleSiteSnapshot(w http.ResponseWriter, r *http.Request) {
	signed, err := s.signedSnapshot(r.Context(), s.publicURL(r))
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, signed)
}

// ---- central side ----

func siteState(site *api.Site, now time.Time) string {
	if site.LastReportAt == nil {
		return "never"
	}
	every := defaultCentralPush
	if site.Snapshot != nil && site.Snapshot.EverySeconds > 0 {
		every = time.Duration(site.Snapshot.EverySeconds) * time.Second
	}
	if site.Source == "file" {
		return "file" // imported by hand: no schedule to be late for
	}
	if now.Sub(*site.LastReportAt) > siteStaleAfter*every {
		return "stale"
	}
	return "fresh"
}

func redactSite(site *api.Site, now time.Time) *api.Site {
	c := *site
	c.TokenHash, c.Token = "", ""
	c.State = siteState(site, now)
	return &c
}

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListSites(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	now := s.now()
	out := []*api.Site{}
	for _, site := range all {
		out = append(out, redactSite(site, now))
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateSite(w http.ResponseWriter, r *http.Request) {
	var req api.Site
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 || len(req.Note) > 1000 {
		writeError(w, http.StatusBadRequest, "give the site a name (up to 100 characters)")
		return
	}
	token := newSecret("site")
	site := &api.Site{ID: newID("site"), Name: req.Name, Note: strings.TrimSpace(req.Note), CreatedAt: s.now(),
		CreatedBy: principalFrom(r.Context()).name, TokenHash: hashSecret(token)}
	if err := s.store.PutSite(r.Context(), site); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "site.created", "", "", fmt.Sprintf("site %q added", site.Name))
	out := redactSite(site, s.now())
	out.Token = token // shown once
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) loadSite(w http.ResponseWriter, r *http.Request) (*api.Site, bool) {
	site, err := s.store.GetSite(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "site not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	return site, true
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteSite(r.Context(), site.ID); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "site.deleted", "", "", fmt.Sprintf("site %q removed", site.Name))
	w.WriteHeader(http.StatusNoContent)
}

// handleRotateSiteToken issues a new token; the old one stops working, and
// the next server to report with the new one may be a different machine
// (a rebuilt site server).
func (s *Server) handleRotateSiteToken(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	token := newSecret("site")
	if err := s.store.UpdateSite(r.Context(), site.ID, "", func(x *api.Site) error {
		x.TokenHash, x.KeyFingerprint = hashSecret(token), ""
		*site = *x
		return nil
	}); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "site.token", "", "", fmt.Sprintf("new token for site %q", site.Name))
	out := redactSite(site, s.now())
	out.Token = token
	writeJSON(w, http.StatusOK, out)
}

// siteError carries an HTTP status out of an UpdateSite callback.
type siteError struct {
	code int
	err  error
}

func (e *siteError) Error() string { return e.err.Error() }

// acceptSnapshot checks a verified snapshot and stores it for a site, in
// one transaction. With tokenHash set (a push), the site must still have
// that token: a report racing a token rotation or a deletion loses.
func (s *Server) acceptSnapshot(ctx context.Context, siteID, tokenHash string, snap *api.SiteSnapshot, source string) (*api.Site, int, error) {
	now := s.now()
	if snap.GeneratedAt.IsZero() {
		return nil, http.StatusBadRequest, errors.New("this isn't an Armada site snapshot")
	}
	if snap.GeneratedAt.After(now.Add(time.Hour)) {
		return nil, http.StatusBadRequest, errors.New("the snapshot is dated in the future: check the site server's clock")
	}
	if u, err := url.Parse(snap.PublicURL); snap.PublicURL != "" && (err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "") {
		snap.PublicURL = "" // only http(s) links to its dashboard
	}
	var out api.Site
	first := false
	err := s.store.UpdateSite(ctx, siteID, tokenHash, func(site *api.Site) error {
		if site.KeyFingerprint != "" && site.KeyFingerprint != snap.KeyFingerprint {
			return &siteError{http.StatusConflict, fmt.Errorf("this site is registered to the server with key %s; if the site server was rebuilt, make a new token for it", site.KeyFingerprint)}
		}
		if site.Snapshot != nil && snap.GeneratedAt.Before(site.Snapshot.GeneratedAt) && source == "file" {
			return &siteError{http.StatusConflict, errors.New("this snapshot is older than the one already here")}
		}
		first = site.LastReportAt == nil
		site.KeyFingerprint, site.Snapshot, site.Source = snap.KeyFingerprint, snap, source
		site.LastReportAt = &now
		out = *site
		return nil
	})
	var se *siteError
	switch {
	case errors.As(err, &se):
		return nil, se.code, se.err
	case errors.Is(err, store.ErrNotFound):
		if tokenHash != "" {
			return nil, http.StatusUnauthorized, errors.New("unknown site token")
		}
		return nil, http.StatusNotFound, errors.New("site not found")
	case err != nil:
		return nil, http.StatusInternalServerError, err
	}
	if first {
		s.logEvent(ctx, "site.connected", "", "", fmt.Sprintf("site %q reported for the first time (server key %s)", out.Name, snap.KeyFingerprint))
	}
	return &out, 0, nil
}

// handleSiteReport takes a pushed, signed snapshot, authenticated by a site token.
func (s *Server) handleSiteReport(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(tok, "site_") {
		writeError(w, http.StatusUnauthorized, "a site token is required")
		return
	}
	h := hashSecret(tok)
	id, err := s.store.SiteIDByTokenHash(r.Context(), h)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "unknown site token")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	var signed api.SignedSnapshot
	if !readJSONLimit(w, r, &signed, maxSnapshotBytes) {
		return
	}
	snap, err := verifySnapshot(&signed)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, code, err := s.acceptSnapshot(r.Context(), id, h, snap, "push"); err != nil {
		if code == http.StatusInternalServerError {
			s.internalError(w, err)
		} else {
			writeError(w, code, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleImportSnapshot stores a snapshot file from a site with no route
// here (armadactl site export there). Its signature is checked as for a push.
func (s *Server) handleImportSnapshot(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	var signed api.SignedSnapshot
	if !readJSONLimit(w, r, &signed, maxSnapshotBytes) {
		return
	}
	snap, err := verifySnapshot(&signed)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, code, err := s.acceptSnapshot(r.Context(), site.ID, "", snap, "file")
	if err != nil {
		if code == http.StatusInternalServerError {
			s.internalError(w, err)
		} else {
			writeError(w, code, err.Error())
		}
		return
	}
	s.logEvent(r.Context(), "site.imported", "", "", fmt.Sprintf("snapshot of site %q from %s imported", site.Name, snap.GeneratedAt.UTC().Format(time.RFC3339)))
	writeJSON(w, http.StatusOK, redactSite(out, s.now()))
}

// siteSilentCandidates: sites that stopped reporting (alert condition site_silent).
func (s *Server) siteSilentCandidates(ctx context.Context, r *api.AlertRule, now time.Time, want map[string]candidate) error {
	limit := time.Duration(r.OfflineMinutes) * time.Minute
	if limit <= 0 {
		limit = 30 * time.Minute
	}
	sites, err := s.store.ListSites(ctx)
	if err != nil {
		return err
	}
	for _, site := range sites {
		if site.Source == "file" {
			continue // imported by hand: silence is expected
		}
		since := site.CreatedAt
		if site.LastReportAt != nil {
			since = *site.LastReportAt
		}
		if gone := now.Sub(since); gone >= limit {
			k := r.ID + "/site=" + site.ID
			what := "hasn't reported for " + roundDuration(gone)
			if site.LastReportAt == nil {
				what = "hasn't reported since it was added " + roundDuration(gone) + " ago"
			}
			want[k] = candidate{rule: r, key: k, severity: api.HealthCritical,
				summary: fmt.Sprintf("Site %s %s: its server may be down or cut off", site.Name, what)}
		}
	}
	return nil
}
