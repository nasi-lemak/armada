package server

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestGzipResponses(t *testing.T) {
	_, ts := setup(t)
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": "acme"}}, &tok)
	for i := 0; i < 30; i++ {
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: "kiosk", Info: api.DeviceInfo{OS: "linux"}}, nil)
	}
	get := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+admin)
		req.Header.Set("Accept-Encoding", "gzip") // set by hand, so Go doesn't decompress for us
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := get("/api/v1/devices")
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("devices not compressed: %v", res.Header)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	var devs []api.Device
	if err := json.Unmarshal(plain, &devs); err != nil || len(devs) != 30 {
		t.Fatalf("decoded: %v %d", err, len(devs))
	}
	// A backup is already gzip: passed through as is.
	b := get("/api/v1/backup")
	defer b.Body.Close()
	if b.Header.Get("Content-Encoding") != "" || b.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("backup: %v", b.Header)
	}
	if _, err := gzip.NewReader(b.Body); err != nil {
		t.Fatalf("backup isn't a plain .tar.gz: %v", err)
	}
}
