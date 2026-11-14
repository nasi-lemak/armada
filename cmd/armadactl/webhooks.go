package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
)

func (c *cli) webhooks(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var hooks []api.EventHook
		if err := c.do("GET", "/api/v1/event-hooks", nil, &hooks); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(hooks)
		}
		if len(hooks) == 0 {
			fmt.Fprintln(c.out, "no event webhooks")
			return nil
		}
		for _, h := range hooks {
			state := "on"
			if !h.Enabled {
				state = "off"
			}
			types := "all events"
			if len(h.Types) > 0 {
				types = strings.Join(h.Types, " ")
			}
			last := "nothing sent yet"
			if d := h.LastDelivery; d != nil {
				last = fmt.Sprintf("last %s %s", map[bool]string{true: "delivered", false: "FAILED"}[d.OK], d.EventType)
				if d.Error != "" {
					last += ": " + d.Error
				}
			}
			fmt.Fprintf(c.out, "%s  %-16s %-3s %-40s %-24s %s\n", h.ID, h.Name, state, h.URL, types, last)
			if h.DisabledReason != "" {
				fmt.Fprintf(c.out, "    %s\n", h.DisabledReason)
			}
		}
		return nil
	case "create", "add":
		fs := flag.NewFlagSet("webhooks create", flag.ContinueOnError)
		name := fs.String("name", "", "")
		u := fs.String("url", "", "")
		types := fs.String("types", "", "")
		secret := fs.String("secret", "", "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		if *name == "" || *u == "" {
			return errors.New("usage: webhooks create --name N --url https://... [--types device.*,alert.*] [--secret S]")
		}
		h := api.EventHook{Name: *name, URL: *u, Secret: *secret, Enabled: true}
		if *types != "" {
			h.Types = strings.FieldsFunc(*types, func(r rune) bool { return r == ',' || r == ' ' })
		}
		var out api.EventHook
		if err := c.do("POST", "/api/v1/event-hooks", h, &out); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(out)
		}
		fmt.Fprintf(c.out, "created %s\nsigning secret (shown once): %s\n", out.ID, out.Secret)
		return nil
	case "test", "delete", "rm", "enable", "disable", "rotate-secret":
		if len(args) != 1 {
			return fmt.Errorf("usage: webhooks %s ID", verb)
		}
		id := url.PathEscape(args[0])
		switch verb {
		case "test":
			var res struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := c.do("POST", "/api/v1/event-hooks/"+id+"/test", nil, &res); err != nil {
				return err
			}
			if !res.OK {
				return errors.New("test delivery failed: " + res.Error)
			}
			fmt.Fprintln(c.out, "test delivered")
			return nil
		case "delete", "rm":
			return c.do("DELETE", "/api/v1/event-hooks/"+id, nil, nil)
		}
		var hooks []api.EventHook
		if err := c.do("GET", "/api/v1/event-hooks", nil, &hooks); err != nil {
			return err
		}
		for _, h := range hooks {
			if h.ID != args[0] {
				continue
			}
			switch verb {
			case "enable", "disable":
				h.Enabled = verb == "enable"
			case "rotate-secret":
				h.RotateSecret = true
			}
			var out api.EventHook
			if err := c.do("PUT", "/api/v1/event-hooks/"+id, h, &out); err != nil {
				return err
			}
			if out.Secret != "" {
				fmt.Fprintf(c.out, "new signing secret (shown once): %s\n", out.Secret)
			}
			return nil
		}
		return fmt.Errorf("no event webhook %s", args[0])
	}
	return fmt.Errorf("unknown webhooks command %q", verb)
}
