package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestSites(t *testing.T) {
	central, cts := setup(t)
	clock := time.Now().UTC()
	central.now = func() time.Time { return clock }
	var site api.Site
	if code := call(t, cts, admin, "POST", "/api/v1/sites", api.Site{Name: "Orchard store (on-prem)"}, &site); code != 201 || !strings.HasPrefix(site.Token, "site_") || site.TokenHash != "" {
		t.Fatalf("create site: %d %+v", code, site)
	}
	// The site server reports to the central one.
	ss, sts := setupWith(t, func(c *Config) { c.Central = CentralConfig{URL: cts.URL, Token: site.Token, Every: time.Minute} })
	var tok api.EnrollmentToken
	call(t, sts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": "acme"}}, &tok)
	var er api.EnrollResponse
	call(t, sts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: "till-1", Info: api.DeviceInfo{OS: "linux"}}, &er)
	ss.centralTick(t.Context(), ss.now())
	ss.bg.Wait()
	var st api.CentralStatus
	call(t, sts, admin, "GET", "/api/v1/site/central", nil, &st)
	if st.LastOKAt == nil || st.LastError != "" {
		t.Fatalf("push status: %+v", st)
	}
	var sites []api.Site
	call(t, cts, admin, "GET", "/api/v1/sites", nil, &sites)
	if len(sites) != 1 || sites[0].State != "fresh" || sites[0].Snapshot == nil || sites[0].Snapshot.Summary.Devices != 1 ||
		sites[0].Snapshot.KeyFingerprint != ss.keyFingerprint() || sites[0].TokenHash != "" || sites[0].Source != "push" {
		t.Fatalf("sites: %+v", sites)
	}
	if g := sites[0].Snapshot.Groupings; len(g) == 0 || g[0].Key != "customer" || g[0].Values[0].Value != "acme" {
		t.Fatalf("groupings: %+v", g)
	}

	// Another server can't report with that token.
	other, _ := setupWith(t, func(c *Config) { c.Central = CentralConfig{URL: cts.URL, Token: site.Token} })
	if err := other.pushSnapshot(t.Context()); err == nil || !strings.Contains(err.Error(), "registered to the server with key") {
		t.Fatalf("impostor: %v", err)
	}
	// Nor with a made-up token.
	req, _ := http.NewRequest("POST", cts.URL+"/api/v1/sites/report", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer site_nope")
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 401 {
		t.Fatalf("bad token: %d", res.StatusCode)
	}

	// An air-gapped site: export a snapshot file, import it on the central server.
	var snap api.SiteSnapshot
	call(t, sts, admin, "GET", "/api/v1/site/snapshot", nil, &snap)
	var gap api.Site
	call(t, cts, admin, "POST", "/api/v1/sites", api.Site{Name: "Warehouse (air-gapped)"}, &gap)
	body, _ := json.Marshal(snap)
	req, _ = http.NewRequest("POST", cts.URL+"/api/v1/sites/"+gap.ID+"/import", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+admin)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 200 {
		t.Fatalf("import: %d", res.StatusCode)
	}

	// A site that stops reporting raises site_silent; one imported by hand doesn't.
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer hook.Close()
	var ch api.NotificationChannel
	call(t, cts, admin, "POST", "/api/v1/channels", api.NotificationChannel{Name: "ops", Type: "webhook", Webhook: &api.WebhookConfig{URL: hook.URL}}, &ch)
	if code := call(t, cts, admin, "POST", "/api/v1/alert-rules", api.AlertRule{Name: "Site silent", Enabled: true, Condition: api.ConditionSiteSilent,
		OfflineMinutes: 15, Channels: []string{ch.ID}}, nil); code != 201 {
		t.Fatalf("rule: %d", code)
	}
	clock = clock.Add(20 * time.Minute)
	if err := central.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	var alerts []api.Alert
	call(t, cts, admin, "GET", "/api/v1/alerts?status=firing", nil, &alerts)
	if len(alerts) != 1 || !strings.Contains(alerts[0].Summary, "Orchard store") {
		t.Fatalf("site_silent: %+v", alerts)
	}
	sites = nil
	call(t, cts, admin, "GET", "/api/v1/sites", nil, &sites)
	states := map[string]string{}
	for _, x := range sites {
		states[x.Name] = x.State
	}
	if states["Orchard store (on-prem)"] != "stale" || states["Warehouse (air-gapped)"] != "file" {
		t.Fatalf("states: %v", states)
	}

	// A new token: the old one stops working; the site server reports again with the new one.
	var rot api.Site
	call(t, cts, admin, "POST", "/api/v1/sites/"+site.ID+"/token", nil, &rot)
	if err := ss.pushSnapshot(t.Context()); err == nil {
		t.Fatal("old token still accepted")
	}
	ss.cfg.Central.Token = rot.Token
	if err := ss.pushSnapshot(t.Context()); err != nil {
		t.Fatalf("new token: %v", err)
	}
}
