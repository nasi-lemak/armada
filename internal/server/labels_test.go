package server

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestQRPathMatchesBitmap(t *testing.T) {
	text := "https://fleet.example/#/devices/dev_abc123"
	path, size, err := qrPath(text)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := qrcode.New(text, qrcode.Medium)
	q.DisableBorder = true
	bm := q.Bitmap()
	if size != len(bm) {
		t.Fatalf("size %d, bitmap %d", size, len(bm))
	}
	// Redraw the path and compare cell by cell.
	got := make([][]bool, size)
	for i := range got {
		got[i] = make([]bool, size)
	}
	for _, m := range regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`).FindAllStringSubmatch(path, -1) {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		n, _ := strconv.Atoi(m[3])
		for i := 0; i < n; i++ {
			got[y][x+i] = true
		}
	}
	for y := range bm {
		for x := range bm[y] {
			if got[y][x] != bm[y][x] {
				t.Fatalf("cell %d,%d differs", x, y)
			}
		}
	}
}

func TestAssetLabels(t *testing.T) {
	_, ts := setup(t)
	enroll := func(name, customer, serial string) string {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": customer}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux", Serial: serial}}, &er)
		return er.DeviceID
	}
	a1, g1 := enroll("acme-1", "acme", "SN-1"), enroll("globex-1", "globex", "SN-2")
	call(t, ts, admin, "PATCH", "/api/v1/devices/"+a1, map[string]any{"inventory": api.Inventory{AssetTag: "ACME-00417"}}, nil)

	var ls []api.AssetLabel
	if code := call(t, ts, admin, "GET", "/api/v1/labels?ids="+a1+","+g1+",dev_missing", nil, &ls); code != 200 || len(ls) != 2 {
		t.Fatalf("labels: %d %+v", code, ls)
	}
	if l := ls[0]; l.Name != "acme-1" || l.Serial != "SN-1" || l.AssetTag != "ACME-00417" || !strings.HasSuffix(l.URL, "/#/devices/"+a1) || l.QRSize < 21 || l.QR == "" {
		t.Fatalf("label: %+v", l)
	}
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleViewer,
		Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-password-1"}, &lr)
	call(t, ts, lr.Token, "GET", "/api/v1/labels?ids="+a1+","+g1, nil, &ls)
	if len(ls) != 1 || ls[0].DeviceID != a1 {
		t.Fatalf("client labels: %+v", ls)
	}
	if code := call(t, ts, admin, "GET", "/api/v1/labels", nil, nil); code != 400 {
		t.Fatalf("no ids: %d", code)
	}
}
