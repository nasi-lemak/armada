package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// Version is the agent's version, set at build time with
// -ldflags "-X github.com/nasi-lemak/armada/internal/agent.Version=1.2.3".
var Version = "dev"

// maxSelfUpdateStarts is how many times a freshly installed agent binary may
// start without completing a heartbeat before the previous binary is restored.
const maxSelfUpdateStarts = 3

// Agent is the on-device fleet agent.
type Agent struct {
	cfg     *Config
	log     *slog.Logger
	state   *stateFile
	client  *client
	windows []Window

	// ExePath is the agent binary replaced by self-updates (default: os.Executable).
	ExePath string
	// Reexec restarts the agent from ExePath. Overridable for tests.
	Reexec func(exe string) error
	// Now is the clock used for maintenance windows. Overridable for tests.
	Now func() time.Time

	mu       sync.Mutex
	busy     bool
	status   string
	interval time.Duration
	wg       sync.WaitGroup
	health   *healthRunner
	gw       *gateway
}

// New creates an agent from a validated config.
func New(cfg *Config, log *slog.Logger) (*Agent, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	st, err := loadState(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	windows, _ := ParseWindows(cfg.MaintenanceWindow)
	a := &Agent{cfg: cfg, log: log, state: st, windows: windows, Reexec: reexec, Now: time.Now, interval: 30 * time.Second,
		health: newHealthRunner(cfg.Health)}
	if cfg.Peripherals != nil {
		a.gw = newGateway(a, cfg.Peripherals)
	}
	a.client, err = newClient(cfg, func() string { return a.state.get().DeviceKey })
	if err != nil {
		return nil, err
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		a.ExePath = exe
	}
	return a, nil
}

// DeviceID returns the enrolled device ID, if any.
func (a *Agent) DeviceID() string { return a.state.get().DeviceID }

// Run enrolls if necessary and then checks in until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.Enroll(ctx); err != nil {
		return err
	}
	a.log.Info("agent running", "device_id", a.DeviceID(), "version", Version, "server", a.cfg.ServerURL)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.healthLoop(ctx)
	}()
	if a.gw != nil {
		a.wg.Add(2)
		go func() {
			defer a.wg.Done()
			a.gw.work(ctx)
		}()
		go func() {
			defer a.wg.Done()
			for ctx.Err() == nil {
				a.gw.scanIfDue(ctx)
				select {
				case <-ctx.Done():
				case <-time.After(15 * time.Second):
				}
			}
		}()
	}
	backoff := time.Second
	for {
		err := a.CheckIn(ctx)
		// Read the interval after checking in: the server may have just changed it.
		wait := jitter(a.currentInterval())
		if err != nil && ctx.Err() == nil {
			a.log.Warn("heartbeat failed", "err", err, "retry_in", backoff)
			if IsStatus(err, 401) {
				a.log.Error("server does not recognise this device; it may have been deleted. Remove " +
					filepath.Join(a.cfg.StateDir, "state.json") + " and restart to re-enroll")
			}
			wait = backoff
			backoff = min(backoff*2, 5*time.Minute)
		} else {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			a.wg.Wait()
			return nil
		case <-time.After(wait):
		case <-a.health.changed:
			// A health status changed: report it now rather than at the next interval.
		}
	}
}

// healthLoop runs due health checks every 15 seconds.
func (a *Agent) healthLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		a.health.runDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunHealthChecks runs all due health checks once (used by -once and tests).
func (a *Agent) RunHealthChecks(ctx context.Context) { a.health.runDue(ctx) }

func jitter(d time.Duration) time.Duration {
	// ±10% so a fleet that rebooted together doesn't check in in lockstep.
	return d + time.Duration((rand.Float64()*0.2-0.1)*float64(d))
}

func (a *Agent) currentInterval() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.interval
}

func (a *Agent) setStatus(s string) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
}

// ---- enrollment ----

