// Package platformtest installs the agent on the machine it runs on, the
// way a customer would, and checks it enrolls, restarts and updates itself.
//
//	ARMADA_PLATFORM_TEST=install go test -v -run TestPlatform ./internal/platformtest
//
// "install" uses the real installer and service manager: install.sh with
// systemd on Linux and launchd on macOS, install.ps1 and the Windows
// service on Windows. It needs root/Administrator (sudo without a password
// on Linux and macOS) and changes the machine, so it is meant for
// throwaway CI machines (.github/workflows/platforms.yml).
//
// "direct" runs the agent as a plain process instead, to check the test
// itself anywhere (Linux and macOS: a self-update replaces the process in
// place there, which on Windows needs the service).
package platformtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const adminToken = "platform-test-admin-token"

type env struct {
	t      *testing.T
	url    string
	mode   string
	dir    string
	logs   *bytes.Buffer // server log
	device string
}

func TestPlatform(t *testing.T) {
	mode := os.Getenv("ARMADA_PLATFORM_TEST")
	switch mode {
	case "install", "direct":
	case "":
		t.Skip("set ARMADA_PLATFORM_TEST=install (changes this machine) or direct")
	default:
		t.Fatalf("ARMADA_PLATFORM_TEST=%s: use install or direct", mode)
	}
	if mode == "direct" && runtime.GOOS == "windows" {
		t.Skip("direct mode can't self-update on Windows; use install")
	}
	e := &env{t: t, mode: mode, dir: t.TempDir(), logs: &bytes.Buffer{}}
	server := e.build("armada-server", "")
	v1 := e.build("armada-agent", "1.0.0")
	v2 := e.build("armada-agent", "1.0.1")
	e.startServer(server)

	e.upload(v1, "1.0.0")
	var tok struct{ Token string }
	e.call("POST", "/api/v1/tokens", map[string]any{"labels": map[string]string{"test": "platform"}}, &tok, 201)

	// 1. Install and enroll.
	start := time.Now()
	stop := e.install(v1, tok.Token)
	defer stop()
	e.waitFor("the device to enroll and check in with 1.0.0", func() bool {
		var devs []map[string]any
		e.call("GET", "/api/v1/devices", nil, &devs, 200)
		for _, d := range devs {
			if pk, _ := d["packages"].(map[string]any); pk["armada-agent"] == "1.0.0" && d["status"] == "online" {
				e.device = d["id"].(string)
				return true
			}
		}
		return false
	})
	t.Logf("enrolled %s in %s", e.device, time.Since(start).Round(time.Second))
	pid := e.servicePID()

	// 2. Restart it from the dashboard.
	var act struct{ ID string }
	e.call("POST", "/api/v1/devices/"+e.device+"/actions", map[string]any{"type": "restart-agent"}, &act, 201)
	restarted := time.Now()
	e.waitFor("the restart to finish and the agent to check in again", func() bool {
		var acts []map[string]any
		e.call("GET", "/api/v1/devices/"+e.device+"/actions", nil, &acts, 200)
		done := false
		for _, a := range acts {
			done = done || (a["id"] == act.ID && a["status"] == "succeeded")
		}
		return done && e.seenSince(restarted.Add(time.Second))
	})
	switch {
	case pid == "":
	case runtime.GOOS == "windows":
		// The agent exits; the service's recovery settings start it again.
		e.waitFor("the service manager to start the agent again", func() bool { p := e.servicePID(); return p != "" && p != pid })
	default:
		// On Linux and macOS it replaces itself in the same process.
		if p := e.servicePID(); p != pid {
			t.Errorf("service process changed from %s to %q; the agent should restart in place", pid, p)
		}
	}
	t.Logf("restarted (process %s -> %s)", pid, e.servicePID())

	// 3. Update itself.
	rel := e.upload(v2, "1.0.1")
	e.call("POST", "/api/v1/deployments", map[string]any{"name": "agent 1.0.1", "release_id": rel,
		"selector": map[string]any{"device_ids": []string{e.device}}, "strategy": map[string]any{"stages": []int{100}}}, nil, 201)
	e.waitFor("the agent to update itself to 1.0.1", func() bool {
		d := e.getDevice()
		pk, _ := d["packages"].(map[string]any)
		return pk["armada-agent"] == "1.0.1" && d["status"] == "online"
	})
	if bin := installedBinary(); e.mode == "install" && bin != "" {
		out, err := exec.Command(bin, "-version").CombinedOutput()
		if err != nil || !strings.Contains(string(out), "1.0.1") {
			t.Errorf("%s -version after the update: %s %v", bin, out, err)
		}
	}
	t.Log("updated to 1.0.1")
	if e.mode == "install" {
		e.uninstall()
	}
}

