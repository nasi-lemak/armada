// Package web embeds the Armada dashboard and device install script.
package web

import "embed"

// FS holds the dashboard's static files under "static/".
//
//go:embed static
var FS embed.FS

// InstallScript is served at /install.sh for one-line device onboarding.
//
//go:embed install.sh
var InstallScript []byte
