package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
)

// ParseTrustedProxies reads a comma-separated list of IPs and CIDRs.
func ParseTrustedProxies(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			if ip := net.ParseIP(p); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				p = fmt.Sprintf("%s/%d", p, bits)
			}
		}
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *Server) trusted(ip net.IP) bool {
	for _, n := range s.cfg.TrustedProxies {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// fromTrustedProxy reports whether the request came through a configured
// reverse proxy, whose forwarding headers can be believed.
func (s *Server) fromTrustedProxy(r *http.Request) bool {
	return s.trusted(net.ParseIP(remoteIP(r)))
}

var warnForwarded sync.Once

// clientIP is the address a request really came from. Behind a trusted
// reverse proxy that is the right-most X-Forwarded-For entry that isn't
// itself a trusted proxy; otherwise the connection's address (headers from
// untrusted peers are ignored, as anyone can send them).
func (s *Server) clientIP(r *http.Request) string {
	remote := remoteIP(r)
	xff := r.Header.Get("X-Forwarded-For")
	if !s.trusted(net.ParseIP(remote)) {
		if xff != "" && len(s.cfg.TrustedProxies) == 0 {
			warnForwarded.Do(func() {
				s.log.Warn("requests arrive with X-Forwarded-For but -trusted-proxies is not set: everyone behind the proxy shares one address for sign-in throttling", "proxy", remote)
			})
		}
		return remote
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			break
		}
		if !s.trusted(ip) {
			return ip.String()
		}
	}
	return remote
}
