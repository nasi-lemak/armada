package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/certs"
)

// The server's own certificate is the one thing whose expiry takes the
// whole fleet down at once: every agent stops checking in. So:
//   - with -tls-cert/-tls-key, the files are re-read when they change (or
//     on SIGHUP), so a renewal (certbot, …) needs no restart;
//   - behind a reverse proxy, the certificate -public-url presents is
//     checked instead;
//   - either way, its expiry shows on the dashboard, is logged daily, and
//     can alert (condition server_cert_expiry).

const (
	serverCertWarnDays   = 21
	serverCertProbeEvery = time.Hour
)

// ServingCert holds the server's TLS certificate and reloads it when its
// files change.
type ServingCert struct {
	certFile, keyFile string
	log               *slog.Logger

	mu          sync.RWMutex
	cert        *tls.Certificate
	leaf        *x509.Certificate
	certMod     time.Time
	keyMod      time.Time
	lastFailure string
}

// LoadServingCert reads a certificate and key; they must match.
func LoadServingCert(certFile, keyFile string, log *slog.Logger) (*ServingCert, error) {
	c := &ServingCert{certFile: certFile, keyFile: keyFile, log: log}
	if err := c.Reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// GetCertificate is for tls.Config.
func (c *ServingCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cert, nil
}

// Leaf is the certificate being served.
func (c *ServingCert) Leaf() *x509.Certificate {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.leaf
}

// Reload reads the files again. If they don't make a valid pair (say the
// certificate was replaced and the key not yet), the current certificate
// stays in use.
func (c *ServingCert) Reload() error {
	cs, err := os.Stat(c.certFile)
	if err != nil {
		return err
	}
	ks, err := os.Stat(c.keyFile)
	if err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return fmt.Errorf("TLS certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("TLS certificate: %w", err)
	}
	c.mu.Lock()
	old := c.leaf
	c.cert, c.leaf, c.certMod, c.keyMod, c.lastFailure = &pair, leaf, cs.ModTime(), ks.ModTime(), ""
	c.mu.Unlock()
	if old != nil && !bytes.Equal(old.Raw, leaf.Raw) && c.log != nil {
		c.log.Info("TLS certificate reloaded", "subject", leaf.Subject.String(), "not_after", leaf.NotAfter.Format(time.RFC3339))
	}
	return nil
}

// changed reports whether either file was modified since the last load.
func (c *ServingCert) changed() bool {
	cs, err1 := os.Stat(c.certFile)
	ks, err2 := os.Stat(c.keyFile)
	if err1 != nil || err2 != nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return !cs.ModTime().Equal(c.certMod) || !ks.ModTime().Equal(c.keyMod)
}

// Watch reloads the certificate whenever its files change, until ctx ends.
func (c *ServingCert) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !c.changed() {
			continue
		}
		if err := c.Reload(); err != nil {
			c.mu.Lock()
			repeat := c.lastFailure == err.Error()
			c.lastFailure = err.Error()
			c.mu.Unlock()
			if !repeat && c.log != nil {
				c.log.Warn("TLS certificate files changed but can't be loaded yet; still serving the previous certificate", "err", err)
			}
		}
	}
}

// ---- status and checks ----

// serverCertState is what the server knows about its certificate.
type serverCertState struct {
	mu        sync.Mutex
	probed    time.Time
	probeInfo *api.CertInfo
	probeErr  string
	logged    time.Time
}

// ServerCertStatus is the server certificate's state, for fleet-wide accounts.
func (s *Server) serverCertStatus(ctx context.Context) *api.ServerCertStatus {
	now := s.now()
	st := &api.ServerCertStatus{}
	switch {
	case s.cfg.ServingCert != nil:
		leaf := s.cfg.ServingCert.Leaf()
		info := certs.Describe(leaf)
		info.KeyMatches = true // loaded as a matching pair
		st.Source, st.Certificate = "tls-cert", &info
	case s.cfg.CheckPublicCert && s.publicHTTPS() != "":
		st.Source = "public-url"
		st.Certificate, st.Error = s.probePublicCert(ctx, now)
	default:
		return nil // plain HTTP, no public URL: nothing to check
	}
	if st.Certificate != nil {
		st.DaysLeft = daysLeft(st.Certificate, now)
		st.Warning = *st.DaysLeft <= serverCertWarnDays
	}
	return st
}

func (s *Server) publicHTTPS() string {
	u, err := url.Parse(s.cfg.Android.PublicURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// probePublicCert reads the certificate the public URL presents (what
// devices see, through any proxy in front of the server), at most hourly.
func (s *Server) probePublicCert(ctx context.Context, now time.Time) (*api.CertInfo, string) {
	st := &s.serverCert
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.probed.IsZero() && now.Sub(st.probed) < serverCertProbeEvery {
		return st.probeInfo, st.probeErr
	}
	st.probed = now
	addr := s.publicHTTPS()
	host, _, _ := net.SplitHostPort(addr)
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	d := tls.Dialer{Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: s.serverCertRoots}}
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		var verr *tls.CertificateVerificationError
		if errors.As(err, &verr) && len(verr.UnverifiedCertificates) > 0 {
			info := certs.Describe(verr.UnverifiedCertificates[0])
			st.probeInfo, st.probeErr = &info, "devices may not trust it: "+verr.Err.Error()
		} else {
			st.probeInfo, st.probeErr = nil, "couldn't check "+addr+": "+err.Error()
		}
		return st.probeInfo, st.probeErr
	}
	defer conn.Close()
	peer := conn.(*tls.Conn).ConnectionState().PeerCertificates
	info := certs.Describe(peer[0])
	st.probeInfo, st.probeErr = &info, ""
	return st.probeInfo, ""
}

// serverCertTick logs a warning daily while the certificate is near expiry.
func (s *Server) serverCertTick(ctx context.Context, now time.Time) {
	st := s.serverCertStatus(ctx)
	if st == nil || (!st.Warning && st.Error == "") {
		return
	}
	s.serverCert.mu.Lock()
	due := now.Sub(s.serverCert.logged) >= 24*time.Hour
	if due {
		s.serverCert.logged = now
	}
	s.serverCert.mu.Unlock()
	if !due {
		return
	}
	if st.Certificate != nil && st.Warning {
		s.log.Warn("the server's TLS certificate expires soon; renew it, or every device stops checking in",
			"not_after", st.Certificate.NotAfter.Format(time.RFC3339), "days_left", *st.DaysLeft, "source", st.Source)
	}
	if st.Error != "" {
		s.log.Warn("server TLS certificate check", "problem", st.Error)
	}
}
