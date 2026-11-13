package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
)

func (c *cli) bulk(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var bs []api.BulkOperation
		if err := c.do("GET", "/api/v1/bulk", nil, &bs); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(bs)
		}
		var rows [][]string
		for _, b := range bs {
			rows = append(rows, []string{b.ID, b.Summary, fmt.Sprint(len(b.Applied)), fmt.Sprint(len(b.Skipped)), fmtProgress(b.Progress), b.CreatedBy, ago(b.CreatedAt)})
		}
		c.table("ID\tOPERATION\tDEVICES\tSKIPPED\tPROGRESS\tBY\tWHEN", rows)
		return nil
	case "get", "show":
		if err := need(args, 1, "bulk operation id"); err != nil {
			return err
		}
		var b api.BulkOperation
		if err := c.do("GET", "/api/v1/bulk/"+args[0], nil, &b); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(b)
		}
		fmt.Fprintf(c.out, "%s: %s by %s, %s\n", b.ID, b.Summary, dash(b.CreatedBy), ago(b.CreatedAt))
		if p := fmtProgress(b.Progress); p != "" {
			fmt.Fprintf(c.out, "Progress: %s\n", p)
		}
		var rows [][]string
		for _, t := range b.Applied {
			rows = append(rows, []string{t.ID, t.Name, dash(t.Status), ""})
		}
		for _, t := range b.Skipped {
			rows = append(rows, []string{t.ID, t.Name, "skipped", t.Reason})
		}
		c.table("DEVICE\tNAME\tSTATUS\tREASON", rows)
		return nil
	}

	// An operation: preview it, and with --yes apply it.
	fs := flag.NewFlagSet("bulk "+verb, flag.ContinueOnError)
	all := fs.Bool("all", false, "")
	channel := fs.String("channel", "", "")
	note := fs.String("note", "", "")
	yes := fs.Bool("yes", false, "")
	var devices, labels multiFlag
	fs.Var(&devices, "device", "")
	fs.Var(&labels, "label", "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	req := api.BulkRequest{All: *all, Note: *note, Selector: api.Selector{Channel: *channel, DeviceIDs: devices}}
	if req.Selector.Labels, err = labelsOf(labels); err != nil {
		return err
	}
	switch verb {
	case "reboot", "restart-agent", "lock", "wake":
		req.Operation = verb
	case "exec":
		req.Operation, req.Args = api.ActionExec, pos
		if len(pos) == 0 {
			return fmt.Errorf("give the command after --: armadactl bulk exec --label K=V -- systemctl restart myapp")
		}
	case "label":
		req.Operation = api.BulkSetLabels
		if req.Labels, err = labelsOf(pos); err != nil || len(req.Labels) == 0 {
			return fmt.Errorf("give labels to set: armadactl bulk label floor=2 --label site=sg")
		}
	case "unlabel":
		req.Operation, req.LabelKeys = api.BulkRemoveLabels, pos
		if len(pos) == 0 {
			return fmt.Errorf("give label keys to remove")
		}
	case "set-channel":
		if err := need(pos, 1, "channel"); err != nil {
			return err
		}
		req.Operation, req.Channel = api.BulkSetChannel, pos[0]
	case "lifecycle":
		if err := need(pos, 1, "state (active, spare, repair or retired)"); err != nil {
			return err
		}
		req.Operation, req.State = api.BulkLifecycle, pos[0]
	default:
		return fmt.Errorf("unknown bulk command %q (run armadactl help)", verb)
	}

	req.DryRun = true
	var prev api.BulkOperation
	if err := c.do("POST", "/api/v1/bulk", req, &prev); err != nil {
		return err
	}
	if !*yes {
		if c.json {
			return c.printJSON(prev)
		}
		fmt.Fprintf(c.out, "%s: %d devices match; it applies to %d", prev.Summary, prev.Matched, len(prev.Applied))
		if len(prev.Skipped) > 0 {
			fmt.Fprintf(c.out, " and skips %d", len(prev.Skipped))
		}
		fmt.Fprintln(c.out, ".")
		printSkipped(c, prev.Skipped)
		fmt.Fprintln(c.out, "Nothing changed. Run it again with --yes to apply.")
		return nil
	}
	req.DryRun, req.Expect = false, &prev.Matched
	var b api.BulkOperation
	if err := c.do("POST", "/api/v1/bulk", req, &b); err != nil {
		return err
	}
	if c.json {
		return c.printJSON(b)
	}
	fmt.Fprintf(c.out, "%s: applied to %d devices, skipped %d (%s)\n", b.Summary, len(b.Applied), len(b.Skipped), b.ID)
	printSkipped(c, b.Skipped)
	if b.Progress != nil {
		fmt.Fprintf(c.out, "Follow it with: armadactl bulk get %s\n", b.ID)
	}
	return nil
}

func printSkipped(c *cli, skipped []api.BulkTarget) {
	for i, t := range skipped {
		if i == 10 {
			fmt.Fprintf(c.out, "  … and %d more\n", len(skipped)-10)
			break
		}
		fmt.Fprintf(c.out, "  skip %s (%s): %s\n", t.Name, t.ID, t.Reason)
	}
}

func fmtProgress(p map[string]int) string {
	var parts []string
	for k, v := range p {
		parts = append(parts, fmt.Sprintf("%d %s", v, k))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
