package agent

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/certs"
)

// ManagedCert is a certificate the server may renew: the agent makes a new
// key and CSR on the device (the key never leaves it), and installs the
// signed certificate it gets back. Only certificates listed in the agent's
// config can be touched, and only at these paths.
type ManagedCert struct {
	Cert string `json:"cert"` // certificate file (PEM)
	Key  string `json:"key"`  // private key file (PEM)
	// Chain is a separate file for intermediate certificates, for services
	// that want one. FullChain instead writes the intermediates after the
	// certificate in Cert (what Mosquitto's certfile and nginx expect).
	Chain     string `json:"chain,omitempty"`
	FullChain bool   `json:"full_chain,omitempty"`
	// Owner ("user" or "user:group") owns the new key and certificate files,
	// for services that drop root (e.g. "mosquitto:mosquitto").
	Owner string `json:"owner,omitempty"`
	// ReloadCommand runs after a new certificate is written (e.g.
	// systemctl restart mosquitto). If it or the check below fails, the
	// previous files are restored and it runs again.
	ReloadCommand []string `json:"reload_command,omitempty"`
	// VerifyAddress (host:port) is checked after reloading: the service
	// there must present the new certificate.
	VerifyAddress string `json:"verify_address,omitempty"`
}

func (m ManagedCert) validate(name string) error {
	for _, p := range []string{m.Cert, m.Key} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("managed_certs %q: cert and key must be absolute paths", name)
		}
	}
	if m.Chain != "" && !filepath.IsAbs(m.Chain) {
		return fmt.Errorf("managed_certs %q: chain must be an absolute path", name)
	}
	if m.VerifyAddress != "" {
		if _, _, err := net.SplitHostPort(m.VerifyAddress); err != nil {
			return fmt.Errorf("managed_certs %q: verify_address must be host:port", name)
		}
	}
	return nil
}

// certReports describes every managed certificate.
func (a *Agent) certReports() []api.CertReport {
	if len(a.cfg.ManagedCerts) == 0 {
		return []api.CertReport{} // none: the server forgets any it knew
	}
	names := make([]string, 0, len(a.cfg.ManagedCerts))
	for n := range a.cfg.ManagedCerts {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]api.CertReport, 0, len(names))
	for _, n := range names {
		out = append(out, inspectCert(n, a.cfg.ManagedCerts[n]))
	}
	return out
}

func inspectCert(name string, m ManagedCert) api.CertReport {
	r := api.CertReport{Target: name}
	data, err := os.ReadFile(m.Cert)
	if errors.Is(err, os.ErrNotExist) {
		r.Error = "no certificate at " + m.Cert
		return r
	} else if err != nil {
		r.Error = err.Error()
		return r
	}
	cs, err := certs.ParseCerts(data)
	if err != nil || len(cs) == 0 {
		r.Error = "no readable certificate in " + m.Cert
		return r
	}
	info := certs.Describe(cs[0])
	if kd, err := os.ReadFile(m.Key); err == nil {
		if k, err := certs.ParseKey(kd); err == nil {
			info.KeyMatches = certs.PublicKeyID(k.Public()) == certs.PublicKeyID(cs[0].PublicKey)
		}
	}
	r.Info = &info
	return r
}

func (a *Agent) pendingKeyPath(target, renewal string) string {
	return filepath.Join(a.cfg.StateDir, "certs", target+"."+renewal+".key")
}

