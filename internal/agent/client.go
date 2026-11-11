package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// HTTPError is a non-2xx response from the server.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("server returned %d: %s", e.Status, e.Message) }

// IsStatus reports whether err is an HTTPError with the given status.
func IsStatus(err error, status int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == status
}

type client struct {
	base     string
	key      func() string
	api      *http.Client
	download *http.Client
}

func newClient(cfg *Config, key func() string) (*client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("read ca_cert: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("ca_cert contains no certificates")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &client{
		base:     cfg.ServerURL,
		key:      key,
		api:      &http.Client{Transport: tr, Timeout: 30 * time.Second},
		download: &http.Client{Transport: tr}, // bounded by context instead
	}, nil
}

func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if k := c.key(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	req.Header.Set("User-Agent", "armada-agent/"+Version)
	resp, err := c.api.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return readHTTPError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func readHTTPError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var er api.ErrorResponse
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &er) == nil && er.Error != "" {
		msg = er.Error
	}
	return &HTTPError{Status: resp.StatusCode, Message: msg}
}

// downloadTo fetches path into dst, resuming a partial download if dst
// already holds a prefix of the file.
func (c *client) downloadTo(ctx context.Context, path, dst string, size int64) error {
	var have int64
	if st, err := os.Stat(dst); err == nil {
		have = st.Size()
	}
	if size > 0 && have == size {
		return nil
	}
	if size > 0 && have > size {
		have = 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key())
	req.Header.Set("User-Agent", "armada-agent/"+Version)
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := c.download.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_WRONLY | os.O_CREATE
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		flags |= os.O_TRUNC
	default:
		return readHTTPError(resp)
	}
	f, err := os.OpenFile(dst, flags, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
