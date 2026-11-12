// Command armadactl is the command-line client for the Armada admin API.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

const usage = `armadactl - manage an Armada device fleet

Usage:
  armadactl [--server URL] [--token TOKEN] [--json] <command> [args]

Connection (flags or environment):
  --server   ARMADA_SERVER        server URL (default http://localhost:8080)
  --token    ARMADA_TOKEN         a personal API token (ut_..., from the dashboard's My account
                                  page or "armadactl me tokens create") or the admin token;
                                  ARMADA_ADMIN_TOKEN is also read

Commands:
  status                                     fleet overview
  devices list [--channel C] [--label K=V] [--status online|offline]
  devices get <id>                           details, update history, actions
  devices set <id> [--name N] [--channel C] [--label K=V]... [--unlabel K]...
  devices logs <id> [--unit U | --file NAME] [--lines 200] [--since 1h] [--grep TEXT] [--timeout 2m]
                                             fetch recent journal or log-file lines (waits for the answer)
  devices wake <id>                          ask the device to check in now
  devices action <id> lock                   Android: lock the screen
  devices action <id> wipe confirm=<id>      Android: factory reset (lost or stolen devices)
  devices delete <id>
  devices action <id> reboot|restart-agent
  devices action <id> exec -- <command> [args...]
  tokens list
  tokens create [--channel C] [--label K=V]... [--max-uses N] [--ttl 24h] [--description D]
  tokens revoke <id>
  releases list [--package P]
  releases upload --package P --version V [--channel C] [--os O] [--arch A]
                  [--notes N] [--version-code N] [--auto-deploy] <file>
  releases delete <id>
  deployments list [--status running,paused]
  deployments get <id>
  deployments create --release <id> [--name N] [--channel C] [--label K=V]...
                  [--device ID]... [--stages 5,25,100] [--stage-duration 15m]
                  [--max-failure-rate 0.2] [--min-samples 3] [--manual]
                  [--allow-downgrade] [--start-at "2026-12-01 02:00"]
                  [--windows 22:00-05:00] [--days mon,tue,wed,thu,fri] [--tz Asia/Singapore]
  deployments pause|resume|advance|retry|complete|abort <id>
  events [--device ID] [--deployment ID] [--limit N]

Monitoring and alerts:
  monitors list
  monitors create --name N --type http|tcp|systemd|process|command [--target T]
                  [--interval 60] [--contains S] [--label K=V]... [-- command args]
  monitors delete <id>
  channels list
  channels create --name N --type webhook --url URL [--secret S] [--header "K: V"]... [--template T]
  channels create --name N --type slack --url HOOK_URL
  channels create --name N --type email --smtp-host H [--smtp-port 587] [--smtp-user U --smtp-password P]
                  --from ADDR --to ADDR [--to ADDR]...
  channels test <id>
  channels delete <id>
  rules list
  rules create --name N --condition health|device_offline|update_failed|deployment_paused
               --channel ID [--channel ID]... [--min-severity warning|critical] [--kind storage]...
               [--offline-minutes 10] [--repeat-minutes 0] [--label K=V]... [--no-resolved]
  rules delete <id>
  alerts [--firing]
  history <device-id> [--range 24h|7d|30d|90d] [--check NAME] [--csv]
  backup [-o FILE]                           download a full server backup (database, keys, artifacts)
  history checks                             check names with history, and how many devices have each
  history compare --check NAME [--range 7d] [--label K=V]... [--channel C] [--csv]

Configuration profiles:
  config list                                profiles and how many devices applied each
  config get <id|name>
  config apply -f profile.json               create or update by name (one profile or a list;
                                             files may use "content_file": "./app.conf")
  config enable|disable|delete <id|name>

Users and access:
  whoami                                     who the token belongs to, role and scope
  users list
  users create --email E --role viewer|operator|admin [--name N] [--scope K=V]...
               [--password P | --sso-only]   password is read from stdin if omitted
  users set <id|email> [--role R] [--name N] [--scope K=V]... [--whole-fleet]
                       [--disable|--enable] [--reset-password]
                       [--reset-2fa]          turn off their authenticator (lost phone)
                       [--unlink-sso]         forget their single sign-on identity
  users delete <id|email>
  groups [list]                              directory group mappings
  groups map --group G --role R [--scope K=V]...
                                             members get role R, limited to matching devices
                                             (no --scope: the whole fleet)
  groups unmap <id|group>
  scim tokens [list]                         SCIM tokens for directory sync
  scim tokens create --name N                prints the SCIM URL and token once
  scim tokens revoke <id>
  me password                                change your password (prompts on stdin)
  me tokens [list]
  me tokens create --name N                  prints the token once
  me tokens revoke <id>

Android:
  apps list
  apps set <package> --signer SHA256 [--signer ...] [--name N] [--license] [--license-ttl 1h]
  apps delete <package>
  devices revoke <id>                        stop issuing app licenses to a device
  devices reinstate <id>
  android qr --token et_... [--wifi-ssid S --wifi-password P] [--out qr.png]
`

type cli struct {
	server string
	token  string
	json   bool
	out    io.Writer
}

