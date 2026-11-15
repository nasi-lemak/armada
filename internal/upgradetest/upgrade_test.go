// Package upgradetest checks that a database written by an older release
// keeps working after upgrading to this build: an old armada-server binary
// is started, filled with data through its API, stopped, and then this
// build's server is started on the same data directory and checked.
//
// It needs an old binary, so it only runs when asked; scripts/upgrade-test.sh
// builds old releases from git and runs it against each:
//
//	ARMADA_UPGRADE_OLD=/path/to/old/armada-server go test -run TestUpgrade ./internal/upgradetest
//
// ARMADA_UPGRADE_NEW names this build's binary (default: built here).
package upgradetest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

const adminToken = "upgrade-test-admin-token"

// seeded is what the old server was given, to check against afterwards.
type seeded struct {
	devices      map[string]device // by ID
	token        string            // enrollment token (acme)
	releases     map[string]string // release ID -> artifact sha256
	deploy       string            // deployment ID
	deployStatus string
	user         string // client account email
	password     string
	channel      string
	rule         string
	events       int
	schemaOld    int
}

type device struct {
	id, key, name string
	labels        map[string]string
}

func TestUpgrade(t *testing.T) {
	old := os.Getenv("ARMADA_UPGRADE_OLD")
	if old == "" {
		t.Skip("set ARMADA_UPGRADE_OLD to an old armada-server binary (see scripts/upgrade-test.sh)")
	}
	cur := os.Getenv("ARMADA_UPGRADE_NEW")
	if cur == "" {
		cur = filepath.Join(t.TempDir(), "armada-server")
		build := exec.Command("go", "build", "-o", cur, "../../cmd/armada-server")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build this server: %v\n%s", err, out)
		}
	}
	data := t.TempDir()

	// 1. The old release, with data.
	srv := start(t, old, data)
	s := seed(t, srv)
	s.schemaOld = schemaVersion(t, old, data)
	srv.stop(t)
	t.Logf("old server (schema version %d): %d devices, %d releases, %d events", s.schemaOld, len(s.devices), len(s.releases), s.events)

	// 2. This build, on the same data.
	if v := schemaVersion(t, cur, data); v != s.schemaOld {
		t.Fatalf("-migrate-status changed the database: version %d, was %d", v, s.schemaOld)
	}
	srv = start(t, cur, data)
	newVersion := schemaVersion(t, cur, data)
	if newVersion <= s.schemaOld {
		t.Logf("no migrations between versions %d and %d", s.schemaOld, newVersion)
	} else {
		backups, _ := filepath.Glob(filepath.Join(data, "armada.db.before-v*"))
		if len(backups) == 0 {
			t.Error("upgrading did not keep a copy of the old database")
		}
	}
	verify(t, srv, s)
	useNewFeatures(t, srv, s)
	srv.stop(t)

	// 3. Starting again changes nothing, and the old release refuses the
	// upgraded database instead of running on it.
	before, _ := filepath.Glob(filepath.Join(data, "armada.db.before-v*"))
	srv = start(t, cur, data)
	verify(t, srv, s)
	srv.stop(t)
	after, _ := filepath.Glob(filepath.Join(data, "armada.db.before-v*"))
	if len(after) != len(before) {
		t.Errorf("a second start made another database copy: %v", after)
	}
	if newVersion > s.schemaOld {
		out, err := runOnce(old, data)
		if err == nil || !strings.Contains(out, "newer") {
			t.Errorf("the old server ran on the upgraded database (err %v):\n%s", err, out)
		}
	}
}

// ---- running a server binary ----

type server struct {
	cmd  *exec.Cmd
	url  string
	logs *bytes.Buffer
}

func start(t *testing.T, bin, data string) *server {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	s := &server{url: "http://" + addr, logs: &bytes.Buffer{}}
	s.cmd = exec.Command(bin, "-data-dir", data, "-admin-token", adminToken, "-listen", addr, "-public-url", s.url)
	s.cmd.Stdout, s.cmd.Stderr = s.logs, s.logs
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.cmd.Process.Kill(); s.cmd.Wait() })
	for i := 0; i < 100; i++ {
		if res, err := http.Get(s.url + "/healthz"); err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return s
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not start:\n%s", bin, s.logs)
	return nil
}

func (s *server) stop(t *testing.T) {
	t.Helper()
	s.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		s.cmd.Process.Kill()
		t.Fatalf("server did not stop:\n%s", s.logs)
	}
	if t.Failed() {
		t.Logf("server log:\n%s", s.logs)
	}
}

