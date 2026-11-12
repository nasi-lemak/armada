package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func (c *cli) whoami() error {
	var me api.Me
	if err := c.do("GET", "/api/v1/me", nil, &me); err != nil {
		return err
	}
	if c.json {
		return c.printJSON(me)
	}
	scope := "whole fleet"
	if me.Scoped {
		scope = scopeString(me.Scope)
	}
	fmt.Fprintf(c.out, "%s\nrole:  %s\nscope: %s\n", me.Name, me.Role, scope)
	return nil
}

func scopeString(sel api.Selector) string {
	var parts []string
	if sel.Channel != "" {
		parts = append(parts, "channel="+sel.Channel)
	}
	if l := fmtLabels(sel.Labels); l != "" && l != "-" {
		parts = append(parts, l)
	}
	if len(parts) == 0 {
		return "whole fleet"
	}
	return strings.Join(parts, " ")
}

// stdin is shared so successive prompts don't lose buffered input.
var stdin = bufio.NewReader(os.Stdin)

// readSecret reads one line from stdin, prompting when it's a terminal.
// Passing passwords on the command line leaves them in shell history.
func readSecret(prompt string) (string, error) {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, prompt+" (input is visible; or pipe it in): ")
	}
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no password given on stdin")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// resolveUser accepts a user ID or an email address.
func (c *cli) resolveUser(ref string) (api.User, error) {
	var us []api.User
	if err := c.do("GET", "/api/v1/users", nil, &us); err != nil {
		return api.User{}, err
	}
	for _, u := range us {
		if u.ID == ref || strings.EqualFold(u.Email, ref) {
			return u, nil
		}
	}
	return api.User{}, fmt.Errorf("no user %q", ref)
}

func (c *cli) users(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var us []api.User
		if err := c.do("GET", "/api/v1/users", nil, &us); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(us)
		}
		var rows [][]string
		for _, u := range us {
			state, last := "active", "never"
			if u.Disabled {
				state = "disabled"
			}
			if u.LastLoginAt != nil {
				last = u.LastLoginAt.Local().Format(time.DateTime)
			}
			var signIn []string
			if !u.NoPassword {
				signIn = append(signIn, "password")
			}
			if u.TwoFactor {
				signIn = append(signIn, "2FA")
			}
			if u.Passkeys > 0 {
				signIn = append(signIn, fmt.Sprintf("%d passkey(s)", u.Passkeys))
			}
			if u.SSO || u.NoPassword {
				signIn = append(signIn, "SSO")
			}
			rows = append(rows, []string{u.ID, u.Email, u.Name, u.Role, scopeString(u.Scope), strings.Join(signIn, "+"), state, last})
		}
		c.table("ID\tEMAIL\tNAME\tROLE\tSCOPE\tSIGN-IN\tSTATE\tLAST SIGN-IN", rows)
		return nil
	case "create", "add":
		fs := flag.NewFlagSet("users create", flag.ContinueOnError)
		var req api.CreateUserRequest
		var scope multiFlag
		fs.StringVar(&req.Email, "email", "", "")
		fs.StringVar(&req.Name, "name", "", "")
		fs.StringVar(&req.Role, "role", "", "")
		fs.StringVar(&req.Password, "password", "", "")
		ssoOnly := fs.Bool("sso-only", false, "")
		fs.Var(&scope, "scope", "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		if req.Email == "" || req.Role == "" {
			return errors.New("--email and --role are required")
		}
		var err error
		if req.Scope.Labels, err = labelsOf(scope); err != nil {
			return err
		}
		if req.Password == "" && !*ssoOnly {
			if req.Password, err = readSecret("Initial password"); err != nil {
				return err
			}
		}
		var u api.User
		if err := c.do("POST", "/api/v1/users", req, &u); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(u)
		}
		fmt.Fprintf(c.out, "user %s created (%s, %s)\n", u.ID, u.Role, scopeString(u.Scope))
		return nil
	case "set", "update":
		fs := flag.NewFlagSet("users set", flag.ContinueOnError)
		var role, name string
		var scope multiFlag
		var whole, disable, enable, reset, reset2FA, unlinkSSO bool
		fs.BoolVar(&reset2FA, "reset-2fa", false, "")
		fs.BoolVar(&unlinkSSO, "unlink-sso", false, "")
		fs.StringVar(&role, "role", "", "")
		fs.StringVar(&name, "name", "", "")
		fs.Var(&scope, "scope", "")
		fs.BoolVar(&whole, "whole-fleet", false, "")
		fs.BoolVar(&disable, "disable", false, "")
		fs.BoolVar(&enable, "enable", false, "")
		fs.BoolVar(&reset, "reset-password", false, "")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "user id or email"); err != nil {
			return err
		}
		u, err := c.resolveUser(pos[0])
		if err != nil {
			return err
		}
		body := map[string]any{}
		if role != "" {
			body["role"] = role
		}
		if name != "" {
			body["name"] = name
		}
		if len(scope) > 0 || whole {
			labels, err := labelsOf(scope)
			if err != nil {
				return err
			}
			body["scope"] = api.Selector{Labels: labels}
		}
		if disable || enable {
			body["disabled"] = disable
		}
		if reset {
			pw, err := readSecret("New password for " + u.Email)
			if err != nil {
				return err
			}
			body["password"] = pw
		}
		if reset2FA {
			body["reset_two_factor"] = true
		}
		if unlinkSSO {
			body["unlink_sso"] = true
		}
		if len(body) == 0 {
			return errors.New("nothing to change")
		}
		if err := c.do("PATCH", "/api/v1/users/"+url.PathEscape(u.ID), body, &u); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(u)
		}
		fmt.Fprintf(c.out, "updated %s (%s, %s)\n", u.Email, u.Role, scopeString(u.Scope))
		return nil
	case "delete", "rm":
		if err := need(args, 1, "user id or email"); err != nil {
			return err
		}
		u, err := c.resolveUser(args[0])
		if err != nil {
			return err
		}
		return c.simpleDelete("user", "/api/v1/users/", []string{u.ID})
	}
	return fmt.Errorf("unknown users command %q", verb)
}

