package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// Client accounts can configure webhook URLs, so the server must not become
// a way to reach its own network (SSRF). Their requests only go to public
// addresses, checked on the resolved IP at connect time (so DNS tricks and
// redirects don't get around it), never through a proxy.

var nonPublic = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
		"::/128", "::1/128", "64:ff9b::/96", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// publicIP reports whether an address is on the public internet.
func publicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range nonPublic {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

var errNonPublic = errors.New("client webhooks can only reach public internet addresses")

func (s *Server) newClientNotifyClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		if s.allowPrivateClientTargets {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if !publicIP(net.ParseIP(host)) {
			return fmt.Errorf("%w (refused %s)", errNonPublic, host)
		}
		return nil
	}}
	tr := &http.Transport{
		Proxy:                 nil, // connect directly, so the address check applies
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && !s.allowPrivateClientTargets {
				return errors.New("client webhooks may only redirect to https")
			}
			return nil
		}}
}

type clientChannelKey struct{}

func withClientChannel(ctx context.Context) context.Context {
	return context.WithValue(ctx, clientChannelKey{}, true)
}

func isClientChannel(ctx context.Context) bool {
	v, _ := ctx.Value(clientChannelKey{}).(bool)
	return v
}
