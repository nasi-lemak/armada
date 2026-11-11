// Package rollout contains the pure decision logic for staged deployments:
// which devices a deployment targets, which of those are eligible in the
// current stage, and which update (if any) a device should be offered.
package rollout

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"strings"
	"unicode"

	"github.com/nasi-lemak/armada/internal/api"
)

// Bucket deterministically maps a device into [0,100) for a deployment.
// Salting with the deployment ID means each rollout canaries a different
// slice of the fleet instead of always hitting the same unlucky devices.
func Bucket(deploymentID, deviceID string) int {
	sum := sha256.Sum256([]byte(deploymentID + "/" + deviceID))
	return int(binary.BigEndian.Uint64(sum[:8]) % 100)
}

// InStage reports whether a device is eligible at the deployment's current stage.
func InStage(d *api.Deployment, deviceID string) bool {
	return Bucket(d.ID, deviceID) < d.CurrentPercent()
}

// Matches reports whether the selector targets the device.
func Matches(s api.Selector, dev *api.Device) bool {
	if s.Channel != "" && s.Channel != dev.Channel {
		return false
	}
	for k, v := range s.Labels {
		got, ok := dev.Labels[k]
		if !ok {
			return false
		}
		if v != "*" && got != v {
			return false
		}
	}
	if len(s.DeviceIDs) > 0 {
		found := false
		for _, id := range s.DeviceIDs {
			if id == dev.ID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// PlatformMatches reports whether a release can run on the device.
func PlatformMatches(r *api.Release, dev *api.Device) bool {
	if r.OS != "" && !strings.EqualFold(r.OS, dev.Info.OS) {
		return false
	}
	if r.Arch != "" && !strings.EqualFold(r.Arch, dev.Info.Arch) {
		return false
	}
	return true
}

// Candidate is a running or paused deployment together with its release and
// this device's existing progress record for it (if any).
type Candidate struct {
	Deployment *api.Deployment
	Release    *api.Release
	Progress   *api.DeviceUpdate
}

// Decision is the outcome of evaluating a device against the active deployments.
type Decision struct {
	// Offer is the candidate whose release should be sent to the device, if any.
	Offer *Candidate
	// Confirmed lists candidates the device has evidently completed (it already
	// runs the target version) whose progress should be marked succeeded.
	Confirmed []*Candidate
}

// Decide picks at most one update to offer a device.
//
// Candidates must be ordered newest first. For each package, the newest
// deployment that targets the device *and* includes it in its current stage
// "claims" the package; older deployments are not considered for it. A
// deployment that targets the device but has not reached it yet does not claim
// it, so an older, fully rolled out deployment can still bring a stale device
// forward. Without AllowDowngrade, an update is never offered if the device
// already runs a newer version, which prevents older deployments from undoing
// a newer canary.
func Decide(dev *api.Device, candidates []*Candidate, busy bool) Decision {
	var dec Decision
	claimed := map[string]bool{}
	for _, c := range candidates {
		d, r := c.Deployment, c.Release
		if claimed[d.Package] {
			continue
		}
		if !Matches(d.Selector, dev) || !PlatformMatches(r, dev) {
			continue
		}
		installed := dev.Packages[d.Package]
		if installed == r.Version {
			// Already there. Confirm success if an update was in flight, and
			// let this deployment claim the package so older ones stay away.
			// A reported failure stands even if the version changed: a failed
			// health check with nothing to roll back to leaves the new version
			// installed but broken.
			if c.Progress != nil && !api.IsTerminalUpdateStatus(c.Progress.Status) {
				dec.Confirmed = append(dec.Confirmed, c)
			}
			if InStage(d, dev.ID) || c.Progress != nil {
				claimed[d.Package] = true
			}
			continue
		}
		if !InStage(d, dev.ID) && c.Progress == nil {
			continue // not this device's turn yet; an older deployment may apply
		}
		claimed[d.Package] = true
		if d.Status != api.DeploymentRunning || busy || dec.Offer != nil {
			continue
		}
		if c.Progress != nil && (c.Progress.Status == api.UpdateFailed || c.Progress.Status == api.UpdateRolledBack) {
			continue // don't hammer a device that already failed this rollout
		}
		if c.Progress != nil && c.Progress.Status == api.UpdateSucceeded {
			// The agent installed it but reports a different version (e.g. a
			// version_command that prints something else). Don't loop forever.
			continue
		}
		if !d.Strategy.AllowDowngrade && installed != "" && Compare(r.Version, installed) < 0 {
			continue
		}
		dec.Offer = c
	}
	return dec
}

// Compare compares two version strings, returning -1, 0 or 1.
// It understands semver-ish versions ("v1.2.10" > "1.2.9", "1.0.0-rc1" < "1.0.0")
// and falls back to lexical comparison for non-numeric segments.
func Compare(a, b string) int {
	a, b = strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v")
	aMain, aPre, _ := strings.Cut(stripBuild(a), "-")
	bMain, bPre, _ := strings.Cut(stripBuild(b), "-")
	if c := compareDotted(aMain, bMain); c != 0 {
		return c
	}
	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		return 1 // release > prerelease
	case bPre == "":
		return -1
	}
	return compareDotted(aPre, bPre)
}

func stripBuild(v string) string {
	if i := strings.IndexByte(v, '+'); i >= 0 {
		return v[:i]
	}
	return v
}

func compareDotted(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if c := compareSegment(x, y); c != 0 {
			return c
		}
	}
	return 0
}

func compareSegment(x, y string) int {
	xn, xerr := strconv.ParseUint(x, 10, 64)
	yn, yerr := strconv.ParseUint(y, 10, 64)
	switch {
	case xerr == nil && yerr == nil:
		return cmp(xn, yn)
	case x == "" && y == "":
		return 0
	case x == "":
		return -1
	case y == "":
		return 1
	case xerr == nil:
		return -1 // numeric identifiers sort before alphanumeric ones
	case yerr == nil:
		return 1
	}
	// Both alphanumeric: compare leading text, then trailing number (rc2 < rc10).
	xp, xs := splitAlphaNum(x)
	yp, ys := splitAlphaNum(y)
	if xp != yp {
		return strings.Compare(xp, yp)
	}
	xi, _ := strconv.ParseUint(xs, 10, 64)
	yi, _ := strconv.ParseUint(ys, 10, 64)
	return cmp(xi, yi)
}

func splitAlphaNum(s string) (string, string) {
	i := strings.IndexFunc(s, unicode.IsDigit)
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

func cmp(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Evaluation is the result of checking a deployment's health and soak time.
type Evaluation struct {
	Pause   bool
	Reason  string
	Advance bool
}

// Evaluate decides whether a running deployment should pause (too many failures)
// or advance to its next stage (soak time elapsed). elapsedSeconds is the time
// spent in the current stage.
func Evaluate(d *api.Deployment, elapsedSeconds int64) Evaluation {
	s := d.Stats
	failures := s.Failed + s.RolledBack - d.AcknowledgedFailures
	if failures < 0 {
		failures = 0
	}
	finished := s.Succeeded + failures
	minSamples := d.Strategy.MinSamples
	if minSamples < 1 {
		minSamples = 1
	}
	if finished >= minSamples && d.Strategy.MaxFailureRate < 1 {
		rate := float64(failures) / float64(finished)
		if rate > d.Strategy.MaxFailureRate {
			return Evaluation{Pause: true, Reason: "failure rate " + pct(rate) + " exceeded limit of " + pct(d.Strategy.MaxFailureRate)}
		}
	}
	last := d.StageIndex >= len(d.Strategy.Stages)-1
	if !last && d.Strategy.AutoAdvance && elapsedSeconds >= d.Strategy.StageDurationSeconds {
		return Evaluation{Advance: true}
	}
	return Evaluation{}
}

func pct(f float64) string {
	return strconv.FormatFloat(f*100, 'f', 1, 64) + "%"
}

// ValidateStrategy normalises and checks a strategy.
func ValidateStrategy(s *api.Strategy) error {
	if len(s.Stages) == 0 {
		s.Stages = []int{100}
	}
	prev := 0
	for _, p := range s.Stages {
		if p <= prev || p > 100 {
			return errString("stages must be strictly increasing percentages between 1 and 100")
		}
		prev = p
	}
	if s.Stages[len(s.Stages)-1] != 100 {
		s.Stages = append(s.Stages, 100)
	}
	if s.MaxFailureRate < 0 || s.MaxFailureRate > 1 {
		return errString("max_failure_rate must be between 0 and 1")
	}
	if s.StageDurationSeconds < 0 {
		return errString("stage_duration_seconds must not be negative")
	}
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }
