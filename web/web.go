// Package web embeds the Armada dashboard and device install script.
package web

import "embed"

// FS holds the dashboard's static files under "static/".
//
//go:embed static
var FS embed.FS

// InstallScript is served at /install.sh for one-line device onboarding
// (Linux and macOS).
//
//go:embed install.sh
var InstallScript []byte

// InstallPowerShell is served at /install.ps1 for Windows.
//
//go:embed install.ps1
var InstallPowerShell []byte