// ---- building and the server ----

func (e *env) build(cmd, version string) string {
	name := cmd
	if version != "" {
		name += "-" + version
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(e.dir, name)
	ld := "-X main.version=" + version
	if version != "" {
		ld += " -X github.com/nasi-lemak/armada/internal/agent.Version=" + version
	}
	c := exec.Command("go", "build", "-ldflags", ld, "-o", out, "../../cmd/"+cmd)
	c.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := c.CombinedOutput(); err != nil {
		e.t.Fatalf("build %s: %v\n%s", cmd, err, b)
	}
	return out
}

func (e *env) startServer(bin string) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	e.url = "http://" + addr
	cmd := exec.Command(bin, "-data-dir", filepath.Join(e.dir, "data"), "-admin-token", adminToken, "-listen", addr,
		"-public-url", e.url, "-heartbeat-interval", "3s", "-engine-interval", "1s", "-rollout-approval", "off")
	cmd.Stdout, cmd.Stderr = e.logs, e.logs
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if e.t.Failed() {
			e.t.Logf("server log:\n%s", e.logs)
		}
	})
	for i := 0; i < 100; i++ {
		if res, err := http.Get(e.url + "/healthz"); err == nil {
			res.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.t.Fatalf("server did not start:\n%s", e.logs)
}

func (e *env) call(method, path string, in, out any, want int) {
	e.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.url+path, body)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if want != 0 && res.StatusCode != want {
		e.t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, b)
	}
	if out != nil {
		json.Unmarshal(b, out)
	}
}

// upload adds an agent release for this platform and returns its ID.
func (e *env) upload(bin, version string) string {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{"package": "armada-agent", "version": version, "os": runtime.GOOS, "arch": runtime.GOARCH} {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", filepath.Base(bin))
	f, err := os.Open(bin)
	if err != nil {
		e.t.Fatal(err)
	}
	io.Copy(fw, f)
	f.Close()
	mw.Close()
	req, _ := http.NewRequest("POST", e.url+"/api/v1/releases", &buf)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct{ Release struct{ ID string } }
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 201 || json.Unmarshal(b, &out) != nil {
		e.t.Fatalf("upload agent %s: %d %s", version, res.StatusCode, b)
	}
	return out.Release.ID
}

// getDevice returns the device (GET /devices/{id} wraps it with its
// updates, actions and events).
func (e *env) getDevice() map[string]any {
	var out struct{ Device map[string]any }
	e.call("GET", "/api/v1/devices/"+e.device, nil, &out, 200)
	if out.Device == nil {
		e.t.Fatal("no device in GET /api/v1/devices/" + e.device)
	}
	return out.Device
}

func (e *env) seenSince(t time.Time) bool {
	d := e.getDevice()
	seen, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(d["last_seen_at"]))
	return seen.After(t)
}

func (e *env) waitFor(what string, ok func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(time.Second)
	}
	e.t.Logf("agent log:\n%s", agentLog(e))
	e.t.Fatalf("timed out waiting for %s", what)
}

// ---- installing ----

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// sudo runs a command as root (directly when already root).
func sudo(t *testing.T, args ...string) string {
	t.Helper()
	if os.Geteuid() == 0 {
		return run(t, args[0], args[1:]...)
	}
	return run(t, "sudo", append([]string{"-n"}, args...)...)
}

