package agent

import (
	"strings"
	"sync"
)

var (
	serialOnce  sync.Once
	serialValue string
)

// hardwareSerial is the device's hardware serial number, read once (it
// doesn't change, and on some platforms reading it runs a command).
func hardwareSerial() string {
	serialOnce.Do(func() { serialValue = cleanSerial(platformSerial()) })
	return serialValue
}

// cleanSerial drops the placeholders firmware ships when nobody set a serial.
func cleanSerial(s string) string {
	s = strings.TrimSpace(strings.Trim(s, "\x00"))
	switch strings.ToLower(s) {
	case "", "0", "none", "n/a", "na", "default string", "to be filled by o.e.m.", "system serial number",
		"not specified", "not applicable", "unknown", "0123456789", "123456789", "serial":
		return ""
	}
	if strings.Trim(s, "0") == "" || strings.Trim(s, "fF") == "" {
		return ""
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
