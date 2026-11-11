package rollout

import (
	"fmt"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// Window is a daily local-time interval in minutes since midnight. End may be
// less than Start for windows that span midnight; 24:00 means end of day.
type Window struct{ Start, End int }

// ParseWindows parses "HH:MM-HH:MM[,HH:MM-HH:MM...]".
func ParseWindows(s string) ([]Window, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []Window
	for _, part := range strings.Split(s, ",") {
		a, b, ok := strings.Cut(strings.TrimSpace(part), "-")
		if !ok {
			return nil, fmt.Errorf("window %q must look like 02:00-05:00", part)
		}
		start, err := parseClock(a)
		if err != nil {
			return nil, err
		}
		end, err := parseClock(b)
		if err != nil {
			return nil, err
		}
		out = append(out, Window{start, end})
	}
	return out, nil
}

func parseClock(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "24:00" {
		return 24 * 60, nil
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("invalid time %q in window (use HH:MM)", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// InWindow reports whether t's wall-clock time falls in any window. No
// windows means always.
func InWindow(ws []Window, t time.Time) bool {
	if len(ws) == 0 {
		return true
	}
	m := t.Hour()*60 + t.Minute()
	for _, w := range ws {
		if w.Start <= w.End {
			if m >= w.Start && m < w.End {
				return true
			}
		} else if m >= w.Start || m < w.End {
			return true
		}
	}
	return false
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// validateSchedule checks a strategy's rollout windows, days and time zone.
func validateSchedule(s *api.Strategy) error {
	if _, err := ParseWindows(s.Windows); err != nil {
		return err
	}
	for i, d := range s.Days {
		d = strings.ToLower(strings.TrimSpace(d))
		if len(d) > 3 {
			d = d[:3]
		}
		if _, ok := weekdays[d]; !ok {
			return fmt.Errorf("unknown day %q (use mon, tue, wed, thu, fri, sat, sun)", s.Days[i])
		}
		s.Days[i] = d
	}
	if s.TimeZone != "" {
		if _, err := time.LoadLocation(s.TimeZone); err != nil {
			return fmt.Errorf("unknown time zone %q (use an IANA name such as Asia/Singapore)", s.TimeZone)
		}
	}
	return nil
}

// WindowOpen reports whether a deployment may make new offers and advance
// stages at time t: within its windows, on its days, in its time zone. A
// window that crosses midnight belongs to the day it starts on.
func WindowOpen(s api.Strategy, t time.Time) bool {
	if s.Windows == "" && len(s.Days) == 0 {
		return true
	}
	loc := time.UTC
	if s.TimeZone != "" {
		if l, err := time.LoadLocation(s.TimeZone); err == nil {
			loc = l
		}
	}
	t = t.In(loc)
	ws, _ := ParseWindows(s.Windows)
	if len(ws) == 0 {
		ws = []Window{{0, 24 * 60}}
	}
	dayOK := func(d time.Weekday) bool {
		if len(s.Days) == 0 {
			return true
		}
		for _, x := range s.Days {
			if weekdays[x] == d {
				return true
			}
		}
		return false
	}
	m := t.Hour()*60 + t.Minute()
	for _, w := range ws {
		switch {
		case w.Start <= w.End:
			if m >= w.Start && m < w.End && dayOK(t.Weekday()) {
				return true
			}
		case m >= w.Start: // evening part of an overnight window
			if dayOK(t.Weekday()) {
				return true
			}
		case m < w.End: // morning part: it started yesterday
			if dayOK(t.AddDate(0, 0, -1).Weekday()) {
				return true
			}
		}
	}
	return false
}
