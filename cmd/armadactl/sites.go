package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/nasi-lemak/armada/internal/api"
)

// sites: on a central server, the other servers reporting to it.
func (c *cli) sites(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var sites []api.Site
		if err := c.do("GET", "/api/v1/sites", nil, &sites); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(sites)
		}
		if len(sites) == 0 {
			fmt.Fprintln(c.out, "no sites")
			return nil
		}
		for _, s := range sites {
			line := fmt.Sprintf("%s  %-28s %-6s", s.ID, s.Name, s.State)
			if sn := s.Snapshot; sn != nil {
				behind := 0
				for _, p := range sn.Insights.Software {
					behind += len(p.Behind)
				}
				line += fmt.Sprintf("  %d/%d online  %d alerts  %d behind  %d certs due  %d disconnections  report %s",
					sn.Summary.Online, sn.Summary.Online+sn.Summary.Offline, sn.Summary.FiringAlerts, behind,
					len(sn.Insights.Certificates.Items), sn.Insights.Connectivity.Outages, sn.GeneratedAt.Local().Format("01-02 15:04"))
			}
			fmt.Fprintln(c.out, line)
		}
		return nil
	case "add":
		if len(args) < 1 {
			return errors.New("usage: sites add NAME [NOTE]")
		}
		req := api.Site{Name: args[0]}
		if len(args) > 1 {
			req.Note = args[1]
		}
		var s api.Site
		if err := c.do("POST", "/api/v1/sites", req, &s); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "added %s\nsite token (shown once): %s\non the site's server: ARMADA_CENTRAL_TOKEN=%s armada-server ... -central-url %s\n", s.ID, s.Token, s.Token, c.server)
		return nil
	case "token":
		if len(args) != 1 {
			return errors.New("usage: sites token ID")
		}
		var s api.Site
		if err := c.do("POST", "/api/v1/sites/"+url.PathEscape(args[0])+"/token", nil, &s); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "new site token (shown once): %s\n", s.Token)
		return nil
	case "delete", "rm":
		if len(args) != 1 {
			return errors.New("usage: sites delete ID")
		}
		return c.do("DELETE", "/api/v1/sites/"+url.PathEscape(args[0]), nil, nil)
	case "import":
		if len(args) != 2 {
			return errors.New("usage: sites import ID SNAPSHOT.json (made with armadactl site export on the site)")
		}
		var body []byte
		var err error
		if args[1] == "-" {
			body, err = io.ReadAll(os.Stdin)
		} else {
			body, err = os.ReadFile(args[1])
		}
		if err != nil {
			return err
		}
		req, err := http.NewRequest("POST", c.server+"/api/v1/sites/"+url.PathEscape(args[0])+"/import", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		var s api.Site
		if err := c.send(req, &s); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "imported the snapshot of %s from %s\n", s.Name, s.Snapshot.GeneratedAt.Local().Format("2006-01-02 15:04"))
		return nil
	}
	return fmt.Errorf("unknown sites command %q", verb)
}

// site: this server's side, reporting to a central one.
func (c *cli) site(verb string, args []string) error {
	switch verb {
	case "export":
		w := c.out
		if len(args) == 2 && args[0] == "-o" && args[1] != "-" {
			f, err := os.Create(args[1])
			if err != nil {
				return err
			}
			defer f.Close()
			w = f
		}
		return c.download("/api/v1/site/snapshot", w)
	case "", "status":
		var st api.CentralStatus
		if err := c.do("GET", "/api/v1/site/central", nil, &st); err != nil {
			return err
		}
		if st.URL == "" {
			fmt.Fprintln(c.out, "not reporting to a central server (-central-url)")
			return nil
		}
		fmt.Fprintf(c.out, "reporting to %s\n", st.URL)
		if st.LastOKAt != nil {
			fmt.Fprintf(c.out, "last report: %s\n", st.LastOKAt.Local().Format("2006-01-02 15:04:05"))
		}
		if st.LastError != "" {
			return errors.New("last attempt failed: " + st.LastError)
		}
		return nil
	}
	return fmt.Errorf("unknown site command %q", verb)
}