func main() {
	c := &cli{out: os.Stdout}
	fs := flag.NewFlagSet("armadactl", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.StringVar(&c.server, "server", envOr("ARMADA_SERVER", "http://localhost:8080"), "")
	fs.StringVar(&c.token, "token", envOr("ARMADA_TOKEN", os.Getenv("ARMADA_ADMIN_TOKEN")), "")
	fs.BoolVar(&c.json, "json", false, "")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	c.server = strings.TrimRight(c.server, "/")
	if err := c.run(fs.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (c *cli) run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return nil
	}
	noun, rest := args[0], args[1:]
	verb := ""
	if len(rest) > 0 {
		verb, rest = rest[0], rest[1:]
	}
	switch noun {
	case "status":
		return c.status()
	case "events":
		return c.events(append([]string{verb}, rest...))
	case "devices", "device":
		return c.devices(verb, rest)
	case "tokens", "token":
		return c.tokens(verb, rest)
	case "releases", "release":
		return c.releases(verb, rest)
	case "apps", "app":
		return c.apps(verb, rest)
	case "monitors", "monitor":
		return c.monitors(verb, rest)
	case "channels", "channel":
		return c.channels(verb, rest)
	case "rules", "rule":
		return c.rules(verb, rest)
	case "alerts":
		return c.alerts(append([]string{verb}, rest...))
	case "history":
		return c.history(verb, rest)
	case "backup":
		return c.backup(append([]string{verb}, rest...))
	case "android":
		return c.android(verb, rest)
	case "config", "profiles", "profile":
		return c.config(verb, rest)
	case "whoami":
		return c.whoami()
	case "scim":
		return c.scim(verb, rest)
	case "groups", "group":
		return c.groups(verb, rest)
	case "users", "user":
		return c.users(verb, rest)
	case "me":
		return c.meCmd(verb, rest)
	case "deployments", "deployment", "deploy":
		return c.deployments(verb, rest)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, usage)
		return nil
	}
	return fmt.Errorf("unknown command %q (run armadactl help)", noun)
}

// ---- flag helpers ----

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func labelsOf(m multiFlag) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range m {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("label %q must be KEY=VALUE", kv)
		}
		out[k] = v
	}
	return out, nil
}

// parse parses flags that may appear before or after positional arguments.
// Everything after "--" is positional.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func need(pos []string, n int, what string) error {
	if len(pos) < n {
		return fmt.Errorf("missing %s", what)
	}
	return nil
}

// ---- HTTP ----

func (c *cli) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.server+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(req, out)
}