func (c *cli) meCmd(verb string, args []string) error {
	switch verb {
	case "", "show":
		return c.whoami()
	case "password":
		cur, err := readSecret("Current password")
		if err != nil {
			return err
		}
		nw, err := readSecret("New password")
		if err != nil {
			return err
		}
		if err := c.do("POST", "/api/v1/me/password", map[string]string{"current": cur, "new": nw}, nil); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "password changed; dashboard sessions were signed out (personal tokens keep working)")
		return nil
	case "tokens", "token":
		sub := ""
		if len(args) > 0 {
			sub, args = args[0], args[1:]
		}
		switch sub {
		case "", "list", "ls":
			var ts []api.APIToken
			if err := c.do("GET", "/api/v1/me/tokens", nil, &ts); err != nil {
				return err
			}
			if c.json {
				return c.printJSON(ts)
			}
			var rows [][]string
			for _, t := range ts {
				last := "never"
				if t.LastUsedAt != nil {
					last = t.LastUsedAt.Local().Format(time.DateTime)
				}
				rows = append(rows, []string{t.ID, t.Name, t.CreatedAt.Local().Format(time.DateTime), last})
			}
			c.table("ID\tNAME\tCREATED\tLAST USED", rows)
			return nil
		case "create", "new":
			fs := flag.NewFlagSet("me tokens create", flag.ContinueOnError)
			name := fs.String("name", "", "")
			if _, err := parse(fs, args); err != nil {
				return err
			}
			if *name == "" {
				return errors.New("--name is required")
			}
			var t api.APIToken
			if err := c.do("POST", "/api/v1/me/tokens", map[string]string{"name": *name}, &t); err != nil {
				return err
			}
			if c.json {
				return c.printJSON(t)
			}
			fmt.Fprintf(c.out, "%s\n\nCopy it now; it is not shown again. Use it with --token or ARMADA_TOKEN.\n", t.Token)
			return nil
		case "revoke", "delete", "rm":
			return c.simpleDelete("token", "/api/v1/me/tokens/", args)
		}
		return fmt.Errorf("unknown me tokens command %q", sub)
	}
	return fmt.Errorf("unknown me command %q", verb)
}