// runOnce starts a server that is expected to exit by itself.
func runOnce(bin, data string) (string, error) {
	cmd := exec.Command(bin, "-data-dir", data, "-admin-token", adminToken, "-listen", "127.0.0.1:0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-done
		return out.String(), nil
	}
}

func schemaVersion(t *testing.T, bin, data string) int {
	t.Helper()
	out, err := exec.Command(bin, "-data-dir", data, "-migrate-status").CombinedOutput()
	if err != nil {
		t.Fatalf("-migrate-status: %v\n%s", err, out)
	}
	var v int
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "schema version:") {
			fmt.Sscanf(strings.TrimPrefix(line, "schema version:"), "%d", &v)
		}
	}
	if v == 0 {
		t.Fatalf("no schema version in -migrate-status output:\n%s", out)
	}
	return v
}

// ---- HTTP ----

type obj = map[string]any

func (s *server) do(t *testing.T, token, method, path string, body any, want int) []byte {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.url+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	if want != 0 && res.StatusCode != want {
		t.Fatalf("%s %s: %d (want %d): %s", method, path, res.StatusCode, want, out)
	}
	return out
}

func (s *server) json(t *testing.T, token, method, path string, body any, want int) obj {
	t.Helper()
	var o obj
	if err := json.Unmarshal(s.do(t, token, method, path, body, want), &o); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return o
}

// list decodes a list response: a JSON array, or an object holding one.
func (s *server) list(t *testing.T, token, path string) []obj {
	t.Helper()
	b := s.do(t, token, "GET", path, nil, 200)
	var arr []obj
	if json.Unmarshal(b, &arr) == nil {
		return arr
	}
	var o map[string]json.RawMessage
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if json.Unmarshal(o[k], &arr) == nil && arr != nil {
			return arr
		}
	}
	t.Fatalf("GET %s: no list in %s", path, b)
	return nil
}

func str(o obj, k string) string { v, _ := o[k].(string); return v }

func (s *server) upload(t *testing.T, pkg, version string, content []byte) obj {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("package", pkg)
	mw.WriteField("version", version)
	mw.WriteField("os", "linux")
	fw, _ := mw.CreateFormFile("file", pkg+".tar.gz")
	fw.Write(content)
	mw.Close()
	req, _ := http.NewRequest("POST", s.url+"/api/v1/releases", &buf)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 201 {
		t.Fatalf("upload %s %s: %d %s", pkg, version, res.StatusCode, b)
	}
	var out struct{ Release obj }
	json.Unmarshal(b, &out)
	return out.Release
}

func heartbeat(i int, version string) obj {
	return obj{
		"info":     obj{"os": "linux", "arch": "arm64", "hostname": fmt.Sprintf("host-%d", i)},
		"metrics":  obj{"num_cpu": 4, "load1": 0.3, "uptime_seconds": 86400, "mem_total_bytes": 4 << 30, "mem_available_bytes": 2 << 30},
		"packages": obj{"kiosk": version},
		"health":   []obj{{"name": "disk /", "kind": "disk_space", "status": "ok", "value": 41, "unit": "%"}},
		"config":   []obj{},
	}
}

// ---- seeding the old release ----

