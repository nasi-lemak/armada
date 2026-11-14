package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func (c *cli) schedules(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var all []api.ScheduledAction
		if err := c.do("GET", "/api/v1/scheduled-actions", nil, &all); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(all)
		}
		if len(all) == 0 {
			fmt.Fprintln(c.out, "nothing is scheduled")
			return nil
		}
		for _, sa := range all {
			next := "paused"
			if sa.Enabled && sa.NextRunAt != nil {
				next = "next " + sa.NextRunAt.Local().Format("Mon 2006-01-02 15:04")
			}
			last := "never run"
			if r := sa.LastRun; r != nil {
				switch {
				case r.Missed:
					last = "last run missed"
				case r.Error != "":
					last = "last run failed: " + r.Error
				default:
					last = fmt.Sprintf("last run %s on %d device(s) (%s)", r.At.Local().Format("01-02 15:04"), r.Applied, r.BulkID)
				}
			}
			days := "daily"
			if len(sa.Days) > 0 {
				days = strings.Join(sa.Days, ",")
			}
			fmt.Fprintf(c.out, "%s  %-24s %-14s %s %s %s  %d device(s)  %s; %s\n", sa.ID, sa.Name, sa.Operation, days, sa.At,
				cmpOr(sa.TimeZone, "UTC"), sa.Matches, next, last)
		}
		return nil
	case "create", "add":
		fs := flag.NewFlagSet("schedules create", flag.ContinueOnError)
		name := fs.String("name", "", "")
		op := fs.String("op", "reboot", "")
		var labels multiFlag
		fs.Var(&labels, "label", "")
		channel := fs.String("channel", "", "")
		all := fs.Bool("all", false, "")
		at := fs.String("at", "", "")
		days := fs.String("days", "", "")
		tz := fs.String("tz", "", "")
		hold := fs.Int("hold", 30, "")
		command := fs.String("command", "", "")
		client := fs.String("client", "", "")
		paused := fs.Bool("paused", false, "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		if *name == "" || *at == "" {
			return errors.New("usage: schedules create --name N --at HH:MM [--op reboot|restart-agent|lock|wake|exec] [--label K=V]... [--channel C] [--all] [--days mon,tue] [--tz Asia/Singapore] [--hold MIN] [--command \"...\"]")
		}
		sa := api.ScheduledAction{Name: *name, Operation: *op, All: *all, At: *at, TimeZone: *tz, HoldAlertsMinutes: hold, Enabled: !*paused,
			Selector: api.Selector{Channel: *channel}}
		if *days != "" {
			sa.Days = strings.Split(*days, ",")
		}
		if *command != "" {
			sa.Args = strings.Fields(*command)
		}
		for _, l := range labels {
			k, v, ok := strings.Cut(l, "=")
			if !ok {
				return fmt.Errorf("--label must look like key=value")
			}
			if sa.Selector.Labels == nil {
				sa.Selector.Labels = map[string]string{}
			}
			sa.Selector.Labels[k] = v
		}
		path := "/api/v1/scheduled-actions"
		if *client != "" {
			path += "?client=" + url.QueryEscape(*client)
		}
		var out api.ScheduledAction
		if err := c.do("POST", path, sa, &out); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(out)
		}
		next := "paused"
		if out.NextRunAt != nil {
			next = "first run " + out.NextRunAt.Local().Format(time.RFC1123)
		}
		fmt.Fprintf(c.out, "created %s: %d device(s) match now; %s\n", out.ID, out.Matches, next)
		return nil
	case "run", "pause", "resume", "delete", "rm":
		if len(args) != 1 {
			return fmt.Errorf("usage: schedules %s ID", verb)
		}
		id := url.PathEscape(args[0])
		switch verb {
		case "run":
			var out api.ScheduledAction
			if err := c.do("POST", "/api/v1/scheduled-actions/"+id+"/run", nil, &out); err != nil {
				return err
			}
			if r := out.LastRun; r != nil && r.Error != "" {
				return errors.New(r.Error)
			} else if r != nil {
				fmt.Fprintf(c.out, "queued on %d device(s), %d skipped (bulk %s)\n", r.Applied, r.Skipped, r.BulkID)
			}
			return nil
		case "delete", "rm":
			return c.do("DELETE", "/api/v1/scheduled-actions/"+id, nil, nil)
		}
		var all []api.ScheduledAction
		if err := c.do("GET", "/api/v1/scheduled-actions", nil, &all); err != nil {
			return err
		}
		for _, sa := range all {
			if sa.ID == args[0] {
				sa.Enabled = verb == "resume"
				return c.do("PUT", "/api/v1/scheduled-actions/"+id, sa, nil)
			}
		}
		return fmt.Errorf("no scheduled action %s", args[0])
	}
	return fmt.Errorf("unknown schedules command %q", verb)
}

func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
