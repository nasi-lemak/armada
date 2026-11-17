package server

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestOffboarding(t *testing.T) {
	s, ts := setup(t)
	ctx := t.Context()
	acme := map[string]string{"customer": "acme"}
	globex := map[string]string{"customer": "globex"}
	enroll := func(name string, labels map[string]string) string {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Description: name, Labels: labels}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux"}}, &er)
		call(t, ts, er.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}}, nil)
		return er.DeviceID
	}
	acme1, acme2, glx := enroll("acme-1", acme), enroll("acme-2", acme), enroll("globex-1", globex)
	user := func(email string, req api.CreateUserRequest) {
		req.Email, req.Password = email, "long-enough-password"
		if code := call(t, ts, admin, "POST", "/api/v1/users", req, nil); code != 201 {
			t.Fatalf("create %s: %d", email, code)
		}
	}
	user("it@acme.com", api.CreateUserRequest{Role: api.RoleOperator, Scope: api.Selector{Labels: acme}})
	user("it@globex.com", api.CreateUserRequest{Role: api.RoleViewer, Scope: api.Selector{Labels: globex}})
	user("partner@msp.com", api.CreateUserRequest{Grants: []api.Grant{{Role: api.RoleOperator, Scope: api.Selector{Labels: acme}}, {Role: api.RoleViewer, Scope: api.Selector{Labels: globex}}}})
	user("ops@vendor.com", api.CreateUserRequest{Role: api.RoleAdmin})
	s.store.PutEventHook(ctx, &api.EventHook{ID: "hook_acme", Name: "acme siem", URL: "https://siem.acme.example/x", Enabled: true, ClientScope: api.Selector{Labels: acme}, Secret: "very-secret-value"})
	s.store.PutScheduledAction(ctx, &api.ScheduledAction{ID: "sa_acme", Name: "acme reboot", Operation: "reboot", Selector: api.Selector{Labels: acme}, At: "03:00"})
	s.store.PutScheduledAction(ctx, &api.ScheduledAction{ID: "sa_glx", Name: "globex reboot", Operation: "reboot", Selector: api.Selector{Labels: globex}, At: "03:00"})
	s.store.PutRegistration(ctx, &api.Registration{Serial: "ACME-9", Labels: acme})

	// The plan, without changing anything.
	var plan offboardPlan
	if code := call(t, ts, admin, "POST", "/api/v1/offboarding", offboardRequest{Labels: acme}, &plan); code != 200 {
		t.Fatalf("plan: %d", code)
	}
	names := func(items []offboardItem) string {
		var out []string
		for _, it := range items {
			out = append(out, it.Name)
		}
		return strings.Join(out, ",")
	}
	if plan.Confirm != "customer=acme" || names(plan.Devices) != "acme-1,acme-2" || names(plan.Users) != "it@acme.com" ||
		names(plan.UsersNarrowed) != "partner@msp.com" || names(plan.EventHooks) != "acme siem" || names(plan.Schedules) != "acme reboot" ||
		len(plan.Tokens) != 2 || len(plan.Registrations) != 1 || plan.Done {
		t.Fatalf("plan: %+v", plan)
	}

	// The export: everything about acme, no secrets.
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/offboarding/export?labels=customer=acme", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Disposition"), "armada-export-customer_acme") &&
		!strings.Contains(res.Header.Get("Content-Disposition"), "armada-export-customer") {
		t.Fatalf("export: %d %v", res.StatusCode, res.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(b)
	}
	devFiles := 0
	for name, content := range files {
		if strings.HasPrefix(name, "devices/") {
			devFiles++
			if strings.Contains(content, "globex") {
				t.Errorf("%s mentions another customer", name)
			}
		}
		if strings.Contains(content, "very-secret-value") || strings.Contains(content, "long-enough-password") {
			t.Errorf("%s holds a secret", name)
		}
	}
	if devFiles != 2 {
		t.Errorf("%d device files: %v", devFiles, files)
	}
	var accounts []api.User
	json.Unmarshal([]byte(files["accounts.json"]), &accounts)
	if len(accounts) != 2 {
		t.Errorf("accounts: %s", files["accounts.json"])
	}
	rows, _ := csv.NewReader(strings.NewReader(files["activity.csv"])).ReadAll()
	sawAcme := false
	for _, r := range rows[1:] {
		sawAcme = sawAcme || r[3] == acme1
		if r[3] == glx {
			t.Errorf("activity export has a globex event: %v", r)
		}
	}
	if !sawAcme || !strings.Contains(files["settings.json"], "acme siem") || !strings.Contains(files["manifest.json"], `"devices": 2`) {
		t.Errorf("export lacks acme's data:\n%s\n%s", files["settings.json"], files["manifest.json"])
	}

	// A wrong confirmation changes nothing.
	if code := call(t, ts, admin, "POST", "/api/v1/offboarding", offboardRequest{Labels: acme, Confirm: "acme"}, nil); code != 400 {
		t.Errorf("wrong confirm: %d", code)
	}
	if _, err := s.store.GetDevice(ctx, acme1); err != nil {
		t.Fatal("a wrong confirmation removed a device")
	}
	// Operators can't offboard.
	var login api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-enough-password"}, &login)
	if code := call(t, ts, login.Token, "POST", "/api/v1/offboarding", offboardRequest{Labels: acme}, nil); code != 403 {
		t.Errorf("client account: %d", code)
	}

	// Run it.
	var done offboardPlan
	if code := call(t, ts, admin, "POST", "/api/v1/offboarding", offboardRequest{Labels: acme, Confirm: "customer=acme"}, &done); code != 200 || !done.Done {
		t.Fatalf("offboard: %d %+v", code, done)
	}
	for _, id := range []string{acme1, acme2} {
		if _, err := s.store.GetDevice(ctx, id); err == nil {
			t.Errorf("device %s is still there", id)
		}
	}
	if _, err := s.store.GetDevice(ctx, glx); err != nil {
		t.Error("globex's device was removed")
	}
	users, _ := s.store.ListUsers(ctx)
	byEmail := map[string]*api.User{}
	for _, u := range users {
		byEmail[u.Email] = u
	}
	if byEmail["it@acme.com"] != nil || byEmail["it@globex.com"] == nil || byEmail["ops@vendor.com"] == nil {
		t.Errorf("accounts after: %v", byEmail)
	}
	if p := byEmail["partner@msp.com"]; p == nil || len(p.Grants) != 1 || p.Grants[0].Scope.Labels["customer"] != "globex" {
		t.Errorf("partner kept %+v", p)
	}
	if hooks, _ := s.store.ListEventHooks(ctx); len(hooks) != 0 {
		t.Errorf("hooks left: %d", len(hooks))
	}
	if sas, _ := s.store.ListScheduledActions(ctx); len(sas) != 1 || sas[0].ID != "sa_glx" {
		t.Errorf("schedules left: %+v", sas)
	}
	toks, _ := s.store.ListTokens(ctx)
	for _, tk := range toks {
		if tk.Labels["customer"] == "acme" && !tk.Revoked || tk.Labels["customer"] == "globex" && tk.Revoked {
			t.Errorf("token %s revoked=%v", tk.Description, tk.Revoked)
		}
	}
	var evs []api.Event
	call(t, ts, admin, "GET", "/api/v1/events?type=customer.offboarded", nil, &evs)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "2 devices deleted") {
		t.Errorf("offboarding event: %+v", evs)
	}

	// Retiring keeps the devices as records.
	call(t, ts, admin, "POST", "/api/v1/offboarding", offboardRequest{Labels: globex, DeviceAction: "retire", Confirm: "customer=globex"}, &done)
	if d, err := s.store.GetDevice(ctx, glx); err != nil || d.Lifecycle != api.LifecycleRetired {
		t.Errorf("globex device after retiring: %+v %v", d, err)
	}
}
