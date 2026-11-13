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

func (c *cli) certs(verb string, args []string) error {
	// Device selection shared by several commands.
	selFlags := func(fs *flag.FlagSet) func() (api.Selector, bool, error) {
		all := fs.Bool("all", false, "")
		channel := fs.String("channel", "", "")
		var devices, labels multiFlag
		fs.Var(&devices, "device", "")
		fs.Var(&labels, "label", "")
		return func() (api.Selector, bool, error) {
			l, err := labelsOf(labels)
			return api.Selector{Channel: *channel, Labels: l, DeviceIDs: devices}, *all, err
		}
	}
	query := func(sel api.Selector) url.Values {
		q := url.Values{}
		for _, id := range sel.DeviceIDs {
			q.Add("device_id", id)
		}
		for k, v := range sel.Labels {
			q.Add("label", k+"="+v)
		}
		if sel.Channel != "" {
			q.Set("channel", sel.Channel)
		}
		return q
	}
	switch verb {
	case "", "list", "ls":
		fs := flag.NewFlagSet("certs list", flag.ContinueOnError)
		expiring := fs.Int("expiring", -1, "")
		waiting := fs.Bool("waiting", false, "")
		sel := selFlags(fs)
		if _, err := parse(fs, args); err != nil {
			return err
		}
		s, _, err := sel()
		if err != nil {
			return err
		}
		q := query(s)
		if *expiring >= 0 {
			q.Set("expiring_within", strconv.Itoa(*expiring))
		}
		if *waiting {
			q.Set("renewal", api.RenewalCSRReady)
		}
		var list []api.DeviceCert
		if err := c.do("GET", "/api/v1/certificates?"+q.Encode(), nil, &list); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(list)
		}
		var rows [][]string
		for _, x := range list {
			name, issuer, expires, left := dash(x.Error), "", "", "?"
			if i := x.Installed; i != nil {
				name, issuer, expires = i.CommonName, i.Issuer, i.NotAfter.Local().Format("2006-01-02")
				if !i.KeyMatches {
					name += " (KEY MISMATCH)"
				}
			}
			if x.DaysLeft != nil {
				left = strconv.Itoa(*x.DaysLeft)
			}
			renewal := "-"
			if r := x.Renewal; r != nil {
				renewal = r.Status
				if r.Message != "" {
					renewal += ": " + firstLine(r.Message)
				}
			}
			rows = append(rows, []string{x.DeviceID, x.DeviceName, x.Target, name, issuer, expires, left, renewal})
		}
		c.table("DEVICE\tNAME\tSERVICE\tCERTIFICATE\tISSUER\tEXPIRES\tDAYS\tRENEWAL", rows)
		return nil

	case "renew":
		fs := flag.NewFlagSet("certs renew", flag.ContinueOnError)
		var targets, dns, ips multiFlag
		fs.Var(&targets, "target", "")
		fs.Var(&dns, "dns", "")
		fs.Var(&ips, "ip", "")
		keyType := fs.String("key-type", "", "")
		cn := fs.String("cn", "", "")
		org := fs.String("org", "", "")
		sel := selFlags(fs)
		if _, err := parse(fs, args); err != nil {
			return err
		}
		s, all, err := sel()
		if err != nil {
			return err
		}
		req := api.RenewCertsRequest{Selector: s, All: all, Targets: targets, DNSNames: dns, IPAddresses: ips, KeyType: *keyType,
			Subject: api.CertSubject{CommonName: *cn, Organization: *org}}
		var out api.RenewCertsResponse
		if err := c.do("POST", "/api/v1/certificates/renew", req, &out); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(out)
		}
		for _, x := range out.Started {
			fmt.Fprintf(c.out, "CSR requested: %s · %s\n", x.DeviceName, x.Target)
		}
		for _, x := range out.Skipped {
			fmt.Fprintf(c.out, "skipped %s: %s\n", x.Name, x.Reason)
		}
		fmt.Fprintln(c.out, "The devices make their CSRs at their next check-in; then: armadactl certs csrs -o csrs.zip")
		return nil

	case "csrs", "csr":
		fs := flag.NewFlagSet("certs csrs", flag.ContinueOnError)
		out := fs.String("o", "", "")
		sel := selFlags(fs)
		if _, err := parse(fs, args); err != nil {
			return err
		}
		s, _, err := sel()
		if err != nil {
			return err
		}
		if *out == "" {
			*out = "csrs-" + time.Now().Format("2006-01-02") + ".zip"
		}
		req, err := http.NewRequest("GET", c.server+"/api/v1/certificates/csrs?"+query(s).Encode(), nil)
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
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(f, resp.Body); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "wrote %s (one .csr per device, and manifest.csv)\n", *out)
		return nil

	case "upload":
		if len(args) == 0 {
			return fmt.Errorf("give certificate files (PEM): armadactl certs upload signed/*.crt")
		}
		var pem strings.Builder
		for _, p := range args {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			pem.Write(b)
			pem.WriteString("\n")
		}
		var out api.UploadCertsResponse
		if err := c.do("POST", "/api/v1/certificates/upload", api.UploadCertsRequest{PEM: pem.String()}, &out); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(out)
		}
		for _, x := range out.Installing {
			fmt.Fprintf(c.out, "installing on %s · %s\n", x.DeviceName, x.Target)
		}
		for _, x := range out.Rejected {
			fmt.Fprintf(c.out, "not used: %s (serial %s): %s\n", x.Subject, x.Serial, x.Reason)
		}
		return nil

	case "email":
		fs := flag.NewFlagSet("certs email", flag.ContinueOnError)
		var to multiFlag
		fs.Var(&to, "to", "")
		note := fs.String("note", "", "")
		sel := selFlags(fs)
		if _, err := parse(fs, args); err != nil {
			return err
		}
		s, all, err := sel()
		if err != nil {
			return err
		}
		var out map[string]int
		if err := c.do("POST", "/api/v1/certificates/email", api.EmailCSRsRequest{Selector: s, All: all, Recipients: to, Note: *note}, &out); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "emailed %d CSRs to %s\n", out["sent"], strings.Join(to, ", "))
		return nil

	case "cancel":
		if err := need(args, 2, "device id and service (target)"); err != nil {
			return err
		}
		return c.do("POST", "/api/v1/certificates/"+url.PathEscape(args[0])+"/"+url.PathEscape(args[1])+"/cancel", nil, nil)
	}
	return fmt.Errorf("unknown certs command %q", verb)
}
