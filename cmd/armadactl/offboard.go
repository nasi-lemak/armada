package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

var offboardSections = []struct{ key, title string }{
	{"devices", "devices"}, {"users", "accounts deleted"}, {"users_narrowed", "accounts that lose only this customer"},
	{"group_mappings", "group mappings"}, {"tokens", "enrollment tokens revoked"}, {"alert_rules", "alert rules"},
	{"channels", "notification channels"}, {"event_hooks", "event webhooks"}, {"registrations", "pre-registrations"},
	{"scheduled_actions", "scheduled actions"}, {"maintenance", "maintenance windows"}, {"report_schedules", "report schedules"},
	{"cert_policies", "certificate policies"},
}

// offboard: plan, export or run a customer's offboarding.
func (c *cli) offboard(verb string, args []string) error {
	fs := flag.NewFlagSet("offboard "+verb, flag.ContinueOnError)
	var labelArgs multiFlag
	fs.Var(&labelArgs, "label", "")
	retire := fs.Bool("retire", false, "")
	confirm := fs.String("confirm", "", "")
	out := fs.String("o", "", "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	labels, err := labelsOf(labelArgs)
	if err != nil {
		return err
	}
	if len(labels) == 0 {
		return errors.New("say which customer: --label customer=acme")
	}
	switch verb {
	case "export":
		if *out == "" {
			*out = "armada-export-" + strings.NewReplacer("=", "_", ",", "_").Replace(labelString(labels)) + "-" + time.Now().Format("2006-01-02") + ".zip"
		}
		f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if err := c.download("/api/v1/offboarding/export?labels="+url.QueryEscape(labelString(labels)), f); err != nil {
			f.Close()
			os.Remove(*out)
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "wrote", *out)
		return nil
	case "plan", "run":
		req := map[string]any{"labels": labels}
		if *retire {
			req["device_action"] = "retire"
		}
		if verb == "run" {
			if *confirm == "" {
				return fmt.Errorf("this removes the customer's devices and accounts; add --confirm %s", labelString(labels))
			}
			req["confirm"] = *confirm
		}
		var raw map[string]any
		if err := c.do("POST", "/api/v1/offboarding", req, &raw); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(raw)
		}
		if raw["done"] == true {
			fmt.Fprintf(c.out, "offboarded %s:\n", raw["confirm"])
		} else {
			fmt.Fprintf(c.out, "offboarding %s would remove (devices are %sd):\n", raw["confirm"], raw["device_action"])
		}
		listed := false
		for _, s := range offboardSections {
			items, _ := raw[s.key].([]any)
			if len(items) == 0 {
				continue
			}
			listed = true
			var names []string
			for _, it := range items {
				m, _ := it.(map[string]any)
				n, _ := m["name"].(string)
				if n == "" {
					n, _ = m["id"].(string)
				}
				names = append(names, n)
			}
			fmt.Fprintf(c.out, "  %3d %s: %s\n", len(items), s.title, strings.Join(names, ", "))
		}
		if !listed {
			fmt.Fprintln(c.out, "  nothing")
		}
		if raw["done"] != true {
			fmt.Fprintf(c.out, "\nExport first: armadactl offboard export --label ...\nThen run: armadactl offboard run --label ... --confirm %s\n", raw["confirm"])
		}
		return nil
	}
	return fmt.Errorf("unknown offboard command %q: plan, export or run", verb)
}

// labelString is labels as k=v,k2=v2, sorted, as the server writes them.
func labelString(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, ",")
}
