package rollout

import "testing"

// Versions are reported by devices. Compare must not crash and must be a
// consistent order: equal to itself, and reversing the arguments reverses
// the answer.
func FuzzCompare(f *testing.F) {
	for _, p := range [][2]string{{"1.2.3", "1.10.0"}, {"2.0.0-rc1", "2.0.0"}, {"v1.0", "1.0"}, {"1.0.0+build5", "1.0.0"},
		{"", "0"}, {"1..2", "1.2"}, {"10a", "10b"}, {"99999999999999999999999", "1"}, {"1.0~beta", "1.0"}} {
		f.Add(p[0], p[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		ab, ba := Compare(a, b), Compare(b, a)
		if Compare(a, a) != 0 {
			t.Fatalf("Compare(%q, itself) = %d", a, Compare(a, a))
		}
		if sign(ab) != -sign(ba) {
			t.Fatalf("Compare(%q, %q) = %d but Compare(%q, %q) = %d", a, b, ab, b, a, ba)
		}
	})
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func FuzzParseWindows(f *testing.F) {
	for _, s := range []string{"02:00-05:00", "22:00-02:00,12:00-13:00", "", "25:00-01:00", "1:2-3:4", "-", ",,"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ws, err := ParseWindows(s)
		if err != nil {
			return
		}
		for _, w := range ws {
			if w.Start < 0 || w.Start >= 24*60 || w.End < 0 || w.End > 24*60 { // 24:00 may end one
				t.Fatalf("%q gave window %+v outside a day", s, w)
			}
		}
	})
}