// Enroll registers the device with the server if it has not been already, and
// checks whether a self-update from a previous run needs to be rolled back.
func (a *Agent) Enroll(ctx context.Context) error {
	if err := a.enroll(ctx); err != nil {
		return err
	}
	return a.checkSelfUpdateOnStart()
}

func (a *Agent) enroll(ctx context.Context) error {
	if a.state.get().DeviceKey != "" {
		return nil
	}
	if a.cfg.EnrollmentToken == "" {
		return errors.New("device is not enrolled: set enrollment_token (or ARMADA_ENROLLMENT_TOKEN)")
	}
	backoff := time.Second
	for {
		var resp api.EnrollResponse
		err := a.client.do(ctx, "POST", "/api/v1/agent/enroll", api.EnrollRequest{
			Token: a.cfg.EnrollmentToken, Name: a.cfg.Name, Info: collectInfo(),
		}, &resp)
		if err == nil {
			if a.cfg.SigningPublicKey != "" && a.cfg.SigningPublicKey != resp.SigningPublicKey {
				return errors.New("server signing key does not match signing_public_key in config; refusing to enroll")
			}
			if err := a.state.update(func(s *State) {
				s.DeviceID, s.DeviceKey, s.SigningPublicKey = resp.DeviceID, resp.DeviceKey, resp.SigningPublicKey
			}); err != nil {
				return err
			}
			if resp.HeartbeatIntervalSeconds > 0 {
				a.interval = time.Duration(resp.HeartbeatIntervalSeconds) * time.Second
			}
			a.log.Info("enrolled", "device_id", resp.DeviceID)
			return nil
		}
		var he *HTTPError
		if errors.As(err, &he) && he.Status < 500 {
			return fmt.Errorf("enrollment rejected: %w", err)
		}
		a.log.Warn("enrollment failed, retrying", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

// ---- heartbeat ----

// CheckIn sends one heartbeat and acts on the response. Updates and actions
// run in the background.
func (a *Agent) CheckIn(ctx context.Context) error {
	a.mu.Lock()
	busy, status := a.busy, a.status
	a.mu.Unlock()
	req := api.HeartbeatRequest{
		Info:     collectInfo(),
		Metrics:  collectMetrics(a.cfg.StateDir),
		Packages: a.packageVersions(ctx),
		Busy:     busy,
		Status:   status,
		Health:   a.health.snapshot(),
	}
	if a.gw != nil {
		req.Peripherals = a.gw.reports()
	}
	var resp api.HeartbeatResponse
	if err := a.client.do(ctx, "POST", "/api/v1/agent/heartbeat", req, &resp); err != nil {
		return err
	}
	if resp.HeartbeatIntervalSeconds > 0 {
		a.mu.Lock()
		a.interval = time.Duration(resp.HeartbeatIntervalSeconds) * time.Second
		a.mu.Unlock()
	}
	a.health.setMonitors(resp.Monitors)
	if a.gw != nil && len(resp.PeripheralUpdates) > 0 {
		a.gw.enqueue(resp.PeripheralUpdates)
	}
	a.confirmSelfUpdate()
	a.flushOutbox(ctx)
	if resp.Update != nil {
		a.maybeStartUpdate(ctx, *resp.Update)
	}
	for _, act := range resp.Actions {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.runAction(ctx, act)
		}()
	}
	return nil
}

// Wait blocks until background updates and actions finish.
func (a *Agent) Wait() { a.wg.Wait() }

func (a *Agent) packageVersions(ctx context.Context) map[string]string {
	st := a.state.get()
	out := map[string]string{api.AgentPackage: Version}
	for name, p := range a.cfg.Packages {
		if len(p.VersionCommand) > 0 {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			outp, code, err := runCommand(cctx, p.VersionCommand, nil)
			cancel()
			if err == nil && code == 0 {
				if v := firstLine(outp); v != "" {
					out[name] = v
					continue
				}
			}
		}
		if v := st.Installed[name].Version; v != "" {
			out[name] = v
		}
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// ---- reporting ----

func (a *Agent) report(ctx context.Context, rep api.UpdateReport) {
	err := a.client.do(ctx, "POST", "/api/v1/agent/updates", rep, nil)
	if err == nil {
		return
	}
	var he *HTTPError
	if errors.As(err, &he) && he.Status < 500 {
		a.log.Warn("update report rejected", "status", rep.Status, "err", err)
		return
	}
	// Server unreachable: keep the report and deliver it on a later check-in.
	if api.IsTerminalUpdateStatus(rep.Status) {
		_ = a.state.update(func(s *State) { s.Outbox = append(s.Outbox, rep) })
	}
}

func (a *Agent) flushOutbox(ctx context.Context) {
	pending := a.state.get().Outbox
	if len(pending) == 0 {
		return
	}
	var keep []api.UpdateReport
	for _, rep := range pending {
		err := a.client.do(ctx, "POST", "/api/v1/agent/updates", rep, nil)
		var he *HTTPError
		if err != nil && !(errors.As(err, &he) && he.Status < 500) {
			keep = append(keep, rep)
		}
	}
	_ = a.state.update(func(s *State) { s.Outbox = keep })
}

// ---- updates ----

func (a *Agent) maybeStartUpdate(ctx context.Context, u api.UpdateInstruction) {
	desc := fmt.Sprintf("%s %s", u.Package, u.Version)
	if !*a.cfg.AutoUpdate {
		a.setStatus("update available: " + desc + " (auto_update disabled)")
		return
	}
	if !InWindow(a.windows, a.Now()) {
		a.setStatus("update to " + desc + " deferred until maintenance window " + a.cfg.MaintenanceWindow)
		return
	}
	if u.Package != api.AgentPackage {
		if _, ok := a.cfg.Packages[u.Package]; !ok {
			a.report(ctx, api.UpdateReport{DeploymentID: u.DeploymentID, ReleaseID: u.ReleaseID, Status: api.UpdateFailed,
				Message: "package " + u.Package + " is not configured on this device"})
			return
		}
	}
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return
	}
	a.busy = true
	a.status = "updating " + desc
	a.mu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			a.mu.Lock()
			a.busy = false
			a.status = ""
			a.mu.Unlock()
		}()
		a.applyUpdate(ctx, u)
	}()
}

func (a *Agent) applyUpdate(ctx context.Context, u api.UpdateInstruction) {
	log := a.log.With("package", u.Package, "version", u.Version, "deployment", u.DeploymentID)
	rep := func(status, msg string) {
		if status == api.UpdateFailed || status == api.UpdateRolledBack {
			log.Warn("update "+status, "msg", msg)
		} else {
			log.Info("update "+status, "msg", msg)
		}
		a.report(ctx, api.UpdateReport{DeploymentID: u.DeploymentID, ReleaseID: u.ReleaseID, Status: status, Message: msg})
	}
	rep(api.UpdateDownloading, "")
	path, err := a.fetchArtifact(ctx, u)
	if err != nil {
		rep(api.UpdateFailed, err.Error())
		return
	}
	rep(api.UpdateInstalling, "")
	if u.Package == api.AgentPackage {
		if err := a.selfUpdate(ctx, u, path); err != nil {
			rep(api.UpdateFailed, err.Error())
		}
		return
	}
	status, msg := a.installPackage(ctx, u, path)
	rep(status, msg)
}

// fetchArtifact downloads and verifies a release, returning the local path.
func (a *Agent) fetchArtifact(ctx context.Context, u api.UpdateInstruction) (string, error) {
	pubB64 := a.cfg.SigningPublicKey
	if pubB64 == "" {
		pubB64 = a.state.get().SigningPublicKey
	}
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", errors.New("no valid release-signing key; refusing to install")
	}
	sig, err := base64.StdEncoding.DecodeString(u.Signature)
	if err != nil || !ed25519.Verify(pub, api.SigningPayload(u.Package, u.Version, u.SHA256), sig) {
		return "", errors.New("release signature is invalid; refusing to install")
	}
	dir := filepath.Join(a.cfg.StateDir, "downloads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, filepath.Base(u.ReleaseID)+".part")
	dctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	if err := a.client.downloadTo(dctx, u.ArtifactPath, path, u.Size); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	sum, size, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if size != u.Size || sum != u.SHA256 {
		os.Remove(path) // corrupt; start over next time
		return "", fmt.Errorf("artifact checksum mismatch (got %s, %d bytes)", sum, size)
	}
	return path, nil
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// installPackage runs the configured install and health commands, rolling
// back on failure. It returns the final update status and a message.
func (a *Agent) installPackage(ctx context.Context, u api.UpdateInstruction, downloaded string) (string, string) {
	pc := a.cfg.Packages[u.Package]
	pkgDir := filepath.Join(a.cfg.StateDir, "packages", safeFileName(u.Package))
	if err := os.MkdirAll(pkgDir, 0o700); err != nil {
		return api.UpdateFailed, err.Error()
	}
	artifact := filepath.Join(pkgDir, safeFileName(u.Version))
	if err := os.Rename(downloaded, artifact); err != nil {
		return api.UpdateFailed, err.Error()
	}
	prev := a.state.get().Installed[u.Package]
	prevVersion := prev.Version
	if v := a.packageVersions(ctx)[u.Package]; v != "" {
		prevVersion = v
	}
	env := []string{
		"ARMADA_PACKAGE=" + u.Package,
		"ARMADA_VERSION=" + u.Version,
		"ARMADA_ARTIFACT=" + artifact,
		"ARMADA_PREVIOUS_VERSION=" + prevVersion,
		"ARMADA_PREVIOUS_ARTIFACT=" + prev.Artifact,
	}
	timeout := a.cfg.CommandTimeout.Duration

	fail := func(why string) (string, string) {
		if len(pc.RollbackCommand) == 0 {
			return api.UpdateFailed, why
		}
		if prevVersion == "" {
			return api.UpdateFailed, why + "; no previous version to roll back to"
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		out, code, err := runCommand(cctx, pc.RollbackCommand, env)
		if err != nil || code != 0 {
			return api.UpdateFailed, why + "; rollback also failed: " + describeRun(out, code, err)
		}
		return api.UpdateRolledBack, why + "; rolled back to " + orUnknown(prevVersion)
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	out, code, err := runCommand(cctx, pc.InstallCommand, env)
	cancel()
	if err != nil || code != 0 {
		return fail("install failed: " + describeRun(out, code, err))
	}
	if len(pc.HealthCommand) > 0 {
		var last string
		healthy := false
		for i := 0; i < pc.HealthRetries; i++ {
			if i > 0 {
				select {
				case <-ctx.Done():
					return api.UpdateFailed, "interrupted during health check"
				case <-time.After(pc.HealthInterval.Duration):
				}
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			out, code, err := runCommand(cctx, pc.HealthCommand, env)
			cancel()
			if err == nil && code == 0 {
				healthy = true
				break
			}
			last = describeRun(out, code, err)
		}
		if !healthy {
			return fail(fmt.Sprintf("health check failed after %d attempts: %s", pc.HealthRetries, last))
		}
	}
	_ = a.state.update(func(s *State) {
		old := s.Installed[u.Package]
		s.Installed[u.Package] = InstalledPackage{
			Version: u.Version, Artifact: artifact,
			PreviousVersion: prevVersion, PreviousArtifact: old.Artifact,
		}
		// Keep the current and previous artifacts for rollback scripts; drop older ones.
		if old.PreviousArtifact != "" && old.PreviousArtifact != artifact && old.PreviousArtifact != old.Artifact {
			os.Remove(old.PreviousArtifact)
		}
	})
	return api.UpdateSucceeded, "installed " + u.Version
}

func orUnknown(s string) string {
	if s == "" {
		return "previous version"
	}
	return s
}

func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == '+':
			return r
		}
		return '_'
	}, s)
}

// ---- self-update ----

// selfUpdate swaps in a new agent binary and restarts into it. The new
// process confirms the update after its first successful heartbeat; if it
// keeps failing to start, the previous binary is restored.
func (a *Agent) selfUpdate(ctx context.Context, u api.UpdateInstruction, downloaded string) error {
	exe := a.ExePath
	if exe == "" {
		return errors.New("cannot determine agent executable path")
	}
	newPath := exe + ".new"
	if err := copyFile(downloaded, newPath, 0o755); err != nil {
		return fmt.Errorf("stage new binary: %w", err)
	}
	os.Remove(downloaded)
	// Smoke test: the new binary must run on this platform and report the expected version.
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	out, code, err := runCommand(cctx, []string{newPath, "-version"}, nil)
	cancel()
	if err != nil || code != 0 {
		os.Remove(newPath)
		return fmt.Errorf("new agent binary failed to run: %s", describeRun(out, code, err))
	}
	if !strings.Contains(out, u.Version) {
		os.Remove(newPath)
		return fmt.Errorf("new agent binary reports version %q, expected %q", firstLine(out), u.Version)
	}
	backup := exe + ".prev"
	os.Remove(backup)
	if err := os.Rename(exe, backup); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("back up current binary: %w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		_ = os.Rename(backup, exe)
		return fmt.Errorf("install new binary: %w", err)
	}
	if err := a.state.update(func(s *State) {
		s.SelfUpdate = &PendingSelfUpdate{
			DeploymentID: u.DeploymentID, ReleaseID: u.ReleaseID, Version: u.Version,
			FromVersion: Version, BackupPath: backup, StartedAt: time.Now().UTC(),
		}
	}); err != nil {
		_ = os.Rename(backup, exe)
		return err
	}
	a.log.Info("restarting into new agent", "version", u.Version)
	if err := a.Reexec(exe); err != nil {
		// Couldn't restart: put the old binary back.
		_ = os.Rename(backup, exe)
		_ = a.state.update(func(s *State) { s.SelfUpdate = nil })
		return fmt.Errorf("restart into new agent: %w", err)
	}
	return nil
}

// checkSelfUpdateOnStart runs before the first heartbeat. It detects a
// crash-looping new binary and restores the previous one.
func (a *Agent) checkSelfUpdateOnStart() error {
	p := a.state.get().SelfUpdate
	if p == nil {
		return nil
	}
	if p.Version != Version {
		// We are not the version we tried to install: the swap was undone.
		msg := fmt.Sprintf("agent is running %s after attempting update to %s", Version, p.Version)
		return a.state.update(func(s *State) {
			s.SelfUpdate = nil
			s.Outbox = append(s.Outbox, api.UpdateReport{DeploymentID: p.DeploymentID, ReleaseID: p.ReleaseID, Status: api.UpdateRolledBack, Message: msg})
		})
	}
	p.Starts++
	if p.Starts <= maxSelfUpdateStarts {
		return a.state.update(func(s *State) { s.SelfUpdate = p })
	}
	a.log.Error("new agent failed to become healthy; restoring previous binary", "version", Version, "previous", p.FromVersion)
	if err := copyFile(p.BackupPath, a.ExePath+".rollback", 0o755); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	if err := os.Rename(a.ExePath+".rollback", a.ExePath); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	msg := fmt.Sprintf("agent %s did not complete a heartbeat after %d starts; rolled back to %s", Version, maxSelfUpdateStarts, p.FromVersion)
	if err := a.state.update(func(s *State) {
		s.SelfUpdate = nil
		s.Outbox = append(s.Outbox, api.UpdateReport{DeploymentID: p.DeploymentID, ReleaseID: p.ReleaseID, Status: api.UpdateRolledBack, Message: msg})
	}); err != nil {
		return err
	}
	return a.Reexec(a.ExePath)
}

// confirmSelfUpdate marks a pending self-update as successful once the new
// binary has completed a heartbeat.
func (a *Agent) confirmSelfUpdate() {
	p := a.state.get().SelfUpdate
	if p == nil || p.Version != Version {
		return
	}
	a.log.Info("self-update confirmed", "version", Version)
	_ = a.state.update(func(s *State) {
		s.SelfUpdate = nil
		s.Outbox = append(s.Outbox, api.UpdateReport{DeploymentID: p.DeploymentID, ReleaseID: p.ReleaseID, Status: api.UpdateSucceeded,
			Message: "agent updated from " + p.FromVersion})
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ---- actions ----

func (a *Agent) runAction(ctx context.Context, act api.Action) {
	result := func(status string, code int, out string) {
		if err := a.client.do(ctx, "POST", "/api/v1/agent/actions/"+act.ID+"/result",
			api.ActionResult{Status: status, ExitCode: code, Output: out}, nil); err != nil {
			a.log.Warn("action result not delivered", "action", act.ID, "err", err)
		}
	}
	a.log.Info("running action", "action", act.ID, "type", act.Type)
	switch act.Type {
	case api.ActionReboot:
		if !*a.cfg.AllowReboot {
			result(api.ActionRejected, -1, "reboot is disabled on this device (allow_reboot=false)")
			return
		}
		result(api.ActionSucceeded, 0, "rebooting")
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if out, code, err := runCommand(cctx, a.cfg.RebootCommand, nil); err != nil || code != 0 {
			a.log.Error("reboot command failed", "out", describeRun(out, code, err))
		}
	case api.ActionRestartAgent:
		result(api.ActionSucceeded, 0, "restarting agent")
		if err := a.Reexec(a.ExePath); err != nil {
			a.log.Error("restart failed", "err", err)
		}
	case api.ActionExec:
		if !a.cfg.AllowExec {
			result(api.ActionRejected, -1, "exec is disabled on this device (allow_exec=false)")
			return
		}
		cctx, cancel := context.WithTimeout(ctx, a.cfg.CommandTimeout.Duration)
		defer cancel()
		out, code, err := runCommand(cctx, act.Args, nil)
		if err != nil {
			result(api.ActionFailed, code, describeRun(out, code, err))
			return
		}
		status := api.ActionSucceeded
		if code != 0 {
			status = api.ActionFailed
		}
		result(status, code, out)
	default:
		result(api.ActionRejected, -1, "unsupported action type "+act.Type)
	}
}

// ---- command execution ----

const maxCommandOutput = 64 * 1024

// tailBuffer keeps the last maxCommandOutput bytes written to it.
type tailBuffer struct{ bytes.Buffer }

func (t *tailBuffer) Write(p []byte) (int, error) {
	n, _ := t.Buffer.Write(p)
	if over := t.Len() - maxCommandOutput; over > 0 {
		t.Next(over)
	}
	return n, nil
}

// runCommand runs argv without a shell, returning combined output and exit code.
// err is non-nil only if the command could not be run or timed out.
func runCommand(ctx context.Context, argv []string, extraEnv []string) (string, int, error) {
	if len(argv) == 0 {
		return "", -1, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), extraEnv...)
	var buf tailBuffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	out := buf.String()
	if ctx.Err() != nil {
		return out, -1, fmt.Errorf("timed out: %w", ctx.Err())
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out, ee.ExitCode(), nil
	}
	if err != nil {
		return out, -1, err
	}
	return out, 0, nil
}

func describeRun(out string, code int, err error) string {
	out = strings.TrimSpace(out)
	if len(out) > 1024 {
		out = "…" + out[len(out)-1024:]
	}
	var s string
	if err != nil {
		s = err.Error()
	} else {
		s = fmt.Sprintf("exit status %d", code)
	}
	if out != "" {
		s += ": " + out
	}
	return s
}
