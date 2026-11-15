package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

// netCheck tests the route from this device to the server, step by step,
// and reports what it found. ok is false at the first step that fails.
func (a *Agent) netCheck(ctx context.Context) (report string, ok bool) {
	var b strings.Builder
	line := func(label, format string, args ...any) {
		fmt.Fprintf(&b, "%-11s %s\n", label, fmt.Sprintf(format, args...))
	}
	host, _ := os.Hostname()
	fmt.Fprintf(&b, "Network check from %s at %s\n\n", host, time.Now().UTC().Format(time.RFC3339))
	server, err := url.Parse(a.cfg.ServerURL)
	if err != nil || server.Host == "" {
		line("Server", "invalid server URL %q", a.cfg.ServerURL)
		return b.String(), false
	}
	line("Server", "%s", server.Redacted())
	port := server.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[server.Scheme]
	}

	tr, _ := a.client.api.Transport.(*http.Transport)
	var tlsConf *tls.Config
	if tr != nil && tr.TLSClientConfig != nil {
		tlsConf = tr.TLSClientConfig.Clone()
	} else {
		tlsConf = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tlsConf.ServerName = server.Hostname()

	// Interfaces and gateway first: they explain a lot when the rest fails.
	line("Interfaces", "%s", interfacesText())
	if gw := defaultGateway(); gw != "" {
		line("Gateway", "%s · %s", gw, pingText(ctx, gw))
	}

	// Proxy.
	proxied := ""
	if tr != nil && tr.Proxy != nil {
		req, _ := http.NewRequest("GET", a.cfg.ServerURL, nil)
		if pu, err := tr.Proxy(req); err == nil && pu != nil {
			proxied = pu.Host
			line("Proxy", "%s (from the environment)", pu.Redacted())
		}
	}
	if proxied == "" {
		line("Proxy", "none: connecting directly")
	}

	// DNS (the device's own, even when a proxy resolves for it).
	t0 := time.Now()
	addrs, err := net.DefaultResolver.LookupHost(ctx, server.Hostname())
	switch {
	case err != nil && proxied == "":
		line("DNS", "FAILED: %s (%s)", server.Hostname(), errText(err))
		return b.String() + "\nResult: the device can't resolve the server's name. Check its DNS settings.\n", false
	case err != nil:
		line("DNS", "%s does not resolve locally (%s); the proxy resolves it", server.Hostname(), errText(err))
	default:
		line("DNS", "%s → %s (%s)", server.Hostname(), strings.Join(firstN(addrs, 4), ", "), ms(time.Since(t0)))
	}

	// TCP to the server (or the proxy).
	target := net.JoinHostPort(server.Hostname(), port)
	what := "server"
	if proxied != "" {
		target, what = proxied, "proxy"
		if _, _, err := net.SplitHostPort(target); err != nil {
			target = net.JoinHostPort(target, "80")
		}
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	t0 = time.Now()
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		line("TCP", "FAILED: %s %s (%s)", what, target, errText(err))
		return b.String() + fmt.Sprintf("\nResult: the device can't open a connection to the %s. A firewall, route or the %s itself may be down.\n", what, what), false
	}
	line("TCP", "%s %s connected in %s (from %s)", what, target, ms(time.Since(t0)), conn.LocalAddr())
	conn.Close()

	// TLS straight to the server (skipped through a proxy: it tunnels).
	if server.Scheme == "https" && proxied == "" {
		t0 = time.Now()
		tc, err := tls.DialWithDialer(&d, "tcp", net.JoinHostPort(server.Hostname(), port), tlsConf)
		if err != nil {
			line("TLS", "FAILED: %s", errText(err))
			hint := "the TLS handshake failed."
			var ue x509.UnknownAuthorityError
			if errors.As(err, &ue) {
				hint = "the device doesn't trust the server's certificate. With a private CA, set ca_cert in the agent's config."
			}
			return b.String() + "\nResult: " + hint + "\n", false
		}
		st := tc.ConnectionState()
		tc.Close()
		if len(st.PeerCertificates) > 0 {
			c := st.PeerCertificates[0]
			days := int(time.Until(c.NotAfter).Hours() / 24)
			name := c.Subject.CommonName
			if name == "" && len(c.DNSNames) > 0 {
				name = c.DNSNames[0]
			}
			line("TLS", "handshake %s · %s · %s · %s · expires %s (%d days) · trusted",
				ms(time.Since(t0)), tls.VersionName(st.Version), tls.CipherSuiteName(st.CipherSuite), name,
				c.NotAfter.UTC().Format("2006-01-02"), days)
		}
	}

	// HTTPS round trips through the agent's own client (proxy, CA and all).
	var times []time.Duration
	var lastErr error
	status := 0
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(a.cfg.ServerURL, "/")+"/healthz", nil)
		t0 = time.Now()
		res, err := a.client.api.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		res.Body.Close()
		status = res.StatusCode
		times = append(times, time.Since(t0))
	}
	if len(times) == 0 {
		line(strings.ToUpper(server.Scheme), "FAILED: %s", errText(lastErr))
		return b.String() + "\nResult: the connection opens but requests to the server fail.\n", false
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	var parts []string
	for _, t := range times {
		parts = append(parts, ms(t))
	}
	line(strings.ToUpper(server.Scheme), "/healthz %d in %s (%d of 3 tries: %s)", status, ms(times[len(times)/2]), len(times), strings.Join(parts, ", "))
	if status != http.StatusOK {
		return b.String() + fmt.Sprintf("\nResult: the server answered %d. Something in between (a proxy or load balancer) may be misrouting.\n", status), false
	}
	if len(times) < 3 {
		return b.String() + "\nResult: the server is reachable, but some requests failed: the connection is unreliable.\n", true
	}
	return b.String() + "\nResult: OK. The server is reachable.\n", true
}

