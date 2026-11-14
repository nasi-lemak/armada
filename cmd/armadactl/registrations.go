package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
)

func (c *cli) registrations(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var rs []api.Registration
		if err := c.do("GET", "/api/v1/registrations", nil, &rs); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(rs)
		}
		if len(rs) == 0 {
			fmt.Fprintln(c.out, "no pre-registered serials")
			return nil
		}
		for _, r := range rs {
			status := "waiting"
			if r.ClaimedBy != "" {
				status = "used by " + r.ClaimedBy
			}
			var labels []string
			for k, v := range r.Labels {
				labels = append(labels, k+"="+v)
			}
			fmt.Fprintf(c.out, "%-20s %-20s %-10s %-28s %s\n", r.Serial, r.Name, r.Channel, strings.Join(labels, " "), status)
		}
		return nil
	case "delete", "rm":
		if len(args) != 1 {
			return errors.New("usage: registrations delete SERIAL")
		}
		return c.do("DELETE", "/api/v1/registrations/"+url.PathEscape(args[0]), nil, nil)
	case "import":
		fs := flag.NewFlagSet("import", flag.ContinueOnError)
		dry := fs.Bool("dry-run", false, "")
		client := fs.String("client", "", "")
		rest, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(rest) != 1 || rest[0] == "" {
			return errors.New("usage: import FILE.csv [--dry-run] [--client K=V]")
		}
		var body []byte
		if rest[0] == "-" {
			body, err = io.ReadAll(os.Stdin)
		} else {
			body, err = os.ReadFile(rest[0])
		}
		if err != nil {
			return err
		}
		q := url.Values{}
		if *dry {
			q.Set("dry_run", "1")
		}
		if *client != "" {
			q.Set("client", *client)
		}
		req, err := http.NewRequest("POST", c.server+"/api/v1/registrations/import?"+q.Encode(), strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "text/csv")
		var res api.ImportResult
		if err := c.send(req, &res); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(res)
		}
		for _, r := range res.Rows {
			who := r.Serial
			if r.Device != "" {
				who = r.Device
			}
			detail := strings.Join(r.Changes, ", ")
			if r.Error != "" {
				detail = r.Error
			}
			fmt.Fprintf(c.out, "line %-4d %-22s %-10s %s\n", r.Line, who, r.Action, detail)
		}
		switch {
		case res.Errors > 0:
			return fmt.Errorf("%d row(s) have errors: nothing was changed", res.Errors)
		case res.DryRun:
			fmt.Fprintf(c.out, "dry run: would update %d device(s) and pre-register %d serial(s)\n", res.Updated, res.Created)
		default:
			fmt.Fprintf(c.out, "updated %d device(s), pre-registered %d serial(s)\n", res.Updated, res.Created)
		}
		return nil
	}
	return fmt.Errorf("unknown registrations command %q", verb)
}
