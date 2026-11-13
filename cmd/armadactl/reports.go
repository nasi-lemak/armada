package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func (c *cli) report(verb string, args []string) error {
	if verb == "schedules" || verb == "schedule" {
		sub := ""
		if len(args) > 0 {
			sub, args = args[0], args[1:]
		}
		return c.reportSchedules(sub, args)
	}
	if verb != "" {
		args = append([]string{verb}, args...)
	}
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	period := fs.String("period", "", "")
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	tz := fs.String("tz", "", "")
	channel := fs.String("channel", "", "")
	format := fs.String("format", "", "")
	out := fs.String("o", "", "")
	var labels multiFlag
	fs.Var(&labels, "label", "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	q := url.Values{}
	for k, v := range map[string]string{"period": *period, "from": *from, "to": *to, "tz": *tz, "channel": *channel, "format": *format} {
		if v != "" {
			q.Set(k, v)
		}
	}
	for _, l := range labels {
		q.Add("label", l)
	}
	if *format == "csv" || *format == "html" {
		req, err := http.NewRequest("GET", c.server+"/api/v1/reports?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		}
		w := c.out
		if *out != "" {
			f, err := os.Create(*out)
			if err != nil {
				return err
			}
			defer f.Close()
			w = f
		}
		_, err = io.Copy(w, resp.Body)
		if err == nil && *out != "" {
			fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
		}
		return err
	}
	var rep api.Report
	if err := c.do("GET", "/api/v1/reports?"+q.Encode(), nil, &rep); err != nil {
		return err
	}
	if c.json {
		return c.printJSON(rep)
	}
	loc, _ := time.LoadLocation(rep.TimeZone)
	if loc == nil {
		loc = time.UTC
	}
	sm := rep.Summary
	fmt.Fprintf(c.out, "%s: %s – %s (%s)\n\n", rep.Title, rep.From.In(loc).Format("2 Jan 2006"), rep.To.In(loc).Add(-time.Second).Format("2 Jan 2006"), rep.TimeZone)
	fmt.Fprintf(c.out, "Devices:       %d (%d in service, %d spare, %d in repair, %d retired)\n", sm.Devices, sm.Active, sm.Spare, sm.InRepair, sm.Retired)
	if sm.Availability != nil {
		fmt.Fprintf(c.out, "Availability:  %.2f%% (%d below %.0f%%)\n", *sm.Availability, sm.BelowTarget, sm.AvailabilityGoal)
	}
	ttr := "—"
	if sm.MeanTimeToResolveMinutes != nil {
		ttr = fmt.Sprintf("%.0f min", *sm.MeanTimeToResolveMinutes)
	}
	fmt.Fprintf(c.out, "Alerts:        %d critical, %d warning (mean time to resolve %s)\n", sm.AlertsCritical, sm.AlertsWarning, ttr)
	fmt.Fprintf(c.out, "Updates:       %d installed, %d failed\n", sm.UpdatesSucceeded, sm.UpdatesFailed)
	fmt.Fprintf(c.out, "Replacements:  %d · maintenance windows: %d\n\n", sm.Replacements, sm.MaintenanceWindows)
	var rows [][]string
	for _, d := range rep.Devices {
		avail, down := "—", "—"
		if d.Availability != nil {
			avail = strconv.FormatFloat(*d.Availability, 'f', 2, 64) + "%"
			down = fmt.Sprintf("%d min", *d.DowntimeMinutes)
		}
		rows = append(rows, []string{d.ID, d.Name, d.Lifecycle, avail, down, strconv.Itoa(d.Alerts), fmt.Sprintf("%d/%d", d.UpdatesSucceeded, d.UpdatesFailed), d.Health})
	}
	c.table("ID\tNAME\tSTATE\tAVAILABILITY\tDOWNTIME\tALERTS\tUPDATES OK/FAILED\tHEALTH", rows)
	for _, n := range rep.Notes {
		fmt.Fprintf(c.out, "\n%s", n)
	}
	fmt.Fprintln(c.out)
	return nil
}

func (c *cli) reportSchedules(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var rs []api.ReportSchedule
		if err := c.do("GET", "/api/v1/report-schedules", nil, &rs); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(rs)
		}
		var rows [][]string
		for _, r := range rs {
			last := "never"
			if r.LastSentAt != nil {
				last = ago(*r.LastSentAt)
			}
			if r.LastError != "" {
				last += " (last attempt failed: " + firstLine(r.LastError) + ")"
			}
			rows = append(rows, []string{r.ID, r.Name, r.Period, strings.Join(r.Recipients, ", "), last})
		}
		c.table("ID\tNAME\tPERIOD\tRECIPIENTS\tLAST SENT", rows)
		return nil
	case "create", "add":
		fs := flag.NewFlagSet("report schedules create", flag.ContinueOnError)
		name := fs.String("name", "", "")
		period := fs.String("period", "monthly", "")
		tz := fs.String("tz", "", "")
		var to, client multiFlag
		fs.Var(&to, "to", "")
		fs.Var(&client, "client", "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		rs := api.ReportSchedule{Name: *name, Period: *period, TimeZone: *tz, Recipients: to}
		var err error
		if rs.ClientScope.Labels, err = labelsOf(client); err != nil {
			return err
		}
		var out api.ReportSchedule
		if err := c.do("POST", "/api/v1/report-schedules", rs, &out); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "%s %s → %s\n", out.ID, out.Name, strings.Join(out.Recipients, ", "))
		return nil
	case "send":
		if err := need(args, 1, "schedule id"); err != nil {
			return err
		}
		if err := c.do("POST", "/api/v1/report-schedules/"+args[0]+"/send", nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "sent")
		return nil
	case "delete", "rm":
		if err := need(args, 1, "schedule id"); err != nil {
			return err
		}
		return c.do("DELETE", "/api/v1/report-schedules/"+args[0], nil, nil)
	}
	return fmt.Errorf("unknown report schedules command %q", verb)
}