func seed(t *testing.T, srv *server) *seeded {
	s := &seeded{devices: map[string]device{}, releases: map[string]string{}, password: "upgrade-test-password-1"}
	tokens := map[string]string{}
	for _, c := range []string{"acme", "globex"} {
		tok := srv.json(t, adminToken, "POST", "/api/v1/tokens", obj{"labels": obj{"customer": c, "site": c + "-hq"}}, 201)
		tokens[c] = str(tok, "token")
	}
	s.token = tokens["acme"]
	for i, c := range []string{"acme", "acme", "globex"} {
		name := fmt.Sprintf("%s-kiosk-%d", c, i)
		er := srv.json(t, "", "POST", "/api/v1/agent/enroll", obj{"token": tokens[c], "name": name,
			"info": obj{"os": "linux", "arch": "arm64", "hostname": name}}, 0)
		d := device{id: str(er, "device_id"), key: str(er, "device_key"), name: name,
			labels: map[string]string{"customer": c, "site": c + "-hq"}}
		if d.id == "" || d.key == "" {
			t.Fatalf("enroll %s: %v", name, er)
		}
		srv.do(t, d.key, "POST", "/api/v1/agent/heartbeat", heartbeat(i, "1.0.0"), 200)
		s.devices[d.id] = d
	}
	// Relabel one device, as an operator would.
	for id, d := range s.devices {
		if d.labels["customer"] == "globex" {
			d.labels["room"] = "lobby"
			srv.do(t, adminToken, "PATCH", "/api/v1/devices/"+id, obj{"labels": d.labels}, 200)
		}
	}

	for _, v := range []string{"1.0.0", "2.0.0"} {
		content := []byte("kiosk build " + v + strings.Repeat(".", 3000))
		rel := srv.upload(t, "kiosk", v, content)
		sum := sha256.Sum256(content)
		s.releases[str(rel, "id")] = hex.EncodeToString(sum[:])
	}
	var v2 string
	for id := range s.releases {
		rel := srv.json(t, adminToken, "GET", "/api/v1/releases/"+id, nil, 200)
		if str(rel, "version") == "2.0.0" {
			v2 = id
		}
	}
	dep := srv.json(t, adminToken, "POST", "/api/v1/deployments", obj{"name": "kiosk 2.0 for acme", "release_id": v2,
		"selector": obj{"labels": obj{"customer": "acme"}}, "strategy": obj{"stages": []int{100}}}, 201)
	s.deploy = str(dep, "id")
	s.deployStatus = str(srv.json(t, adminToken, "GET", "/api/v1/deployments/"+s.deploy, nil, 200), "status")

	s.user = "it@acme.example"
	srv.do(t, adminToken, "POST", "/api/v1/users", obj{"email": s.user, "name": "Acme IT", "role": "operator",
		"scope": obj{"labels": obj{"customer": "acme"}}, "password": s.password}, 201)
	ch := srv.json(t, adminToken, "POST", "/api/v1/channels", obj{"name": "ops", "type": "webhook",
		"webhook": obj{"url": "http://127.0.0.1:9/hook"}}, 201)
	s.channel = str(ch, "id")
	rule := srv.json(t, adminToken, "POST", "/api/v1/alert-rules", obj{"name": "kiosk offline", "condition": "device_offline",
		"enabled": true, "channels": []string{s.channel}, "for_seconds": 600}, 201)
	s.rule = str(rule, "id")
	for id := range s.devices {
		srv.do(t, adminToken, "POST", "/api/v1/devices/"+id+"/actions", obj{"type": "reboot"}, 0)
		break
	}
	s.events = len(srv.list(t, adminToken, "/api/v1/events?limit=1000"))
	return s
}

// ---- checking this build ----

func verify(t *testing.T, srv *server, s *seeded) {
	t.Helper()
	devs := srv.list(t, adminToken, "/api/v1/devices")
	if len(devs) != len(s.devices) {
		t.Fatalf("%d devices after the upgrade, want %d", len(devs), len(s.devices))
	}
	for _, d := range devs {
		want, ok := s.devices[str(d, "id")]
		if !ok {
			t.Errorf("unknown device after the upgrade: %v", d)
			continue
		}
		if str(d, "name") != want.name {
			t.Errorf("device %s is named %q, want %q", want.id, str(d, "name"), want.name)
		}
		labels, _ := d["labels"].(obj)
		for k, v := range want.labels {
			if labels[k] != v {
				t.Errorf("device %s label %s = %v, want %s", want.name, k, labels[k], v)
			}
		}
		if pk, _ := d["packages"].(obj); pk["kiosk"] == nil {
			t.Errorf("device %s lost its package list: %v", want.name, d["packages"])
		}
		// The device's key still works.
		srv.do(t, want.key, "POST", "/api/v1/agent/heartbeat", heartbeat(0, "1.0.0"), 200)
		srv.do(t, adminToken, "GET", "/api/v1/devices/"+want.id, nil, 200)
	}

	rels := srv.list(t, adminToken, "/api/v1/releases")
	if len(rels) != len(s.releases) {
		t.Errorf("%d releases after the upgrade, want %d", len(rels), len(s.releases))
	}
	for id, sum := range s.releases {
		b := srv.do(t, adminToken, "GET", "/api/v1/releases/"+id+"/artifact", nil, 200)
		got := sha256.Sum256(b)
		if hex.EncodeToString(got[:]) != sum {
			t.Errorf("release %s artifact changed", id)
		}
	}
	dep := srv.json(t, adminToken, "GET", "/api/v1/deployments/"+s.deploy, nil, 200)
	if str(dep, "status") != s.deployStatus {
		t.Errorf("deployment status %q after the upgrade, want %q", str(dep, "status"), s.deployStatus)
	}

	// The client account signs in with its password and sees only its devices.
	login := srv.json(t, "", "POST", "/api/v1/auth/login", obj{"email": s.user, "password": s.password}, 200)
	sess := str(login, "token")
	if sess == "" {
		t.Fatalf("login after the upgrade: %v", login)
	}
	mine := srv.list(t, sess, "/api/v1/devices")
	if len(mine) != 2 {
		t.Errorf("client account sees %d devices, want 2", len(mine))
	}
	for _, d := range mine {
		if l, _ := d["labels"].(obj); l["customer"] != "acme" {
			t.Errorf("client account sees another customer's device: %v", d["name"])
		}
	}
	srv.do(t, sess, "POST", "/api/v1/auth/logout", nil, 0)

	found := false
	for _, r := range srv.list(t, adminToken, "/api/v1/alert-rules") {
		found = found || str(r, "id") == s.rule
	}
	if !found {
		t.Error("alert rule lost in the upgrade")
	}
	found = false
	for _, c := range srv.list(t, adminToken, "/api/v1/channels") {
		found = found || str(c, "id") == s.channel
	}
	if !found {
		t.Error("notification channel lost in the upgrade")
	}
	if n := len(srv.list(t, adminToken, "/api/v1/events?limit=1000")); n < s.events {
		t.Errorf("%d events after the upgrade, had %d", n, s.events)
	}
	srv.do(t, adminToken, "GET", "/api/v1/summary", nil, 200)
}

