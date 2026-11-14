package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func importCSV(t *testing.T, ts *httptest.Server, token, csvText, query string) (int, api.ImportResult) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/registrations/import"+query, strings.NewReader(csvText))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/csv")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var out api.ImportResult
	if res.StatusCode == 200 {
		if err := json.NewDecoder(bytes.NewReader(b)).Decode(&out); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Logf("import %d: %s", res.StatusCode, b)
	}
	return res.StatusCode, out
}

func TestPreRegistrationAndImport(t *testing.T) {
	_, ts := setup(t)
	tokens := map[string]string{}
	enroll := func(name, customer, serial string) string {
		t.Helper()
		if tokens[customer] == "" {
			var tok api.EnrollmentToken
			call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": customer}}, &tok)
			tokens[customer] = tok.Token
		}
		var er api.EnrollResponse
		if code := call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tokens[customer], Name: name, Info: api.DeviceInfo{OS: "linux", Serial: serial}}, &er); code != 200 {
			t.Fatalf("enroll %s: %d", name, code)
		}
		return er.DeviceID
	}
	device := func(id string) *api.Device {
		t.Helper()
		var det struct {
			Device *api.Device `json:"device"`
		}
		call(t, ts, admin, "GET", "/api/v1/devices/"+id, nil, &det)
		return det.Device
	}
	a1 := enroll("acme-1", "acme", "SN-1")
	g1 := enroll("globex-1", "globex", "SN-G")

	csvText := "Serial,Device,Name,Channel,Labels,label:site,Asset tag,Location\n" +
		"SN-1,,till-1,,floor=2,orchard,ACME-1,Orchard store\n" +
		",globex-1,,beta,,,,\n" +
		"SN-9,,till-9,beta,customer=globex; role=till,sg,,Warehouse\n" +
		"\n"
	// Dry run: a report, no changes.
	code, res := importCSV(t, ts, admin, csvText, "?dry_run=1")
	if code != 200 || !res.DryRun || res.Applied || res.Updated != 2 || res.Created != 1 || res.Errors != 0 {
		t.Fatalf("dry run: %d %+v", code, res)
	}
	if d := device(a1); d.Name != "acme-1" || d.Labels["site"] != "" {
		t.Fatalf("dry run changed the device: %+v", d)
	}
	code, res = importCSV(t, ts, admin, csvText, "")
	if code != 200 || !res.Applied || res.Updated != 2 || res.Created != 1 {
		t.Fatalf("import: %d %+v", code, res)
	}
	if d := device(a1); d.Name != "till-1" || d.Labels["site"] != "orchard" || d.Labels["floor"] != "2" || d.Labels["customer"] != "acme" ||
		d.Inventory == nil || d.Inventory.AssetTag != "ACME-1" || d.Inventory.Location != "Orchard store" {
		t.Fatalf("a1 after import: %+v %+v", d, d.Inventory)
	}
	if d := device(g1); d.Channel != "beta" || d.Name != "globex-1" {
		t.Fatalf("g1 after import: %+v", d)
	}
	// Importing the same file again changes nothing more.
	if _, res = importCSV(t, ts, admin, csvText, "?dry_run=1"); res.Rows[0].Action != "unchanged" || res.Rows[1].Action != "unchanged" {
		t.Fatalf("re-import: %+v", res.Rows)
	}

	// The pre-registered serial enrolls (with an acme token, lower case):
	// it takes the registration's name, channel and labels, but the token's
	// customer label wins.
	n9 := enroll("raspberrypi", "acme", " sn-9 ")
	if d := device(n9); d.Name != "till-9" || d.Channel != "beta" || d.Labels["customer"] != "acme" || d.Labels["site"] != "sg" ||
		d.Labels["role"] != "till" || d.Inventory == nil || d.Inventory.Location != "Warehouse" {
		t.Fatalf("pre-registered device: %+v %+v", d, d.Inventory)
	}
	var regs []api.Registration
	call(t, ts, admin, "GET", "/api/v1/registrations", nil, &regs)
	if len(regs) != 1 || regs[0].ClaimedBy != n9 {
		t.Fatalf("registrations: %+v", regs)
	}
	// It's used once.
	if d := device(enroll("raspberrypi", "acme", "SN-9")); d.Name != "raspberrypi" {
		t.Fatalf("registration reused: %+v", d)
	}

	// Any bad row: nothing is applied.
	code, res = importCSV(t, ts, admin, "serial,name,channel\nSN-1,renamed,\nSN-2,x,Not A Channel\nSN-1,again,\n", "")
	if code != 200 || res.Applied || res.Errors != 2 || res.Rows[1].Error != "invalid channel" || !strings.Contains(res.Rows[2].Error, "line 2") {
		t.Fatalf("bad rows: %d %+v", code, res)
	}
	if d := device(a1); d.Name != "till-1" {
		t.Fatalf("a failed import changed a device: %+v", d)
	}
	for _, bad := range []string{"", "name\nx\n", "serial,colour\nSN-1,red\n", "serial,labels\nSN-1,novalue\n"} {
		if code, _ := importCSV(t, ts, admin, bad, ""); code != 400 {
			t.Errorf("%q: %d", bad, code)
		}
	}

	// A client operator.
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleOperator,
		Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-password-1"}, &lr)
	// Globex's device is invisible: its serial becomes a registration, not an edit.
	code, res = importCSV(t, ts, lr.Token, "serial,name\nSN-G,mine\nSN-50,acme-50\n", "")
	if code != 200 || !res.Applied || res.Rows[0].Action != "registered" || res.Created != 2 {
		t.Fatalf("client import: %d %+v", code, res)
	}
	if d := device(g1); d.Name != "globex-1" {
		t.Fatalf("client renamed another client's device: %+v", d)
	}
	// Can't move its own device to another client, or pre-register outside its scope.
	if _, res = importCSV(t, ts, lr.Token, "serial,labels\nSN-1,customer=globex\nSN-51,customer=globex\n", ""); res.Errors != 2 || res.Applied {
		t.Fatalf("client relabel: %+v", res)
	}
	// A client's registration only applies to devices enrolled into that client.
	if d := device(enroll("globex-2", "globex", "SN-50")); d.Name != "globex-2" {
		t.Fatalf("another client's enrollment used the registration: %+v", d)
	}
	if d := device(enroll("pi", "acme", "SN-50")); d.Name != "acme-50" {
		t.Fatalf("own enrollment: %+v", d)
	}
	// The client sees its own registrations only, and can't delete the vendor's.
	call(t, ts, lr.Token, "GET", "/api/v1/registrations", nil, &regs)
	if len(regs) != 2 {
		t.Fatalf("client registrations: %+v", regs)
	}
	if code := call(t, ts, lr.Token, "DELETE", "/api/v1/registrations/SN-9", nil, nil); code != 404 {
		t.Fatalf("client deleted vendor registration: %d", code)
	}
	if code := call(t, ts, lr.Token, "DELETE", "/api/v1/registrations/sn-g", nil, nil); code != 204 {
		t.Fatalf("client delete own: %d", code)
	}
	if code := call(t, ts, admin, "DELETE", "/api/v1/registrations/SN-9", nil, nil); code != 204 {
		t.Fatalf("admin delete: %d", code)
	}
}