func (c *cli) send(req *http.Request, out any) error {
	if c.token == "" {
		return errors.New("no admin token: set ARMADA_ADMIN_TOKEN or pass --token")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var er api.ErrorResponse
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &er) == nil && er.Error != "" {
			return fmt.Errorf("%s (HTTP %d)", er.Error, resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *cli) printJSON(v any) error {
	enc := json.NewEncoder(c.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (c *cli) table(header string, rows [][]string) {
	tw := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func fmtLabels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

func fmtPackages(m map[string]string) string {
	if len(m) == 0 {
		return "-"
	}
	return strings.ReplaceAll(fmtLabels(m), "=", "@")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- commands ----

func (c *cli) status() error {
	var s api.FleetSummary
	if err := c.do("GET", "/api/v1/summary", nil, &s); err != nil {
		return err
	}
	if c.json {
		return c.printJSON(s)
	}
	fmt.Fprintf(c.out, "Devices:      %d (%d online, %d offline)\n", s.Devices, s.Online, s.Offline)
	fmt.Fprintf(c.out, "Releases:     %d\n", s.Releases)
	fmt.Fprintf(c.out, "Deployments:  %d active\n", s.ActiveDeployments)
	pkgs := make([]string, 0, len(s.PackageVersions))
	for p := range s.PackageVersions {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		var parts []string
		for v, n := range s.PackageVersions[p] {
			parts = append(parts, fmt.Sprintf("%s ×%d", v, n))
		}
		sort.Strings(parts)
		fmt.Fprintf(c.out, "  %-20s %s\n", p, strings.Join(parts, ", "))
	}
	return nil
}

func (c *cli) devices(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		fs := flag.NewFlagSet("devices list", flag.ContinueOnError)
		channel := fs.String("channel", "", "")
		status := fs.String("status", "", "")
		var labels multiFlag
		fs.Var(&labels, "label", "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		q := url.Values{}
		if *channel != "" {
			q.Set("channel", *channel)
		}
		if *status != "" {
			q.Set("status", *status)
		}
		for _, l := range labels {
			q.Add("label", l)
		}
		var devs []api.Device
		if err := c.do("GET", "/api/v1/devices?"+q.Encode(), nil, &devs); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(devs)
		}
		var rows [][]string
		for _, d := range devs {
			platform := d.Info.OS + "/" + d.Info.Arch
			if d.ParentID != "" {
				platform = "ble:" + d.Info.Model
			}
			health := d.Health
			if health == "" {
				health = "ok"
			}
			rows = append(rows, []string{d.ID, d.Name, d.Status, health, d.Channel, platform, fmtPackages(d.Packages), fmtLabels(d.Labels), ago(d.LastSeenAt)})
		}
		c.table("ID\tNAME\tSTATUS\tHEALTH\tCHANNEL\tPLATFORM\tPACKAGES\tLABELS\tLAST SEEN", rows)
		return nil
	case "get", "show":
		if err := need(args, 1, "device id"); err != nil {
			return err
		}
		var det struct {
			Device  api.Device          `json:"device"`
			Updates []*api.DeviceUpdate `json:"updates"`
			Actions []*api.Action       `json:"actions"`
		}
		if err := c.do("GET", "/api/v1/devices/"+url.PathEscape(args[0]), nil, &det); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(det)
		}
		d := det.Device
		fmt.Fprintf(c.out, "ID:        %s\nName:      %s\nStatus:    %s (last seen %s)\nChannel:   %s\nLabels:    %s\n",
			d.ID, d.Name, d.Status, ago(d.LastSeenAt), d.Channel, fmtLabels(d.Labels))
		fmt.Fprintf(c.out, "Host:      %s (%s/%s, kernel %s)\nIPs:       %s\nPackages:  %s\n",
			d.Info.Hostname, d.Info.OS, d.Info.Arch, dash(d.Info.Kernel), dash(strings.Join(d.Info.IPs, ", ")), fmtPackages(d.Packages))
		if d.AgentStatus != "" {
			fmt.Fprintf(c.out, "Agent:     %s\n", d.AgentStatus)
		}
		m := d.Metrics
		fmt.Fprintf(c.out, "Uptime:    %s   Load: %.2f   Mem free: %s / %s   Disk free: %s / %s\n",
			time.Duration(m.UptimeSeconds)*time.Second, m.Load1, bytesH(m.MemAvailableBytes), bytesH(m.MemTotalBytes), bytesH(m.DiskFreeBytes), bytesH(m.DiskTotalBytes))
		if len(det.Updates) > 0 {
			fmt.Fprintln(c.out, "\nUpdates:")
			var rows [][]string
			for _, u := range det.Updates {
				rows = append(rows, []string{u.Package, dash(u.FromVersion) + " → " + u.Version, u.Status, ago(u.UpdatedAt), u.Message})
			}
			c.table("PACKAGE\tVERSION\tSTATUS\tUPDATED\tMESSAGE", rows)
		}
		if len(det.Actions) > 0 {
			fmt.Fprintln(c.out, "\nActions:")
			var rows [][]string
			for _, a := range det.Actions {
				rows = append(rows, []string{a.ID, a.Type + " " + strings.Join(a.Args, " "), a.Status, ago(a.UpdatedAt), firstLine(a.Output)})
			}
			c.table("ID\tACTION\tSTATUS\tUPDATED\tOUTPUT", rows)
		}
		return nil
	case "set", "label":
		fs := flag.NewFlagSet("devices set", flag.ContinueOnError)
		name := fs.String("name", "", "")
		channel := fs.String("channel", "", "")
		var labels, unlabels multiFlag
		fs.Var(&labels, "label", "")
		fs.Var(&unlabels, "unlabel", "")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "device id"); err != nil {
			return err
		}
		// "devices label <id> k=v k2-" shorthand.
		for _, p := range pos[1:] {
			if k, ok := strings.CutSuffix(p, "-"); ok && !strings.Contains(p, "=") {
				unlabels = append(unlabels, k)
			} else {
				labels = append(labels, p)
			}
		}
		set, err := labelsOf(labels)
		if err != nil {
			return err
		}
		body := map[string]any{"set_labels": set, "remove_labels": []string(unlabels)}
		if *name != "" {
			body["name"] = *name
		}
		if *channel != "" {
			body["channel"] = *channel
		}
		var d api.Device
		if err := c.do("PATCH", "/api/v1/devices/"+url.PathEscape(pos[0]), body, &d); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(d)
		}
		fmt.Fprintf(c.out, "%s updated: name=%s channel=%s labels=%s\n", d.ID, d.Name, d.Channel, fmtLabels(d.Labels))
		return nil
	case "delete", "rm":
		if err := need(args, 1, "device id"); err != nil {
			return err
		}
		if err := c.do("DELETE", "/api/v1/devices/"+url.PathEscape(args[0]), nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "deleted", args[0])
		return nil
	case "revoke", "reinstate":
		if err := need(args, 1, "device id"); err != nil {
			return err
		}
		var d api.Device
		if err := c.do("PATCH", "/api/v1/devices/"+url.PathEscape(args[0]), map[string]any{"license_revoked": verb == "revoke"}, &d); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(d)
		}
		if d.LicenseRevoked {
			fmt.Fprintf(c.out, "%s: licenses revoked; its apps stop working when their current license expires\n", d.ID)
		} else {
			fmt.Fprintf(c.out, "%s: licenses reinstated\n", d.ID)
		}
		return nil
	case "action", "run":
		if err := need(args, 2, "device id and action type"); err != nil {
			return err
		}
		req := api.CreateActionRequest{Type: args[1]}
		if len(args) > 2 {
			req.Args = args[2:]
			if req.Args[0] == "--" {
				req.Args = req.Args[1:]
			}
		}
		var a api.Action
		if err := c.do("POST", "/api/v1/devices/"+url.PathEscape(args[0])+"/actions", req, &a); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(a)
		}
		fmt.Fprintf(c.out, "queued %s (%s); a connected device runs it within seconds, others at their next check-in\n", a.ID, a.Type)
		return nil
	case "logs", "log":
		return c.deviceLogs(args)
	case "wake":
		if err := need(args, 1, "device id"); err != nil {
			return err
		}
		var wr api.WakeResponse
		if err := c.do("POST", "/api/v1/devices/"+url.PathEscape(args[0])+"/wake", nil, &wr); err != nil {
			return err
		}
		if wr.Connected {
			fmt.Fprintln(c.out, "device is checking in now")
		} else {
			fmt.Fprintln(c.out, "device has no live connection; it checks in at its next heartbeat")
		}
		return nil
	}
	return fmt.Errorf("unknown devices command %q", verb)
}

