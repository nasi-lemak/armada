package rollout

import (
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestWindowOpen(t *testing.T) {
	sg, _ := time.LoadLocation("Asia/Singapore")
	at := func(day, hhmm string) time.Time { // day in Nov 2026: 9=Mon ... 15=Sun
		tm, _ := time.ParseInLocation("2006-01-02 15:04", "2026-11-"+day+" "+hhmm, sg)
		return tm
	}
	st := api.Strategy{Windows: "22:00-05:00", Days: []string{"mon", "tue", "wed", "thu", "fri"}, TimeZone: "Asia/Singapore"}
	if err := ValidateStrategy(&st); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		day, hhmm string
		want      bool
	}{
		{"09", "21:59", false}, // Monday evening, before the window
		{"09", "22:00", true},  // Monday night
		{"10", "04:59", true},  // Tuesday early morning: Monday's window
		{"10", "05:00", false},
		{"13", "23:00", true},  // Friday night
		{"14", "03:00", true},  // Saturday 03:00 still belongs to Friday's window
		{"14", "23:00", false}, // Saturday night: no window
		{"16", "01:00", false}, // Monday 01:00 belongs to Sunday, which is excluded
	} {
		if got := WindowOpen(st, at(c.day, c.hhmm)); got != c.want {
			t.Errorf("%s %s: got %v", c.day, c.hhmm, got)
		}
	}
	// The time zone matters: 22:00 in Singapore is 14:00 UTC.
	if !WindowOpen(st, time.Date(2026, 11, 9, 14, 30, 0, 0, time.UTC)) {
		t.Error("time zone not applied")
	}
	if !WindowOpen(api.Strategy{}, time.Now()) || !WindowOpen(api.Strategy{Days: []string{"sat"}}, at("14", "12:00")) {
		t.Error("open cases")
	}
	for _, bad := range []api.Strategy{{Windows: "25:00-01:00"}, {Days: []string{"someday"}}, {TimeZone: "Mars/Olympus"}} {
		if ValidateStrategy(&bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestWindowEnd(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		s    api.Strategy
		now  string
		want string
	}{
		{api.Strategy{Windows: "02:00-04:00"}, "2026-11-15T03:00:00Z", "2026-11-15T04:00:00Z"},
		{api.Strategy{Windows: "22:00-05:00"}, "2026-11-15T23:00:00Z", "2026-11-16T05:00:00Z"},
		{api.Strategy{Windows: "22:00-05:00"}, "2026-11-16T01:00:00Z", "2026-11-16T05:00:00Z"},
		// Whole days in a row merge: Saturday and Sunday end Monday 00:00.
		{api.Strategy{Days: []string{"sat", "sun"}}, "2026-11-14T10:00:00Z", "2026-11-16T00:00:00Z"},
		{api.Strategy{Windows: "02:00-04:00", TimeZone: "Asia/Singapore"}, "2026-11-14T19:00:00Z", "2026-11-14T20:00:00Z"},
	} {
		if got := WindowEnd(c.s, at(c.now)); !got.Equal(at(c.want)) {
			t.Errorf("%+v at %s: got %s, want %s", c.s, c.now, got.UTC().Format(time.RFC3339), c.want)
		}
	}
}
