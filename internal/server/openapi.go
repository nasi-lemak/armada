package server

import (
	"net/http"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode"
)

// The OpenAPI document is generated from the route table, so it lists
// exactly the routes the server has, with the role each needs and whether
// client accounts may use it. Request and response bodies are the JSON
// forms of the types in internal/api.

type apiRoute struct {
	pattern string // "GET /api/v1/devices/{id}"
	minRole string
	clients bool // client (device-scoped) accounts may call it
	handler string
}

// handlerName is a handler method's name ("handleListEvents").
func handlerName(h http.HandlerFunc) string {
	name := runtime.FuncForPC(reflect.ValueOf(h).Pointer()).Name()
	name = strings.TrimSuffix(name[strings.LastIndex(name, ".")+1:], "-fm")
	return name
}

// summaryOverrides fix summaries the handler name doesn't say well.
var summaryOverrides = map[string]string{
	"GET /api/v1/events":                "List activity-log events (filters: type, actor, q, since, until, device_id; format=csv)",
	"GET /api/v1/devices/{id}":          "Get a device with its updates, actions and events",
	"POST /api/v1/bulk":                 "Preview (dry_run) or run a bulk operation on devices",
	"GET /api/v1/reports":               "Availability report (format=json, csv or html)",
	"POST /api/v1/registrations/import": "Import a CSV: update enrolled devices, pre-register serials (dry_run=1 to preview)",
	"GET /api/v1/openapi.json":          "This document",
	"GET /api/v1/me":                    "Who you are: your account, role and access",
	"GET /api/v1/search":                "Quick search: devices, releases, rollouts, users (q=...)",
}

// splitCamel splits "CreateSCIMToken" into Create, SCIM, Token.
func splitCamel(s string) []string {
	r := []rune(s)
	var out []string
	start := 0
	for i := 1; i < len(r); i++ {
		lowerBefore := unicode.IsLower(r[i-1]) || unicode.IsDigit(r[i-1])
		// "SCIMToken" splits before T; "CSRs" (a plural acronym) doesn't split.
		pluralS := i+1 < len(r) && r[i+1] == 's' && (i+2 == len(r) || unicode.IsUpper(r[i+2]))
		acronymEnd := unicode.IsUpper(r[i-1]) && i+1 < len(r) && unicode.IsLower(r[i+1]) && !pluralS
		if unicode.IsUpper(r[i]) && (lowerBefore || acronymEnd) {
			out = append(out, string(r[start:i]))
			start = i
		}
	}
	return append(out, string(r[start:]))
}

// opName is the handler's name without "handle"; a handler that serves
// both POST and PUT (saveX, putX) is createX or updateX by method.
func opName(rt apiRoute) string {
	name := strings.TrimPrefix(rt.handler, "handle")
	for _, verb := range []string{"Save", "Put"} {
		if rest, ok := strings.CutPrefix(name, verb); ok && rest != "" {
			switch {
			case strings.HasPrefix(rt.pattern, "POST "):
				return "Create" + rest
			case strings.HasPrefix(rt.pattern, "PUT "):
				return "Update" + rest
			}
		}
	}
	return name
}

// summaryFor turns "handleListDeviceCerts" into "List device certs".
func summaryFor(rt apiRoute) string {
	if s, ok := summaryOverrides[rt.pattern]; ok {
		return s
	}
	name := opName(rt)
	if name == "" || strings.HasPrefix(name, "func") {
		return rt.pattern
	}
	words := splitCamel(name)
	for i, w := range words {
		switch {
		case len(w) > 1 && strings.ToUpper(w) == w: // an acronym
		case w == "CSRs" || w == "URLs":
		default:
			if i == 0 {
				words[i] = w
			} else {
				words[i] = strings.ToLower(w)
			}
		}
	}
	return strings.Join(words, " ")
}

var pathParam = regexp.MustCompile(`\{([a-zA-Z_]+)(\.\.\.)?\}`)

func (s *Server) openAPI(base string) map[string]any {
	paths := map[string]map[string]any{}
	routes := append([]apiRoute(nil), s.apiRoutes...)
	sort.Slice(routes, func(i, j int) bool { return routes[i].pattern < routes[j].pattern })
	for _, rt := range routes {
		method, path, ok := strings.Cut(rt.pattern, " ")
		if !ok {
			continue
		}
		path = pathParam.ReplaceAllString(path, "{$1}")
		var params []map[string]any
		for _, m := range pathParam.FindAllStringSubmatch(path, -1) {
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
		}
		tag := strings.SplitN(strings.TrimPrefix(path, "/api/v1/"), "/", 2)[0]
		op := map[string]any{
			"operationId":              operationID(rt),
			"summary":                  summaryFor(rt),
			"tags":                     []string{tag},
			"x-armada-min-role":        rt.minRole,
			"x-armada-client-accounts": rt.clients,
			"responses": map[string]any{
				"2XX": map[string]string{"description": "Success (JSON unless stated)"},
				"400": map[string]string{"description": "Invalid request: {\"error\": \"...\"}"},
				"401": map[string]string{"description": "Missing or invalid token"},
				"403": map[string]string{"description": "Your role or client scope doesn't allow it"},
				"404": map[string]string{"description": "Not found (or not yours)"},
			},
		}
		if params != nil {
			op["parameters"] = params
		}
		if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
			op["requestBody"] = map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": map[string]string{"type": "object"}}}}
		}
		if paths[path] == nil {
			paths[path] = map[string]any{}
		}
		paths[path][strings.ToLower(method)] = op
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "Armada API",
			"version": "1",
			"description": "The Armada fleet management API. Authenticate with `Authorization: Bearer <token>`: a personal API token " +
				"(My account → API tokens), the server's admin token, or a dashboard session. Each operation states the least role it " +
				"needs (x-armada-min-role: viewer < operator < admin) and whether client accounts, limited to some devices, may use it " +
				"(x-armada-client-accounts); they only ever see and change their own devices. Bodies are JSON; errors are " +
				"{\"error\": \"...\"}. Sign-in (/api/v1/auth/*), SCIM (/scim/v2/*) and the agent protocol (/api/v1/agent/*) are not listed.",
		},
		"servers":    []map[string]string{{"url": base}},
		"security":   []map[string][]string{{"bearer": {}}},
		"components": map[string]any{"securitySchemes": map[string]any{"bearer": map[string]string{"type": "http", "scheme": "bearer"}}},
		"paths":      paths,
	}
}

// operationID is the handler name without "handle", lower camel case.
func operationID(rt apiRoute) string {
	name := opName(rt)
	if name == "" || strings.HasPrefix(name, "func") {
		return strings.NewReplacer(" ", "_", "/", "_", "{", "", "}", "").Replace(rt.pattern)
	}
	r := []rune(name)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.openAPI(s.publicURL(r)))
}