// deviceLogs queues a logs action and waits for the device's answer.
func (c *cli) deviceLogs(args []string) error {
	fs := flag.NewFlagSet("devices logs", flag.ContinueOnError)
	unit := fs.String("unit", "", "")
	file := fs.String("file", "", "")
	lines := fs.Int("lines", 200, "")
	since := fs.String("since", "", "")
	grep := fs.String("grep", "", "")
	timeout := fs.Duration("timeout", 2*time.Minute, "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "device id"); err != nil {
		return err
	}
	req := api.CreateActionRequest{Type: api.ActionLogs, Args: []string{"journal"}}
	if *file != "" {
		req.Args = []string{"file", "name=" + *file}
	} else if *unit != "" {
		req.Args = append(req.Args, "unit="+*unit)
	}
	req.Args = append(req.Args, fmt.Sprintf("lines=%d", *lines))
	if *since != "" {
		req.Args = append(req.Args, "since="+*since)
	}
	if *grep != "" {
		req.Args = append(req.Args, "grep="+*grep)
	}
	path := "/api/v1/devices/" + url.PathEscape(pos[0]) + "/actions"
	var a api.Action
	if err := c.do("POST", path, req, &a); err != nil {
		return err
	}
	deadline := time.Now().Add(*timeout)
	for {
		time.Sleep(700 * time.Millisecond)
		var acts []api.Action
		if err := c.do("GET", path, nil, &acts); err != nil {
			return err
		}
		for _, x := range acts {
			if x.ID != a.ID || x.Status == api.ActionQueued || x.Status == api.ActionSent {
				continue
			}
			if c.json {
				return c.printJSON(x)
			}
			fmt.Fprintln(c.out, x.Output)
			if x.Status != api.ActionSucceeded {
				return fmt.Errorf("device could not read logs (%s)", x.Status)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no answer within %s; the device may be offline. The result will appear in 'armadactl devices get %s' (action %s)", *timeout, pos[0], a.ID)
		}
	}
}

// parseStartAt accepts RFC 3339 or "YYYY-MM-DD HH:MM" in tz (default local).
func parseStartAt(s, tz string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	loc := time.Local
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return time.Time{}, fmt.Errorf("unknown time zone %q", tz)
		}
		loc = l
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid --start-at %q (use \"2026-12-01 02:00\" or RFC 3339)", s)
}

func (c *cli) scim(verb string, args []string) error {
	if verb != "tokens" && verb != "token" {
		return fmt.Errorf("unknown scim command %q (try: scim tokens)", verb)
	}
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "", "list", "ls":
		var ts []api.SCIMToken
		if err := c.do("GET", "/api/v1/scim/tokens", nil, &ts); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(ts)
		}
		var rows [][]string
		for _, t := range ts {
			last := "never"
			if t.LastUsedAt != nil {
				last = t.LastUsedAt.Local().Format(time.DateTime)
			}
			rows = append(rows, []string{t.ID, t.Name, t.CreatedAt.Local().Format(time.DateTime), last})
		}
		c.table("ID\tNAME\tCREATED\tLAST SYNC", rows)
		return nil
	case "create", "new":
		fs := flag.NewFlagSet("scim tokens create", flag.ContinueOnError)
		name := fs.String("name", "", "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		var t api.SCIMToken
		if err := c.do("POST", "/api/v1/scim/tokens", map[string]string{"name": *name}, &t); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(t)
		}
		fmt.Fprintf(c.out, "SCIM base URL: %s\nToken:         %s\n\nEnter both in your identity provider's provisioning settings. The token is not shown again.\n", t.URL, t.Token)
		return nil
	case "revoke", "delete", "rm":
		return c.simpleDelete("SCIM token", "/api/v1/scim/tokens/", args)
	}
	return fmt.Errorf("unknown scim tokens command %q", sub)
}