// useNewFeatures exercises features newer than most old releases on the
// upgraded data, so new tables and columns are known to work there too.
func useNewFeatures(t *testing.T, srv *server, s *seeded) {
	t.Helper()
	var id string
	for id = range s.devices {
		break
	}
	// The old enrollment token still enrolls devices.
	er := srv.json(t, "", "POST", "/api/v1/agent/enroll", obj{"token": s.token, "name": "acme-new",
		"info": obj{"os": "linux", "arch": "amd64", "serial": "SN-NEW-1"}}, 0)
	if str(er, "device_id") == "" {
		t.Fatalf("enroll with an old token: %v", er)
	}
	srv.do(t, str(er, "device_key"), "POST", "/api/v1/agent/heartbeat", heartbeat(9, "2.0.0"), 200)
	srv.do(t, adminToken, "DELETE", "/api/v1/devices/"+str(er, "device_id"), nil, 0)

	srv.do(t, adminToken, "GET", "/api/v1/devices/"+id+"/timeline", nil, 200)
	srv.do(t, adminToken, "GET", "/api/v1/insights", nil, 200)
	srv.do(t, adminToken, "GET", "/api/v1/search?q=kiosk", nil, 200)
	srv.do(t, adminToken, "GET", "/api/v1/events?format=csv&type=device.*", nil, 200)
	srv.do(t, adminToken, "GET", "/api/v1/openapi.json", nil, 200)
	srv.do(t, adminToken, "GET", "/api/v1/registrations", nil, 200)
	srv.do(t, adminToken, "GET", "/api/v1/sites", nil, 200)
	srv.do(t, adminToken, "GET", "/api/v1/backup/status", nil, 200)

	sched := srv.json(t, adminToken, "POST", "/api/v1/scheduled-actions", obj{"name": "nightly reboot", "operation": "reboot",
		"selector": obj{"labels": obj{"customer": "acme"}}, "at": "03:00", "time_zone": "UTC"}, 201)
	srv.do(t, adminToken, "DELETE", "/api/v1/scheduled-actions/"+str(sched, "id"), nil, 0)
	hook := srv.json(t, adminToken, "POST", "/api/v1/event-hooks", obj{"name": "audit", "url": "http://127.0.0.1:9/events",
		"enabled": true}, 201)
	srv.do(t, adminToken, "DELETE", "/api/v1/event-hooks/"+str(hook, "id"), nil, 0)
	site := srv.json(t, adminToken, "POST", "/api/v1/sites", obj{"name": "plant 2"}, 201)
	srv.do(t, adminToken, "DELETE", "/api/v1/sites/"+str(site, "id"), nil, 0)

	// A backup of the upgraded database.
	b := srv.do(t, adminToken, "GET", "/api/v1/backup", nil, 200)
	if len(b) < 1000 {
		t.Errorf("backup is only %d bytes", len(b))
	}
}
