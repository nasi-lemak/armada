package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func TestGzipCheckIns(t *testing.T) {
	_, ts := setup(t)
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok)
	b, _ := json.Marshal(api.EnrollRequest{Token: tok.Token, Name: "kiosk", Info: api.DeviceInfo{OS: "linux"}})
	res, err := http.Post(ts.URL+"/api/v1/agent/enroll", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var er api.EnrollResponse
	json.NewDecoder(res.Body).Decode(&er)
	res.Body.Close()
	if res.Header.Get(agentGzipHeader) != "gzip" {
		t.Error("enrollment doesn't say gzip is accepted")
	}
	post := func(body []byte, encoding string) *http.Response {
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/agent/heartbeat", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+er.DeviceKey)
		req.Header.Set("Content-Type", "application/json")
		if encoding != "" {
			req.Header.Set("Content-Encoding", encoding)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	hb, _ := json.Marshal(api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux", Hostname: "compressed-host"}, Packages: map[string]string{"kiosk": "2.0"}})
	if res := post(gz(hb), "gzip"); res.StatusCode != 200 || res.Header.Get(agentGzipHeader) != "" {
		t.Fatalf("gzip check-in: %d %v (the offer is only for plain bodies)", res.StatusCode, res.Header)
	}
	var d struct{ Device api.Device }
	call(t, ts, admin, "GET", "/api/v1/devices/"+er.DeviceID, nil, &d)
	if d.Device.Info.Hostname != "compressed-host" || d.Device.Packages["kiosk"] != "2.0" {
		t.Errorf("compressed check-in not stored: %+v", d.Device)
	}
	if res := post(hb, ""); res.StatusCode != 200 || res.Header.Get(agentGzipHeader) != "gzip" {
		t.Errorf("plain check-in: %d %v", res.StatusCode, res.Header)
	}
	if res := post([]byte("not gzip"), "gzip"); res.StatusCode != 400 {
		t.Errorf("garbage gzip: %d", res.StatusCode)
	}
	// A small body that expands past the limit is refused.
	bomb := gz([]byte(`{"info":{"hostname":"` + strings.Repeat("a", int(maxJSONBody)+10) + `"}}`))
	if len(bomb) > 100<<10 {
		t.Fatalf("bomb is %d bytes compressed", len(bomb))
	}
	if res := post(bomb, "gzip"); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("gzip bomb: %d", res.StatusCode)
	}
}