// certCSR makes a new key and CSR for a managed certificate. The key waits
// in the agent's state directory until the signed certificate arrives.
func (a *Agent) certCSR(spec api.CertCSRSpec) (string, error) {
	if _, ok := a.cfg.ManagedCerts[spec.Target]; !ok {
		return "", fmt.Errorf("no managed_certs entry named %q on this device", spec.Target)
	}
	if !safeName(spec.Target) || !safeName(spec.RenewalID) {
		return "", errors.New("invalid target or renewal id")
	}
	key, err := certs.GenerateKey(spec.KeyType)
	if err != nil {
		return "", err
	}
	csr, err := certs.MakeCSR(key, spec.Subject, spec.DNSNames, spec.IPAddresses)
	if err != nil {
		return "", err
	}
	pemKey, err := certs.EncodeKey(key)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(a.cfg.StateDir, "certs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Only the newest request for a target counts: drop older pending keys.
	old, _ := filepath.Glob(filepath.Join(dir, spec.Target+".*.key"))
	for _, p := range old {
		os.Remove(p)
	}
	if err := writeFileAtomic(a.pendingKeyPath(spec.Target, spec.RenewalID), pemKey, 0o600); err != nil {
		return "", err
	}
	return string(csr), nil
}

// certInstall installs a signed certificate with its pending key, reloads
// the service and checks it, putting the previous files back on failure.
func (a *Agent) certInstall(ctx context.Context, spec api.CertInstallSpec) (*api.CertInfo, error) {
	m, ok := a.cfg.ManagedCerts[spec.Target]
	if !ok {
		return nil, fmt.Errorf("no managed_certs entry named %q on this device", spec.Target)
	}
	if !safeName(spec.Target) || !safeName(spec.RenewalID) {
		return nil, errors.New("invalid target or renewal id")
	}
	keyPath := a.pendingKeyPath(spec.Target, spec.RenewalID)
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, errors.New("the key for this request isn't on the device any more (a newer CSR replaced it?); request a new CSR")
	}
	key, err := certs.ParseKey(keyPEM)
	if err != nil {
		return nil, err
	}
	cs, err := certs.ParseCerts([]byte(spec.Certificate))
	if err != nil || len(cs) == 0 {
		return nil, errors.New("no certificate to install")
	}
	leaf := cs[0]
	if certs.PublicKeyID(leaf.PublicKey) != certs.PublicKeyID(key.Public()) {
		return nil, errors.New("the certificate was not issued for this device's key")
	}
	chain, err := certs.ParseCerts([]byte(spec.Chain))
	if err != nil {
		return nil, err
	}
	chain = append(cs[1:], chain...)

	certPEM := certs.EncodeCerts(leaf)
	if m.FullChain {
		certPEM = certs.EncodeCerts(append([]*x509.Certificate{leaf}, chain...)...)
	}
	files := []struct {
		path string
		data []byte
		mode fs.FileMode
	}{{m.Key, keyPEM, 0o600}, {m.Cert, certPEM, 0o644}}
	if m.Chain != "" && len(chain) > 0 {
		files = append(files, struct {
			path string
			data []byte
			mode fs.FileMode
		}{m.Chain, certs.EncodeCerts(chain...), 0o644})
	}

	uid, gid := -1, -1
	if m.Owner != "" {
		if uid, gid, err = lookupOwner(m.Owner); err != nil {
			return nil, err
		}
	}
	// Keep what's there now, to put back if the new certificate doesn't work.
	type saved struct {
		path   string
		data   []byte
		mode   fs.FileMode
		exists bool
	}
	var backups []saved
	for i, f := range files {
		b := saved{path: f.path}
		if st, err := os.Stat(f.path); err == nil {
			b.exists, b.mode = true, st.Mode().Perm()
			if b.data, err = os.ReadFile(f.path); err != nil {
				return nil, err
			}
			files[i].mode = b.mode // keep the existing permissions
		}
		backups = append(backups, b)
	}
	restore := func() {
		for _, b := range backups {
			if b.exists {
				writeFileAtomic(b.path, b.data, b.mode)
				if uid >= 0 {
					os.Chown(b.path, uid, gid)
				}
			} else {
				os.Remove(b.path)
			}
		}
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			restore()
			return nil, err
		}
		if err := writeFileAtomic(f.path, f.data, f.mode); err != nil {
			restore()
			return nil, err
		}
		if uid >= 0 {
			if err := os.Chown(f.path, uid, gid); err != nil {
				restore()
				return nil, err
			}
		}
	}
	check := func() error {
		if err := a.reloadCert(ctx, spec.Target, m); err != nil {
			return err
		}
		return verifyServing(ctx, m.VerifyAddress, leaf)
	}
	if err := check(); err != nil {
		restore()
		if rerr := a.reloadCert(ctx, spec.Target, m); rerr != nil {
			return nil, fmt.Errorf("%v; previous certificate restored, but reloading it also failed: %v", err, rerr)
		}
		return nil, fmt.Errorf("%v; previous certificate restored", err)
	}
	os.Remove(keyPath)
	info := certs.Describe(leaf)
	info.KeyMatches = true
	return &info, nil
}

