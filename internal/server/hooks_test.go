package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

type hookReceiver struct {
	mu     sync.Mutex
	got    []api.EventHookPayload
	sigOK  []bool
	status int
	ts     *httptest.Server
}

func newHookReceiver(t *testing.T, secret string, tlsOn bool) *hookReceiver {
	rv := &hookReceiver{status: 200}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(r.Header.Get("X-Armada-Timestamp") + "."))
		mac.Write(body)
		ok := r.Header.Get("X-Armada-Signature") == "sha256="+hex.EncodeToString(mac.Sum(nil))
		var p api.EventHookPayload
		json.Unmarshal(body, &p)
		rv.mu.Lock()
		defer rv.mu.Unlock()
		if r.Header.Get("X-Armada-Event") != p.Type {
			ok = false
		}
		rv.got, rv.sigOK = append(rv.got, p), append(rv.sigOK, ok)
		w.WriteHeader(rv.status)
	})
	if tlsOn {
		rv.ts = httptest.NewTLSServer(h)
	} else {
		rv.ts = httptest.NewServer(h)
	}
	t.Cleanup(rv.ts.Close)
	return rv
}

func (rv *hookReceiver) types() []string {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	var out []string
	for _, p := range rv.got {
		out = append(out, p.Type)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEventHooks(t *testing.T) {
	s, ts := setup(t)
	s.allowPrivateClientTargets = true
	s.hooks.retryDelay = func(int) time.Duration { return 5 * time.Millisecond }
	vendor := newHookReceiver(t, "topsecret", false)
	var created api.EventHook
	if code := call(t, ts, admin, "POST", "/api/v1/event-hooks", api.EventHook{Name: "SIEM", URL: vendor.ts.URL, Types: []string{"device.*"},
		Secret: "topsecret", Enabled: true}, &created); code != 201 || created.Secret != "topsecret" || !created.HasSecret {
		t.Fatalf("create: %d %+v", code, created)
	}
	enroll := func(name, customer string) string {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": customer}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux"}}, &er)
		return er.DeviceID
	}
	a1 := enroll("acme-1", "acme")
	waitFor(t, "device.enrolled", func() bool { return len(vendor.types()) == 1 })
	vendor.mu.Lock()
	if p := vendor.got[0]; p.Type != "device.enrolled" || p.Device == nil || p.Device.ID != a1 || p.Device.Labels["customer"] != "acme" ||
		p.ID == 0 || p.Hook != "SIEM" || !vendor.sigOK[0] {
		t.Fatalf("payload: %+v sig %v", p, vendor.sigOK[0])
	}
	vendor.mu.Unlock()
	// Not a device.* event: not sent.
	call(t, ts, admin, "POST", "/api/v1/devices/"+a1+"/actions", api.CreateActionRequest{Type: "reboot"}, nil)

	// A client's webhook: https only, its own devices' events only.
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleOperator,
		Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-password-1"}, &lr)
	if code := call(t, ts, lr.Token, "POST", "/api/v1/event-hooks", api.EventHook{Name: "acme", URL: vendor.ts.URL, Enabled: true}, nil); code != 400 {
		t.Fatalf("client http hook: %d", code)
	}
	client := newHookReceiver(t, "", true)
	s.clientNotifyClient.Transport.(*http.Transport).TLSClientConfig = client.ts.Client().Transport.(*http.Transport).TLSClientConfig
	var ch api.EventHook
	if code := call(t, ts, lr.Token, "POST", "/api/v1/event-hooks", api.EventHook{Name: "acme", URL: client.ts.URL, Enabled: true}, &ch); code != 201 ||
		ch.ClientScope.Labels["customer"] != "acme" || !strings.HasPrefix(ch.Secret, "whsec_") {
		t.Fatalf("client hook: %d %+v", code, ch)
	}
	client.mu.Lock()
	client.sigOK = nil
	client.mu.Unlock()
	enroll("globex-1", "globex")
	call(t, ts, admin, "POST", "/api/v1/devices/"+a1+"/actions", api.CreateActionRequest{Type: "restart-agent"}, nil)
	waitFor(t, "client action event", func() bool { return len(client.types()) >= 1 })
	time.Sleep(50 * time.Millisecond)
	for _, p := range client.got {
		if p.Device == nil || p.Device.ID != a1 || p.DeploymentID != "" {
			t.Fatalf("client got: %+v", p)
		}
	}
	waitFor(t, "vendor globex enrollment", func() bool { return len(vendor.types()) == 2 })
	if got := vendor.types(); got[1] != "device.enrolled" {
		t.Fatalf("vendor got %v", got)
	}

	// Secrets never come back; clients don't see the vendor's webhooks.
	var list []api.EventHook
	call(t, ts, admin, "GET", "/api/v1/event-hooks", nil, &list)
	if len(list) != 2 || list[0].Secret != "" || !list[0].HasSecret {
		t.Fatalf("list: %+v", list)
	}
	call(t, ts, lr.Token, "GET", "/api/v1/event-hooks", nil, &list)
	if len(list) != 1 || list[0].ID != ch.ID {
		t.Fatalf("client list: %+v", list)
	}
	if code := call(t, ts, lr.Token, "DELETE", "/api/v1/event-hooks/"+created.ID, nil, nil); code != 404 {
		t.Fatalf("client deleted vendor hook: %d", code)
	}
	// Rotating the secret returns a new one, once.
	var rot api.EventHook
	created.RotateSecret, created.Secret = true, ""
	if code := call(t, ts, admin, "PUT", "/api/v1/event-hooks/"+created.ID, created, &rot); code != 200 || !strings.HasPrefix(rot.Secret, "whsec_") {
		t.Fatalf("rotate: %d %+v", code, rot)
	}
	var res map[string]any
	call(t, ts, lr.Token, "POST", "/api/v1/event-hooks/"+ch.ID+"/test", nil, &res)
	if res["ok"] != true {
		t.Fatalf("test delivery: %v", res)
	}

	// A failing endpoint is retried, recorded, and disabled after enough failures.
	client.mu.Lock()
	client.status = 500
	client.mu.Unlock()
	ctx := context.Background()
	for i := 0; i < hookMaxFailures; i++ {
		s.logEvent(ctx, "custom.ping", a1, "", "ping")
	}
	waitFor(t, "webhook disabled", func() bool {
		h, err := s.store.GetEventHook(ctx, ch.ID)
		return err == nil && !h.Enabled
	})
	h, _ := s.store.GetEventHook(ctx, ch.ID)
	if h.LastDelivery == nil || h.LastDelivery.OK || h.LastDelivery.Attempts != hookAttempts || !strings.Contains(h.DisabledReason, "HTTP 500") {
		t.Fatalf("disabled hook: %+v %+v", h, h.LastDelivery)
	}
	var evs []api.Event
	call(t, ts, admin, "GET", "/api/v1/events?type=hook.disabled", nil, &evs)
	if len(evs) != 1 {
		t.Fatalf("hook.disabled events: %+v", evs)
	}
	// Turning it back on clears the failure count.
	h.Enabled, h.Secret = true, ""
	var back api.EventHook
	call(t, ts, lr.Token, "PUT", "/api/v1/event-hooks/"+ch.ID, h, &back)
	if !back.Enabled || back.Failures != 0 || back.DisabledReason != "" || !back.HasSecret {
		t.Fatalf("re-enabled: %+v", back)
	}
}
