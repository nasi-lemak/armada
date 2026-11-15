package server

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func tinyPNG(t *testing.T) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func putAttachment(t *testing.T, url, key string, body []byte) int {
	req, _ := http.NewRequest("PUT", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func getAttachment(t *testing.T, url, token string) (int, string, []byte) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), b
}

func TestScreenshots(t *testing.T) {
	enroll := func(ts string, call2 func(string, string, string, any, any) int, customer, os string) api.EnrollResponse {
		var tok api.EnrollmentToken
		call2(admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": customer}}, &tok)
		var er api.EnrollResponse
		call2("", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: customer + "-" + os, Info: api.DeviceInfo{OS: os}}, &er)
		return er
	}

	// Off by default.
	_, off := setup(t)
	c := func(tok, m, p string, in, out any) int { return call(t, off, tok, m, p, in, out) }
	d0 := enroll(off.URL, c, "acme", "linux")
	if code := c(admin, "POST", "/api/v1/devices/"+d0.DeviceID+"/actions", api.CreateActionRequest{Type: api.ActionScreenshot}, nil); code != 403 {
		t.Fatalf("screenshots off: %d", code)
	}

	s, ts := setupWith(t, func(c *Config) { c.Screenshots = true })
	clock := time.Now()
	s.now = func() time.Time { return clock }
	c = func(tok, m, p string, in, out any) int { return call(t, ts, tok, m, p, in, out) }
	var me api.Me
	c(admin, "GET", "/api/v1/me", nil, &me)
	if !me.Features.Screenshots {
		t.Fatal("feature not advertised")
	}
	dev := enroll(ts.URL, c, "acme", "linux")
	droid := enroll(ts.URL, c, "acme", "kiosk")
	// Android enrollment needs hardware attestation; report Android in a heartbeat instead.
	c(droid.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "android"}}, nil)
	if code := c(admin, "POST", "/api/v1/devices/"+droid.DeviceID+"/actions", api.CreateActionRequest{Type: api.ActionScreenshot}, nil); code != 400 {
		t.Fatalf("android: %d", code)
	}
	var a api.Action
	if code := c(admin, "POST", "/api/v1/devices/"+dev.DeviceID+"/actions", api.CreateActionRequest{Type: api.ActionScreenshot}, &a); code != 201 {
		t.Fatalf("queue: %d", code)
	}
	upURL := ts.URL + "/api/v1/agent/actions/" + a.ID + "/attachment"
	getURL := ts.URL + "/api/v1/actions/" + a.ID + "/attachment"
	if code, _, _ := getAttachment(t, getURL, admin); code != 404 {
		t.Fatalf("before upload: %d", code)
	}
	if code := putAttachment(t, upURL, dev.DeviceKey, []byte("not an image")); code != 400 {
		t.Fatalf("junk: %d", code)
	}
	if code := putAttachment(t, upURL, droid.DeviceKey, tinyPNG(t)); code != 404 {
		t.Fatalf("another device's action: %d", code)
	}
	if code := putAttachment(t, upURL, dev.DeviceKey, tinyPNG(t)); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	if code := putAttachment(t, upURL, dev.DeviceKey, tinyPNG(t)); code != 409 {
		t.Fatalf("second upload: %d", code)
	}
	if code, ct, body := getAttachment(t, getURL, admin); code != 200 || ct != "image/png" || !bytes.Equal(body, tinyPNG(t)) {
		t.Fatalf("view: %d %s", code, ct)
	}
	// The request is in the activity log, with who asked.
	var evs []api.Event
	c(admin, "GET", "/api/v1/events?type=action.queued&device_id="+dev.DeviceID, nil, &evs)
	if len(evs) != 1 || evs[0].Actor != "admin token" {
		t.Fatalf("audit: %+v", evs)
	}

	// Viewing needs the operator role on the device; other clients can't see it at all.
	for _, u := range []struct{ email, customer, role string }{{"view@acme.com", "acme", api.RoleViewer}, {"op@globex.com", "globex", api.RoleOperator}} {
		c(admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: u.email, Role: u.role, Scope: api.Selector{Labels: map[string]string{"customer": u.customer}}, Password: "long-password-1"}, nil)
	}
	login := func(email string) string {
		var lr api.LoginResponse
		c("", "POST", "/api/v1/auth/login", api.LoginRequest{Email: email, Password: "long-password-1"}, &lr)
		return lr.Token
	}
	if code, _, _ := getAttachment(t, getURL, login("view@acme.com")); code != 403 {
		t.Fatalf("viewer: %d", code)
	}
	if code, _, _ := getAttachment(t, getURL, login("op@globex.com")); code != 404 {
		t.Fatalf("other client: %d", code)
	}

	// Deleted after the retention period.
	path := filepath.Join(s.cfg.DataDir, "attachments", a.ID+".img")
	old := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(path, old, old)
	s.pruneAttachments()
	clock = clock.Add(8 * 24 * time.Hour)
	if code, _, _ := getAttachment(t, getURL, login("view@acme.com")); code != 403 {
		t.Fatalf("viewer after: %d", code)
	}
	if code, _, _ := getAttachment(t, getURL, admin); code != 410 {
		t.Fatalf("after retention: %d", code)
	}
}