func (c *cli) tokens(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var ts []api.EnrollmentToken
		if err := c.do("GET", "/api/v1/tokens", nil, &ts); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(ts)
		}
		var rows [][]string
		for _, t := range ts {
			uses := strconv.Itoa(t.Uses)
			if t.MaxUses > 0 {
				uses += "/" + strconv.Itoa(t.MaxUses)
			}
			state := "active"
			switch {
			case t.Revoked:
				state = "revoked"
			case t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt):
				state = "expired"
			case t.MaxUses > 0 && t.Uses >= t.MaxUses:
				state = "used up"
			}
			exp := "never"
			if t.ExpiresAt != nil {
				exp = t.ExpiresAt.Local().Format("2006-01-02 15:04")
			}
			rows = append(rows, []string{t.ID, state, t.Channel, fmtLabels(t.Labels), uses, exp, dash(t.Description)})
		}
		c.table("ID\tSTATE\tCHANNEL\tLABELS\tUSES\tEXPIRES\tDESCRIPTION", rows)
		return nil
	case "create", "new":
		fs := flag.NewFlagSet("tokens create", flag.ContinueOnError)
		req := api.CreateTokenRequest{}
		fs.StringVar(&req.Channel, "channel", "stable", "")
		fs.StringVar(&req.Description, "description", "", "")
		fs.IntVar(&req.MaxUses, "max-uses", 0, "")
		ttl := fs.Duration("ttl", 0, "")
		var labels multiFlag
		fs.Var(&labels, "label", "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		var err error
		if req.Labels, err = labelsOf(labels); err != nil {
			return err
		}
		req.TTLSeconds = int64(ttl.Seconds())
		var t api.EnrollmentToken
		if err := c.do("POST", "/api/v1/tokens", req, &t); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(t)
		}
		fmt.Fprintf(c.out, "Enrollment token %s created (channel %s). Save it now; it is not shown again:\n\n  %s\n\n", t.ID, t.Channel, t.Token)
		fmt.Fprintf(c.out, "Enroll a device:\n  armada-agent --server %s --token %s\n", c.server, t.Token)
		return nil
	case "revoke", "delete", "rm":
		if err := need(args, 1, "token id"); err != nil {
			return err
		}
		if err := c.do("DELETE", "/api/v1/tokens/"+url.PathEscape(args[0]), nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "revoked", args[0])
		return nil
	}
	return fmt.Errorf("unknown tokens command %q", verb)
}

func (c *cli) releases(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		fs := flag.NewFlagSet("releases list", flag.ContinueOnError)
		pkg := fs.String("package", "", "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		var rels []api.Release
		if err := c.do("GET", "/api/v1/releases?package="+url.QueryEscape(*pkg), nil, &rels); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(rels)
		}
		var rows [][]string
		for _, r := range rels {
			plat := dash(r.OS) + "/" + dash(r.Arch)
			if r.OS == "" && r.Arch == "" {
				plat = "any"
			}
			rows = append(rows, []string{r.ID, r.Package, r.Version, r.Channel, plat, bytesH(uint64(r.Size)), r.SHA256[:12], ago(r.CreatedAt)})
		}
		c.table("ID\tPACKAGE\tVERSION\tCHANNEL\tPLATFORM\tSIZE\tSHA256\tCREATED", rows)
		return nil
	case "upload", "create", "push":
		fs := flag.NewFlagSet("releases upload", flag.ContinueOnError)
		fields := map[string]*string{}
		for _, f := range []string{"package", "version", "channel", "os", "arch", "notes", "version-code"} {
			fields[f] = fs.String(f, "", "")
		}
		autoDeploy := fs.Bool("auto-deploy", false, "")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "artifact file"); err != nil {
			return err
		}
		if *fields["package"] == "" || *fields["version"] == "" {
			return errors.New("--package and --version are required")
		}
		return c.upload(fields, *autoDeploy, pos[0])
	case "delete", "rm":
		if err := need(args, 1, "release id"); err != nil {
			return err
		}
		if err := c.do("DELETE", "/api/v1/releases/"+url.PathEscape(args[0]), nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "deleted", args[0])
		return nil
	}
	return fmt.Errorf("unknown releases command %q", verb)
}