// install installs the agent and returns a function that stops it.
func (e *env) install(bin, token string) func() {
	t := e.t
	if e.mode == "direct" {
		exe := filepath.Join(e.dir, "agent", filepath.Base(bin))
		os.MkdirAll(filepath.Dir(exe), 0o755)
		b, _ := os.ReadFile(bin)
		os.WriteFile(exe, b, 0o755)
		log := filepath.Join(e.dir, "agent.log")
		cmd := exec.Command(exe, "-server", e.url, "-token", token, "-state-dir", filepath.Join(e.dir, "agent-state"), "-config", "", "-log-file", log)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return func() { cmd.Process.Kill(); cmd.Wait() }
	}
	switch runtime.GOOS {
	case "windows":
		script := fmt.Sprintf("$env:ARMADA_SERVER_URL='%s'; $env:ARMADA_ENROLLMENT_TOKEN='%s'; irm %s/install.ps1 | iex", e.url, token, e.url)
		t.Log(run(t, "powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script))
	default:
		sh := fmt.Sprintf("curl -fsSL %s/install.sh | env ARMADA_SERVER_URL=%s ARMADA_ENROLLMENT_TOKEN=%s sh", e.url, e.url, token)
		t.Log(sudo(t, "sh", "-c", sh))
	}
	return func() {}
}

func (e *env) uninstall() {
	t := e.t
	switch runtime.GOOS {
	case "windows":
		t.Log(run(t, installedBinary(), "-uninstall-service"))
		if out, _ := exec.Command("sc.exe", "query", "armada-agent").CombinedOutput(); !strings.Contains(string(out), "1060") {
			t.Errorf("the service is still there after -uninstall-service:\n%s", out)
		}
	case "darwin":
		sudo(t, "launchctl", "bootout", "system/com.armada.agent")
		sudo(t, "rm", "-f", "/Library/LaunchDaemons/com.armada.agent.plist")
	default:
		sudo(t, "systemctl", "disable", "--now", "armada-agent")
	}
	t.Log("uninstalled")
}

func installedBinary() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("ProgramFiles"), "Armada", "armada-agent.exe")
	default:
		return "/usr/local/bin/armada-agent"
	}
}

// servicePID returns the agent service's process ID ("" in direct mode or
// when it isn't running).
func (e *env) servicePID() string {
	if e.mode != "install" {
		return ""
	}
	var out []byte
	switch runtime.GOOS {
	case "windows":
		out, _ = exec.Command("sc.exe", "queryex", "armada-agent").CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "PID" {
				if p := strings.TrimSpace(v); p != "0" {
					return p
				}
			}
		}
	case "darwin":
		out, _ = exec.Command("sudo", "-n", "launchctl", "print", "system/com.armada.agent").CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), " = "); ok && k == "pid" {
				return strings.TrimSpace(v)
			}
		}
	default:
		out, _ = exec.Command("systemctl", "show", "-p", "MainPID", "--value", "armada-agent").CombinedOutput()
		if p := strings.TrimSpace(string(out)); p != "0" {
			return p
		}
	}
	return ""
}

func agentLog(e *env) string {
	var out []byte
	switch {
	case e.mode == "direct":
		out, _ = os.ReadFile(filepath.Join(e.dir, "agent.log"))
	case runtime.GOOS == "windows":
		out, _ = os.ReadFile(filepath.Join(os.Getenv("ProgramData"), "Armada", "agent.log"))
	case runtime.GOOS == "darwin":
		out, _ = exec.Command("sudo", "-n", "cat", "/var/log/armada-agent.log", "/var/log/armada-agent.stderr.log").CombinedOutput()
	default:
		out, _ = exec.Command("sudo", "-n", "journalctl", "-u", "armada-agent", "--no-pager", "-n", "200").CombinedOutput()
	}
	return string(out)
}
