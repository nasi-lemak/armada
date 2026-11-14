package server

import "testing"

func TestOpenAPISummaries(t *testing.T) {
	for in, want := range map[string]string{"handleCreateSCIMToken": "Create SCIM token", "handleDownloadCSRs": "Download CSRs",
		"handleListDeviceCerts": "List device certs", "handleTwoFactorDisable": "Two factor disable", "handleListURLs": "List URLs"} {
		if got := summaryFor(apiRoute{pattern: "GET /x", handler: in}); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
	for pattern, want := range map[string]string{"POST /api/v1/channels": "Create channel", "PUT /api/v1/channels/{id}": "Update channel"} {
		if got := summaryFor(apiRoute{pattern: pattern, handler: "handleSaveChannel"}); got != want {
			t.Errorf("%s: %q, want %q", pattern, got, want)
		}
	}
}