func (c *cli) upload(fields map[string]*string, autoDeploy bool, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			keys := []string{"package", "version", "channel", "os", "arch", "notes", "version-code"}
			for _, k := range keys {
				if v := *fields[k]; v != "" {
					if err := mw.WriteField(strings.ReplaceAll(k, "-", "_"), v); err != nil {
						return err
					}
				}
			}
			if err := mw.WriteField("auto_deploy", strconv.FormatBool(autoDeploy)); err != nil {
				return err
			}
			part, err := mw.CreateFormFile("file", filepath.Base(path))
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequest("POST", c.server+"/api/v1/releases", pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var res struct {
		Release    api.Release     `json:"release"`
		Deployment *api.Deployment `json:"deployment"`
	}
	if err := c.send(req, &res); err != nil {
		return err
	}
	if c.json {
		return c.printJSON(res)
	}
	fmt.Fprintf(c.out, "release %s: %s %s (%s, sha256 %s)\n", res.Release.ID, res.Release.Package, res.Release.Version, bytesH(uint64(res.Release.Size)), res.Release.SHA256)
	if res.Deployment != nil {
		fmt.Fprintf(c.out, "auto-deploy started: %s (stages %v)\n", res.Deployment.ID, res.Deployment.Strategy.Stages)
	}
	return nil
}

func (c *cli) deployments(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		fs := flag.NewFlagSet("deployments list", flag.ContinueOnError)
		status := fs.String("status", "", "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		var deps []api.Deployment
		if err := c.do("GET", "/api/v1/deployments?status="+url.QueryEscape(*status), nil, &deps); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(deps)
		}
		var rows [][]string
		for _, d := range deps {
			s := d.Stats
			rows = append(rows, []string{d.ID, d.Name, d.Package + "@" + d.Version, d.Status, fmt.Sprintf("%d%% (%d/%d)", d.CurrentPercent(), d.StageIndex+1, len(d.Strategy.Stages)),
				fmt.Sprintf("%d ok, %d failed, %d in progress", s.Succeeded, s.Failed+s.RolledBack, s.Pending+s.InProgress), ago(d.CreatedAt)})
		}
		c.table("ID\tNAME\tRELEASE\tSTATUS\tSTAGE\tPROGRESS\tCREATED", rows)
		return nil
	case "get", "show":
		if err := need(args, 1, "deployment id"); err != nil {
			return err
		}
		var det struct {
			Deployment api.Deployment      `json:"deployment"`
			Updates    []*api.DeviceUpdate `json:"updates"`
			Targeted   int                 `json:"targeted"`
			Eligible   int                 `json:"eligible"`
		}
		if err := c.do("GET", "/api/v1/deployments/"+url.PathEscape(args[0]), nil, &det); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(det)
		}
		d := det.Deployment
		fmt.Fprintf(c.out, "ID:        %s\nName:      %s\nRelease:   %s %s (%s)\nStatus:    %s %s\n", d.ID, d.Name, d.Package, d.Version, d.ReleaseID, d.Status, d.StatusReason)
		fmt.Fprintf(c.out, "Stage:     %d%% (stage %d of %v, started %s)\n", d.CurrentPercent(), d.StageIndex+1, d.Strategy.Stages, ago(d.StageStartedAt))
		fmt.Fprintf(c.out, "Targets:   %d devices, %d eligible at this stage\n", det.Targeted, det.Eligible)
		s := d.Stats
		fmt.Fprintf(c.out, "Progress:  %d succeeded, %d failed, %d rolled back, %d in progress, %d offered (failure rate %.1f%%)\n",
			s.Succeeded, s.Failed, s.RolledBack, s.InProgress, s.Pending, s.FailureRate*100)
		if len(det.Updates) > 0 {
			fmt.Fprintln(c.out)
			var rows [][]string
			for _, u := range det.Updates {
				rows = append(rows, []string{u.DeviceID, dash(u.FromVersion), u.Status, ago(u.UpdatedAt), u.Message})
			}
			c.table("DEVICE\tFROM\tSTATUS\tUPDATED\tMESSAGE", rows)
		}
		return nil
	case "create", "new":
		fs := flag.NewFlagSet("deployments create", flag.ContinueOnError)
		release := fs.String("release", "", "")
		name := fs.String("name", "", "")
		channel := fs.String("channel", "", "")
		stages := fs.String("stages", "", "")
		stageDur := fs.Duration("stage-duration", -1, "")
		maxFail := fs.Float64("max-failure-rate", -1, "")
		minSamples := fs.Int("min-samples", -1, "")
		manual := fs.Bool("manual", false, "")
		downgrade := fs.Bool("allow-downgrade", false, "")
		startAt := fs.String("start-at", "", "")
		windows := fs.String("windows", "", "")
		days := fs.String("days", "", "")
		tz := fs.String("tz", "", "")
		var labels, devices multiFlag
		fs.Var(&labels, "label", "")
		fs.Var(&devices, "device", "")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if *release == "" && len(pos) > 0 {
			*release = pos[0]
		}
		if *release == "" {
			return errors.New("--release is required")
		}
		req := api.CreateDeploymentRequest{Name: *name, ReleaseID: *release, Selector: api.Selector{Channel: *channel, DeviceIDs: devices}}
		if req.Selector.Labels, err = labelsOf(labels); err != nil {
			return err
		}
		sched := *startAt != "" || *windows != "" || *days != ""
		if *stages != "" || *stageDur >= 0 || *maxFail >= 0 || *minSamples >= 0 || *manual || *downgrade || sched {
			st := api.Strategy{Stages: []int{5, 25, 50, 100}, StageDurationSeconds: 900, AutoAdvance: !*manual, MaxFailureRate: 0.2, MinSamples: 3, AllowDowngrade: *downgrade}
			if *stages != "" {
				st.Stages = nil
				for _, p := range strings.Split(*stages, ",") {
					n, err := strconv.Atoi(strings.TrimSpace(p))
					if err != nil {
						return fmt.Errorf("invalid --stages: %w", err)
					}
					st.Stages = append(st.Stages, n)
				}
			}
			if *stageDur >= 0 {
				st.StageDurationSeconds = int64(stageDur.Seconds())
			}
			if *maxFail >= 0 {
				st.MaxFailureRate = *maxFail
			}
			if *minSamples >= 0 {
				st.MinSamples = *minSamples
			}
			if *startAt != "" {
				t, err := parseStartAt(*startAt, *tz)
				if err != nil {
					return err
				}
				st.StartAt = &t
			}
			st.Windows, st.TimeZone = *windows, *tz
			if *days != "" {
				st.Days = strings.Split(*days, ",")
			}
			req.Strategy = &st
		}
		var d api.Deployment
		if err := c.do("POST", "/api/v1/deployments", req, &d); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(d)
		}
		if d.Status == api.DeploymentScheduled {
			fmt.Fprintf(c.out, "deployment %s scheduled for %s: %s %s, stages %v\n", d.ID, d.Strategy.StartAt.Local().Format(time.DateTime), d.Package, d.Version, d.Strategy.Stages)
		} else {
			fmt.Fprintf(c.out, "deployment %s started: %s %s, stages %v\n", d.ID, d.Package, d.Version, d.Strategy.Stages)
		}
		return nil
	case "pause", "resume", "abort", "advance", "retry", "complete":
		if err := need(args, 1, "deployment id"); err != nil {
			return err
		}
		var d api.Deployment
		if err := c.do("POST", "/api/v1/deployments/"+url.PathEscape(args[0])+"/"+verb, nil, &d); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(d)
		}
		fmt.Fprintf(c.out, "%s: %s at %d%%\n", d.ID, d.Status, d.CurrentPercent())
		return nil
	}
	return fmt.Errorf("unknown deployments command %q", verb)
}

