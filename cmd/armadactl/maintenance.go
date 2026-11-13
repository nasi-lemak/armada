package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func (c *cli) maintenance(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		fs := flag.NewFlagSet("maintenance list", flag.ContinueOnError)
		all := fs.Bool("all", false, "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		q := ""
		if *all {
			q = "?all=1"
		}
		var ms []api.Maintenance
		if err := c.do("GET", "/api/v1/maintenance"+q, nil, &ms); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(ms)
		}
		var rows [][]string
		for _, m := range ms {
			rows = append(rows, []string{m.ID, m.State, m.Name, maintenanceTargets(m), maintenanceWhen(m), maintenanceEffects(m), m.CreatedBy})
		}
		c.table("ID\tSTATE\tNAME\tDEVICES\tWHEN\tEFFECT\tBY", rows)
		return nil

	case "start", "schedule", "create":
		fs := flag.NewFlagSet("maintenance "+verb, flag.ContinueOnError)
		name := fs.String("name", "", "")
		note := fs.String("note", "", "")
		dur := fs.Duration("for", 0, "")
		start := fs.String("start", "", "")
		end := fs.String("end", "", "")
		windows := fs.String("windows", "", "")
		days := fs.String("days", "", "")
		tz := fs.String("tz", "", "")
		channel := fs.String("channel", "", "")
		alertsOnly := fs.Bool("alerts-only", false, "")
		updatesOnly := fs.Bool("updates-only", false, "")
		var devices, labels, client multiFlag
		fs.Var(&devices, "device", "")
		fs.Var(&labels, "label", "")
		fs.Var(&client, "client", "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		m := api.Maintenance{Name: *name, Note: *note, Windows: *windows, TimeZone: *tz,
			Selector: api.Selector{Channel: *channel, DeviceIDs: devices}}
		var err error
		if m.Selector.Labels, err = labelsOf(labels); err != nil {
			return err
		}
		if m.ClientScope.Labels, err = labelsOf(client); err != nil {
			return err
		}
		if *days != "" {
			m.Days = strings.Split(*days, ",")
		}
		f := false
		switch {
		case *alertsOnly && *updatesOnly:
			return fmt.Errorf("--alerts-only and --updates-only contradict each other")
		case *alertsOnly:
			m.PauseUpdates = &f
		case *updatesOnly:
			m.SuppressAlerts = &f
		}
		when := func(flagName, s string) (time.Time, error) {
			t, err := parseStartAt(s, *tz)
			if err != nil {
				return t, fmt.Errorf("invalid --%s %q (use \"2026-12-01 02:00\" or RFC 3339)", flagName, s)
			}
			return t, nil
		}
		if *start != "" {
			if m.StartsAt, err = when("start", *start); err != nil {
				return err
			}
		} else {
			m.StartsAt = time.Now()
		}
		switch {
		case *end != "" && *dur != 0:
			return fmt.Errorf("use --end or --for, not both")
		case *end != "":
			e, err := when("end", *end)
			if err != nil {
				return err
			}
			m.EndsAt = &e
		case *dur != 0:
			e := m.StartsAt.Add(*dur)
			m.EndsAt = &e
		case *windows == "" && *days == "":
			return fmt.Errorf("say how long: --for 2h, --end \"2026-12-01 06:00\", or recurring --windows/--days")
		}
		var out api.Maintenance
		if err := c.do("POST", "/api/v1/maintenance", m, &out); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(out)
		}
		fmt.Fprintf(c.out, "%s %s (%s): %s, %s\n", out.ID, out.Name, out.State, maintenanceWhen(out), maintenanceEffects(out))
		return nil

	case "end", "stop", "cancel":
		if err := need(args, 1, "maintenance id"); err != nil {
			return err
		}
		var out api.Maintenance
		if err := c.do("POST", "/api/v1/maintenance/"+args[0]+"/end", nil, &out); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "ended %s\n", out.Name)
		return nil

	case "delete", "rm":
		if err := need(args, 1, "maintenance id"); err != nil {
			return err
		}
		return c.do("DELETE", "/api/v1/maintenance/"+args[0], nil, nil)
	}
	return fmt.Errorf("unknown maintenance command %q", verb)
}

func maintenanceTargets(m api.Maintenance) string {
	var parts []string
	if n := len(m.Selector.DeviceIDs); n == 1 {
		parts = append(parts, m.Selector.DeviceIDs[0])
	} else if n > 1 {
		parts = append(parts, fmt.Sprintf("%d devices", n))
	}
	for k, v := range m.Selector.Labels {
		parts = append(parts, k+"="+v)
	}
	if m.Selector.Channel != "" {
		parts = append(parts, "channel="+m.Selector.Channel)
	}
	return strings.Join(parts, " ")
}

func maintenanceWhen(m api.Maintenance) string {
	const layout = "2006-01-02 15:04"
	s := m.StartsAt.Local().Format(layout)
	if m.EndsAt != nil {
		s += " → " + m.EndsAt.Local().Format(layout)
	} else {
		s = "from " + s
	}
	if m.Windows != "" || len(m.Days) > 0 {
		w := m.Windows
		if w == "" {
			w = "all day"
		}
		if len(m.Days) > 0 {
			w += " " + strings.Join(m.Days, ",")
		}
		if m.TimeZone != "" {
			w += " " + m.TimeZone
		}
		s = w + " (" + s + ")"
	}
	return s
}

func maintenanceEffects(m api.Maintenance) string {
	var e []string
	if m.Holds() {
		e = append(e, "alerts held")
	}
	if m.Pauses() {
		e = append(e, "updates paused")
	}
	return strings.Join(e, ", ")
}
