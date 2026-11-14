package server

import (
	"strings"
	"testing"
)

func TestOpenAPI(t *testing.T) {
	_, ts := setup(t)
	var doc struct {
		OpenAPI string                               `json:"openapi"`
		Paths   map[string]map[string]map[string]any `json:"paths"`
	}
	if code := call(t, ts, admin, "GET", "/api/v1/openapi.json", nil, &doc); code != 200 || doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi: %d %q", code, doc.OpenAPI)
	}
	ids := map[string]string{}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			id, _ := op["operationId"].(string)
			if id == "" || ids[id] != "" {
				t.Errorf("%s %s: operationId %q (also %s)", method, path, id, ids[id])
			}
			ids[id] = method + " " + path
			if s, _ := op["summary"].(string); s == "" || strings.HasPrefix(s, "func") {
				t.Errorf("%s %s: summary %q", method, path, s)
			}
			if strings.Contains(path, "{") && op["parameters"] == nil {
				t.Errorf("%s %s: no path parameters", method, path)
			}
		}
	}
	dev := doc.Paths["/api/v1/devices/{id}"]["get"]
	if dev["x-armada-min-role"] != "viewer" || dev["x-armada-client-accounts"] != true {
		t.Fatalf("device route: %v", dev)
	}
	if del := doc.Paths["/api/v1/devices/{id}"]["delete"]; del["x-armada-client-accounts"] != false {
		t.Fatalf("delete device: %v", del)
	}
	if len(doc.Paths) < 80 {
		t.Fatalf("only %d paths", len(doc.Paths))
	}
}