func (c *cli) events(args []string) error {
	if len(args) > 0 && args[0] == "" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	device := fs.String("device", "", "")
	deployment := fs.String("deployment", "", "")
	limit := fs.Int("limit", 50, "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	q := url.Values{"device_id": {*device}, "deployment_id": {*deployment}, "limit": {strconv.Itoa(*limit)}}
	var evs []api.Event
	if err := c.do("GET", "/api/v1/events?"+q.Encode(), nil, &evs); err != nil {
		return err
	}
	if c.json {
		return c.printJSON(evs)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		fmt.Fprintf(c.out, "%s  %-22s %s\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.Type, e.Message)
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

func bytesH(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (c *cli) apps(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var apps []api.App
		if err := c.do("GET", "/api/v1/apps", nil, &apps); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(apps)
		}
		var rows [][]string
		for _, a := range apps {
			lic := "off"
			if a.Licensing {
				lic = (time.Duration(a.LicenseTTLSeconds) * time.Second).String() + " licenses"
			}
			var short []string
			for _, s := range a.Signers {
				short = append(short, s[:16]+"…")
			}
			rows = append(rows, []string{a.Package, dash(a.Name), strings.Join(short, ","), lic})
		}
		c.table("PACKAGE\tNAME\tSIGNERS\tLICENSING", rows)
		return nil
	case "set", "register", "put":
		fs := flag.NewFlagSet("apps set", flag.ContinueOnError)
		name := fs.String("name", "", "")
		lic := fs.Bool("license", false, "")
		ttl := fs.Duration("license-ttl", time.Hour, "")
		var signers multiFlag
		fs.Var(&signers, "signer", "")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if err := need(pos, 1, "package name"); err != nil {
			return err
		}
		if len(signers) == 0 {
			return errors.New("at least one --signer (SHA-256 of the signing certificate) is required")
		}
		var a api.App
		body := api.App{Name: *name, Signers: signers, Licensing: *lic, LicenseTTLSeconds: int64(ttl.Seconds())}
		if err := c.do("PUT", "/api/v1/apps/"+url.PathEscape(pos[0]), body, &a); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(a)
		}
		fmt.Fprintf(c.out, "%s registered (%d signer(s), licensing %v)\n", a.Package, len(a.Signers), a.Licensing)
		return nil
	case "delete", "rm":
		if err := need(args, 1, "package name"); err != nil {
			return err
		}
		if err := c.do("DELETE", "/api/v1/apps/"+url.PathEscape(args[0]), nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "removed", args[0])
		return nil
	}
	return fmt.Errorf("unknown apps command %q", verb)
}

func (c *cli) android(verb string, args []string) error {
	if verb != "qr" {
		return fmt.Errorf("unknown android command %q (try: android qr)", verb)
	}
	fs := flag.NewFlagSet("android qr", flag.ContinueOnError)
	token := fs.String("token", "", "")
	ssid := fs.String("wifi-ssid", "", "")
	pass := fs.String("wifi-password", "", "")
	sec := fs.String("wifi-security", "", "")
	out := fs.String("out", "armada-android-qr.png", "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *token == "" {
		return errors.New("--token is required (create one with: armadactl tokens create)")
	}
	var r struct {
		Payload     string   `json:"payload"`
		QRPNG       string   `json:"qr_png"`
		DownloadURL string   `json:"download_url"`
		Warnings    []string `json:"warnings"`
	}
	body := map[string]string{"enrollment_token": *token, "wifi_ssid": *ssid, "wifi_password": *pass, "wifi_security": *sec}
	if err := c.do("POST", "/api/v1/android/provisioning", body, &r); err != nil {
		return err
	}
	if c.json {
		return c.printJSON(r)
	}
	png, err := base64.StdEncoding.DecodeString(r.QRPNG)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, png, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Wrote %s. On a new or factory-reset Android device, tap the welcome screen six times and scan it.\n", *out)
	for _, w := range r.Warnings {
		fmt.Fprintln(c.out, "warning:", w)
	}
	return nil
}

func (c *cli) simpleDelete(kind, path string, args []string) error {
	if err := need(args, 1, kind+" id"); err != nil {
		return err
	}
	if err := c.do("DELETE", path+url.PathEscape(args[0]), nil, nil); err != nil {
		return err
	}
	fmt.Fprintln(c.out, "deleted", args[0])
	return nil
}

func (c *cli) monitors(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var ms []api.Monitor
		if err := c.do("GET", "/api/v1/monitors", nil, &ms); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(ms)
		}
		var rows [][]string
		for _, m := range ms {
			target := m.Check.Target
			if m.Check.Type == "command" {
				target = strings.Join(m.Check.Command, " ")
			}
			state := "enabled"
			if !m.Enabled {
				state = "disabled"
			}
			rows = append(rows, []string{m.ID, m.Name, m.Check.Type, target, fmtLabels(m.Selector.Labels), state})
		}
		c.table("ID\tNAME\tTYPE\tTARGET\tDEVICES\tSTATE", rows)
		return nil
	case "create", "add":
		fs := flag.NewFlagSet("monitors create", flag.ContinueOnError)
		m := api.Monitor{Enabled: true}
		fs.StringVar(&m.Name, "name", "", "")
		fs.StringVar(&m.Check.Type, "type", "", "")
		fs.StringVar(&m.Check.Target, "target", "", "")
		fs.StringVar(&m.Check.Contains, "contains", "", "")
		fs.IntVar(&m.Check.IntervalSeconds, "interval", 60, "")
		fs.IntVar(&m.Check.TimeoutSeconds, "timeout", 0, "")
		fs.IntVar(&m.Check.WarnMs, "warn-ms", 0, "")
		var labels multiFlag
		fs.Var(&labels, "label", "")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if m.Check.Type == "command" {
			m.Check.Command = pos
		}
		if m.Selector.Labels, err = labelsOf(labels); err != nil {
			return err
		}
		var out api.Monitor
		if err := c.do("POST", "/api/v1/monitors", m, &out); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(out)
		}
		fmt.Fprintf(c.out, "monitor %s created; matching devices start running it on their next check-in\n", out.ID)
		return nil
	case "delete", "rm":
		return c.simpleDelete("monitor", "/api/v1/monitors/", args)
	}
	return fmt.Errorf("unknown monitors command %q", verb)
}