func errText(err error) string {
	if err == nil {
		return "unknown error"
	}
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return "no such host"
	case errors.As(err, &dnsErr) && dnsErr.IsTimeout:
		return "DNS timeout"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "i/o timeout"):
		return "timed out"
	case strings.Contains(err.Error(), "connection refused"):
		return "connection refused"
	case strings.Contains(err.Error(), "no route to host"), strings.Contains(err.Error(), "network is unreachable"):
		return "no route to the network"
	}
	return err.Error()
}

func ms(d time.Duration) string {
	if d < time.Millisecond {
		return "<1 ms"
	}
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], fmt.Sprintf("… (%d more)", len(s)-n))
	}
	return s
}

// interfacesText lists the interfaces that are up, with their addresses.
func interfacesText() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	var out []string
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 || i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := i.Addrs()
		var as []string
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				as = append(as, a.String())
			}
		}
		if len(as) == 0 {
			continue
		}
		out = append(out, i.Name+" "+strings.Join(as, " "))
	}
	if len(out) == 0 {
		return "no interface with an address is up"
	}
	return strings.Join(out, " · ")
}

// defaultGateway reads the IPv4 default route (Linux only; "" elsewhere).
func defaultGateway() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		g, err := hex.DecodeString(fields[2])
		if err != nil || len(g) != 4 {
			continue
		}
		return net.IPv4(g[3], g[2], g[1], g[0]).String() // little-endian
	}
	return ""
}

// pingText pings an address three times with the system ping, if there is one.
func pingText(ctx context.Context, addr string) string {
	path, err := exec.LookPath("ping")
	if err != nil {
		return "not pinged (no ping command)"
	}
	args := []string{"-c", "3", "-W", "1", addr}
	if runtime.GOOS == "windows" {
		args = []string{"-n", "3", "-w", "1000", addr}
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, args...).CombinedOutput()
	text := string(out)
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if strings.Contains(l, "packet loss") || strings.Contains(l, "Lost =") {
			return "ping: " + l
		}
	}
	if err != nil {
		return "ping failed: " + errText(err)
	}
	return "ping: no summary"
}
