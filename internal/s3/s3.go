// Package s3 is a small client for S3-compatible object storage (AWS S3,
// MinIO, Wasabi, Backblaze B2, ...): put, list and delete objects under a
// prefix, signed with AWS Signature Version 4. It uses path-style URLs:
// https://host/bucket/prefix/.
package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Client talks to one bucket and prefix.
type Client struct {
	endpoint  *url.URL // scheme://host
	bucket    string
	prefix    string // "" or ending in "/"
	region    string
	accessKey string
	secretKey string
	HTTP      *http.Client
	Now       func() time.Time
}

// New parses a path-style URL such as https://s3.eu-west-1.amazonaws.com/my-bucket/armada/.
func New(rawURL, region, accessKey, secretKey string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("S3 URL must look like https://host/bucket/prefix/")
	}
	parts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 2)
	if parts[0] == "" {
		return nil, errors.New("S3 URL must name a bucket: https://host/bucket/prefix/")
	}
	if accessKey == "" || secretKey == "" {
		return nil, errors.New("S3 needs an access key and a secret key")
	}
	if region == "" {
		region = "us-east-1"
	}
	c := &Client{endpoint: &url.URL{Scheme: u.Scheme, Host: u.Host}, bucket: parts[0], region: region,
		accessKey: accessKey, secretKey: secretKey, HTTP: &http.Client{Timeout: 30 * time.Minute}, Now: time.Now}
	if len(parts) == 2 && parts[1] != "" {
		c.prefix = strings.TrimSuffix(parts[1], "/") + "/"
	}
	return c, nil
}

// String describes where objects go, without credentials.
func (c *Client) String() string {
	return c.endpoint.String() + "/" + c.bucket + "/" + c.prefix
}

// Object is one listed object (key relative to the prefix).
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Put uploads body (its SHA-256 given as hex) as prefix+name.
func (c *Client) Put(ctx context.Context, name string, body io.Reader, size int64, sha256Hex string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.objectURL(name), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/gzip")
	c.sign(req, sha256Hex)
	return c.do(req, nil)
}

// Delete removes prefix+name.
func (c *Client) Delete(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.objectURL(name), nil)
	if err != nil {
		return err
	}
	c.sign(req, emptySHA256)
	return c.do(req, nil)
}

// List returns the objects under the prefix (up to 1000).
func (c *Client) List(ctx context.Context) ([]Object, error) {
	u := *c.endpoint
	u.Path = "/" + c.bucket + "/"
	u.RawQuery = url.Values{"list-type": {"2"}, "prefix": {c.prefix}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	c.sign(req, emptySHA256)
	var res struct {
		Contents []struct {
			Key          string
			Size         int64
			LastModified time.Time
		}
	}
	if err := c.do(req, &res); err != nil {
		return nil, err
	}
	out := make([]Object, 0, len(res.Contents))
	for _, o := range res.Contents {
		out = append(out, Object{Key: strings.TrimPrefix(o.Key, c.prefix), Size: o.Size, LastModified: o.LastModified})
	}
	return out, nil
}

func (c *Client) objectURL(name string) string {
	u := *c.endpoint
	u.Path = "/" + c.bucket + "/" + c.prefix + name
	return u.String()
}

func (c *Client) do(req *http.Request, out any) error {
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		var e struct {
			Code    string
			Message string
		}
		if xml.Unmarshal(b, &e) == nil && e.Code != "" {
			return fmt.Errorf("S3 %s: %s (HTTP %d)", e.Code, e.Message, res.StatusCode)
		}
		return fmt.Errorf("S3 HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return xml.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign adds AWS Signature Version 4 headers to req.
func (c *Client) sign(req *http.Request, payloadHash string) {
	now := c.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", authorization(req, payloadHash, amzDate, c.region, c.accessKey, c.secretKey))
}

// authorization computes the Authorization header for a request whose
// x-amz-* headers are already set. Every header on the request is signed,
// plus host.
func authorization(req *http.Request, payloadHash, amzDate, region, accessKey, secretKey string) string {
	date := amzDate[:8]
	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "user-agent" || lk == "content-length" {
			continue
		}
		headers[lk] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{req.Method, canonicalURI(req.URL.EscapedPath()), canonicalQuery(req.URL.Query()),
		canonHeaders.String(), signed, payloadHash}, "\n")
	scope := date + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := hmacSHA256([]byte("AWS4"+secretKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	return "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// canonicalURI encodes each path segment as S3 expects (RFC 3986).
func canonicalURI(escaped string) string {
	if escaped == "" {
		return "/"
	}
	segs := strings.Split(escaped, "/")
	for i, s := range segs {
		if u, err := url.PathUnescape(s); err == nil {
			segs[i] = uriEncode(u)
		}
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