func (c *cli) channels(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var chs []api.NotificationChannel
		if err := c.do("GET", "/api/v1/channels", nil, &chs); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(chs)
		}
		var rows [][]string
		for _, ch := range chs {
			dest := ""
			switch {
			case ch.Webhook != nil:
				dest = ch.Webhook.URL
			case ch.Slack != nil:
				dest = ch.Slack.WebhookURL
			case ch.Email != nil:
				dest = strings.Join(ch.Email.To, ", ")
			}
			rows = append(rows, []string{ch.ID, ch.Name, ch.Type, dest})
		}
		c.table("ID\tNAME\tTYPE\tDESTINATION", rows)
		return nil
	case "create", "add":
		fs := flag.NewFlagSet("channels create", flag.ContinueOnError)
		name := fs.String("name", "", "")
		typ := fs.String("type", "", "")
		u := fs.String("url", "", "")
		secret := fs.String("secret", "", "")
		tpl := fs.String("template", "", "")
		host := fs.String("smtp-host", "", "")
		port := fs.Int("smtp-port", 587, "")
		user := fs.String("smtp-user", "", "")
		pass := fs.String("smtp-password", "", "")
		from := fs.String("from", "", "")
		var headers, to multiFlag
		fs.Var(&headers, "header", "")
		fs.Var(&to, "to", "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		ch := api.NotificationChannel{Name: *name, Type: *typ}
		switch *typ {
		case "webhook":
			hm := map[string]string{}
			for _, hd := range headers {
				k, v, ok := strings.Cut(hd, ":")
				if !ok {
					return fmt.Errorf("header %q must be \"Name: value\"", hd)
				}
				hm[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
			ch.Webhook = &api.WebhookConfig{URL: *u, Secret: *secret, Headers: hm, BodyTemplate: *tpl}
		case "slack":
			ch.Slack = &api.SlackConfig{WebhookURL: *u}
		case "email":
			ch.Email = &api.EmailConfig{Host: *host, Port: *port, Username: *user, Password: *pass, From: *from, To: to}
		}
		var out api.NotificationChannel
		if err := c.do("POST", "/api/v1/channels", ch, &out); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(out)
		}
		fmt.Fprintf(c.out, "channel %s created; try it with: armadactl channels test %s\n", out.ID, out.ID)
		return nil
	case "test":
		if err := need(args, 1, "channel id"); err != nil {
			return err
		}
		if err := c.do("POST", "/api/v1/channels/"+url.PathEscape(args[0])+"/test", nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "test notification delivered")
		return nil
	case "delete", "rm":
		return c.simpleDelete("channel", "/api/v1/channels/", args)
	}
	return fmt.Errorf("unknown channels command %q", verb)
}

func (c *cli) rules(verb string, args []string) error {
	switch verb {
	case "", "list", "ls":
		var rs []api.AlertRule
		if err := c.do("GET", "/api/v1/alert-rules", nil, &rs); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(rs)
		}
		var rows [][]string
		for _, r := range rs {
			state := "enabled"
			if !r.Enabled {
				state = "disabled"
			}
			rows = append(rows, []string{r.ID, r.Name, r.Condition, fmtLabels(r.Selector.Labels), strings.Join(r.Channels, ","), state})
		}
		c.table("ID\tNAME\tCONDITION\tDEVICES\tCHANNELS\tSTATE", rows)
		return nil
	case "create", "add":
		fs := flag.NewFlagSet("rules create", flag.ContinueOnError)
		r := api.AlertRule{Enabled: true}
		fs.StringVar(&r.Name, "name", "", "")
		fs.StringVar(&r.Condition, "condition", "", "")
		fs.StringVar(&r.MinSeverity, "min-severity", "", "")
		fs.IntVar(&r.OfflineMinutes, "offline-minutes", 0, "")
		fs.IntVar(&r.RepeatMinutes, "repeat-minutes", 0, "")
		noResolved := fs.Bool("no-resolved", false, "")
		var labels, chans, kinds multiFlag
		fs.Var(&labels, "label", "")
		fs.Var(&chans, "channel", "")
		fs.Var(&kinds, "kind", "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		var err error
		if r.Selector.Labels, err = labelsOf(labels); err != nil {
			return err
		}
		r.Channels, r.CheckKinds, r.NotifyResolved = chans, kinds, !*noResolved
		var out api.AlertRule
		if err := c.do("POST", "/api/v1/alert-rules", r, &out); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(out)
		}
		fmt.Fprintf(c.out, "rule %s created\n", out.ID)
		return nil
	case "delete", "rm":
		return c.simpleDelete("rule", "/api/v1/alert-rules/", args)
	}
	return fmt.Errorf("unknown rules command %q", verb)
}

