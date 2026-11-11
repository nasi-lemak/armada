package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
)

// profileFile is a profile as written in a file for "config apply". Files
// may give content_file (relative to the profile file) instead of content.
type profileFile struct {
	api.ConfigProfile
	Files []struct {
		api.ConfigFile
		ContentFile string `json:"content_file,omitempty"`
	} `json:"files,omitempty"`
	Enabled *bool `json:"enabled,omitempty"`
}

func (c *cli) config(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var ps []api.ConfigProfile
		if err := c.do("GET", "/api/v1/config-profiles", nil, &ps); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(ps)
		}
		var rows [][]string
		for _, p := range ps {
			state := "enabled"
			if !p.Enabled {
				state = "disabled"
			}
			key := p.Kind
			if p.Target != "" {
				key += ":" + p.Target
			}
			rows = append(rows, []string{p.ID, p.Name, key, scopeString(p.Selector), strconv.Itoa(p.Priority), state,
				fmt.Sprintf("%d applied, %d pending, %d failed", p.Compliance[api.ConfigApplied], p.Compliance[api.ConfigPending], p.Compliance[api.ConfigFailed])})
		}
		c.table("ID\tNAME\tCONFIGURES\tDEVICES\tPRIORITY\tSTATE\tCOMPLIANCE", rows)
		return nil
	case "get", "show":
		p, err := c.findProfile(args)
		if err != nil {
			return err
		}
		return c.printJSON(p)
	case "apply":
		fs := flag.NewFlagSet("config apply", flag.ContinueOnError)
		file := fs.String("f", "", "")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if *file == "" && len(pos) > 0 {
			*file = pos[0]
		}
		if *file == "" {
			return errors.New("usage: armadactl config apply -f profile.json")
		}
		return c.applyProfiles(*file)
	case "delete", "rm":
		p, err := c.findProfile(args)
		if err != nil {
			return err
		}
		return c.simpleDelete("profile", "/api/v1/config-profiles/", []string{p.ID})
	case "enable", "disable":
		p, err := c.findProfile(args)
		if err != nil {
			return err
		}
		p.Enabled = verb == "enable"
		if err := c.do("PUT", "/api/v1/config-profiles/"+url.PathEscape(p.ID), p, nil); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "%sd %s\n", verb, p.Name)
		return nil
	}
	return fmt.Errorf("unknown config command %q", verb)
}

// findProfile resolves an ID or name.
func (c *cli) findProfile(args []string) (*api.ConfigProfile, error) {
	if err := need(args, 1, "profile id or name"); err != nil {
		return nil, err
	}
	var ps []api.ConfigProfile
	if err := c.do("GET", "/api/v1/config-profiles", nil, &ps); err != nil {
		return nil, err
	}
	for i := range ps {
		if ps[i].ID == args[0] || strings.EqualFold(ps[i].Name, args[0]) {
			return &ps[i], nil
		}
	}
	return nil, fmt.Errorf("no profile %q", args[0])
}

// applyProfiles creates or updates (by name) the profiles in a JSON file
// holding one profile or a list.
func (c *cli) applyProfiles(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var list []profileFile
	if strings.HasPrefix(strings.TrimSpace(string(b)), "[") {
		err = json.Unmarshal(b, &list)
	} else {
		var one profileFile
		err = json.Unmarshal(b, &one)
		list = []profileFile{one}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var existing []api.ConfigProfile
	if err := c.do("GET", "/api/v1/config-profiles", nil, &existing); err != nil {
		return err
	}
	for _, pf := range list {
		p := pf.ConfigProfile
		p.Enabled = pf.Enabled == nil || *pf.Enabled
		p.Files = nil
		for _, f := range pf.Files {
			if f.ContentFile != "" {
				src := f.ContentFile
				if !filepath.IsAbs(src) {
					src = filepath.Join(filepath.Dir(path), src)
				}
				data, err := os.ReadFile(src)
				if err != nil {
					return err
				}
				f.Content = string(data)
			}
			p.Files = append(p.Files, f.ConfigFile)
		}
		method, target, verb := "POST", "/api/v1/config-profiles", "created"
		for _, e := range existing {
			if strings.EqualFold(e.Name, p.Name) {
				method, target, verb = "PUT", "/api/v1/config-profiles/"+url.PathEscape(e.ID), "updated"
			}
		}
		var out api.ConfigProfile
		if err := c.do(method, target, p, &out); err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		fmt.Fprintf(c.out, "%s %s (%s)\n", verb, out.Name, out.ID)
	}
	return nil
}