func (a *Agent) reloadCert(ctx context.Context, target string, m ManagedCert) error {
	if len(m.ReloadCommand) == 0 {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, code, err := runCommand(cctx, m.ReloadCommand, []string{"ARMADA_CERT_TARGET=" + target, "ARMADA_CERT_FILE=" + m.Cert, "ARMADA_KEY_FILE=" + m.Key})
	if err != nil || code != 0 {
		return fmt.Errorf("reload command failed: %s", describeRun(out, code, err))
	}
	return nil
}

// certVerifyTimeout is how long a reloaded service has to present the new certificate.
var certVerifyTimeout = 20 * time.Second

// verifyServing checks the service at addr presents want, retrying while
// it restarts.
func verifyServing(ctx context.Context, addr string, want *x509.Certificate) error {
	if addr == "" {
		return nil
	}
	sum := sha256.Sum256(want.Raw)
	wantFP := hex.EncodeToString(sum[:])
	deadline := time.Now().Add(certVerifyTimeout)
	var last error
	for {
		d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 3 * time.Second},
			// Only the certificate presented matters here, not whether this
			// device trusts its issuer.
			Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			peer := conn.(*tls.Conn).ConnectionState().PeerCertificates
			conn.Close()
			if len(peer) > 0 {
				s := sha256.Sum256(peer[0].Raw)
				if hex.EncodeToString(s[:]) == wantFP {
					return nil
				}
				last = fmt.Errorf("%s still presents the old certificate (serial %s)", addr, strings.ToUpper(peer[0].SerialNumber.Text(16)))
			} else {
				last = fmt.Errorf("%s presented no certificate", addr)
			}
		} else {
			last = fmt.Errorf("can't reach %s over TLS: %v", addr, err)
		}
		if time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(time.Second):
		}
	}
}

func lookupOwner(owner string) (int, int, error) {
	name, group, _ := strings.Cut(owner, ":")
	u, err := user.Lookup(name)
	if err != nil {
		return -1, -1, fmt.Errorf("owner %q: %w", owner, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return -1, -1, fmt.Errorf("owner %q: %w", owner, err)
		}
		gid, _ = strconv.Atoi(g.Gid)
	}
	return uid, gid, nil
}

func safeName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// runCertAction handles the certificate actions.
func (a *Agent) runCertAction(ctx context.Context, act api.Action) (status, out string) {
	if len(act.Args) != 1 {
		return api.ActionFailed, "malformed certificate action"
	}
	switch act.Type {
	case api.ActionCertCSR:
		var spec api.CertCSRSpec
		if err := json.Unmarshal([]byte(act.Args[0]), &spec); err != nil {
			return api.ActionFailed, "malformed CSR request"
		}
		csr, err := a.certCSR(spec)
		if err != nil {
			return api.ActionFailed, err.Error()
		}
		return api.ActionSucceeded, csr
	case api.ActionCertInstall:
		var spec api.CertInstallSpec
		if err := json.Unmarshal([]byte(act.Args[0]), &spec); err != nil {
			return api.ActionFailed, "malformed install request"
		}
		info, err := a.certInstall(ctx, spec)
		if err != nil {
			return api.ActionFailed, err.Error()
		}
		b, _ := json.Marshal(info)
		return api.ActionSucceeded, string(b)
	}
	return api.ActionRejected, "unsupported action type " + act.Type
}