func (c *cli) alerts(args []string) error {
	if len(args) > 0 && args[0] == "" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("alerts", flag.ContinueOnError)
	firing := fs.Bool("firing", false, "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	q := ""
	if *firing {
		q = "?status=firing"
	}
	var as []api.Alert
	if err := c.do("GET", "/api/v1/alerts"+q, nil, &as); err != nil {
		return err
	}
	if c.json {
		return c.printJSON(as)
	}
	var rows [][]string
	for _, a := range as {
		rows = append(rows, []string{a.Status, a.Severity, a.RuleName, firstLine(a.Summary), ago(a.StartedAt)})
	}
	c.table("STATUS\tSEVERITY\tRULE\tSUMMARY\tSINCE", rows)
	return nil
}

func (c *cli) history(verb string, args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	rng := fs.String("range", "24h", "")
	check := fs.String("check", "", "")
	channel := fs.String("channel", "", "")
	asCSV := fs.Bool("csv", false, "")
	var labels multiFlag
	fs.Var(&labels, "label", "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	var series []api.HistorySeries
	if verb == "checks" {
		var names []struct {
			Name    string `json:"name"`
			Devices int    `json:"devices"`
		}
		if err := c.do("GET", "/api/v1/history/checks", nil, &names); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(names)
		}
		var rows [][]string
		for _, n := range names {
			rows = append(rows, []string{n.Name, strconv.Itoa(n.Devices)})
		}
		c.table("CHECK\tDEVICES", rows)
		return nil
	}
	if verb == "compare" {
		if *check == "" {
			return errors.New("--check is required; list the names with: armadactl history checks")
		}
		q := url.Values{"check": {*check}, "range": {*rng}, "channel": {*channel}}
		for _, l := range labels {
			q.Add("label", l)
		}
		var resp struct {
			Matching int                 `json:"matching"`
			Series   []api.HistorySeries `json:"series"`
		}
		if err := c.do("GET", "/api/v1/history/compare?"+q.Encode(), nil, &resp); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(resp)
		}
		series = resp.Series
		if !*asCSV && resp.Matching > len(series) {
			defer fmt.Fprintf(c.out, "\n%d devices have this check; showing the %d that need attention most.\n", resp.Matching, len(series))
		}
	} else {
		if verb == "" {
			return errors.New("usage: armadactl history <device-id> [--range 7d] [--csv]")
		}
		var h api.DeviceHistory
		if err := c.do("GET", "/api/v1/devices/"+url.PathEscape(verb)+"/history?range="+url.QueryEscape(*rng), nil, &h); err != nil {
			return err
		}
		if c.json {
			return c.printJSON(h)
		}
		for _, sr := range h.Series {
			if *check == "" || sr.Name == *check {
				series = append(series, sr)
			}
		}
	}
	_ = pos
	if *asCSV {
		w := csv.NewWriter(c.out)
		w.Write([]string{"device", "series", "unit", "time", "value", "min", "max", "status"})
		for _, sr := range series {
			dev := sr.DeviceName
			if dev == "" {
				dev = verb
			}
			for _, p := range sr.Points {
				w.Write([]string{dev, sr.Name, sr.Unit, time.Unix(p.T, 0).UTC().Format(time.RFC3339), num(p.V), num(p.Min), num(p.Max), p.S})
			}
		}
		w.Flush()
		return w.Error()
	}
	var rows [][]string
	for _, sr := range series {
		name := sr.Name
		if sr.DeviceName != "" {
			name = sr.DeviceName
		}
		now, lo, hi := "-", "-", "-"
		var mn, mx *float64
		for _, p := range sr.Points {
			for _, v := range []*float64{p.V, p.Min, p.Max} {
				if v == nil {
					continue
				}
				if mn == nil || *v < *mn {
					mn = v
				}
				if mx == nil || *v > *mx {
					mx = v
				}
			}
			if p.V != nil {
				now = num(p.V)
			}
		}
		if mn != nil {
			lo, hi = num(mn), num(mx)
		}
		avail := "-"
		if sr.Availability != nil {
			avail = fmt.Sprintf("%.1f%%", *sr.Availability*100)
			if sr.Name == "online" {
				avail += " up"
			} else {
				avail += " ok"
			}
		}
		rows = append(rows, []string{name, now, lo, hi, sr.Unit, avail, strconv.Itoa(len(sr.Points))})
	}
	c.table("SERIES\tLATEST\tMIN\tMAX\tUNIT\tAVAILABILITY\tPOINTS", rows)
	return nil
}

func num(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func (c *cli) backup(args []string) error {
	if len(args) > 0 && args[0] == "" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	out := fs.String("o", "armada-backup-"+time.Now().Format("20060102-150405")+".tar.gz", "")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	req, err := http.NewRequest("GET", c.server+"/api/v1/backup", nil)
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
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("backup failed: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(*out)
		return fmt.Errorf("backup interrupted: %w", err)
	}
	fmt.Fprintf(c.out, "wrote %s (%s). It contains the signing keys: store it securely.\n", *out, bytesH(uint64(n)))
	return nil
}
