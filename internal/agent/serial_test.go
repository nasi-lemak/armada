package agent

import "testing"

func TestCleanSerial(t *testing.T) {
	for in, want := range map[string]string{
		"  C02XK0ABJG5H\n":       "C02XK0ABJG5H",
		"10000000a3b4c5d6\x00":   "10000000a3b4c5d6",
		"To Be Filled By O.E.M.": "",
		"Default string":         "",
		"0000000000000000":       "",
		"System Serial Number":   "",
		"ffffffff":               "",
		"":                       "",
	} {
		if got := cleanSerial(in); got != want {
			t.Errorf("cleanSerial(%q) = %q, want %q", in, got, want)
		}
	}
}
