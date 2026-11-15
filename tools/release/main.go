// Command release builds an Armada release into dist/<version>/: the server,
// agent and CLI for every supported platform, .deb packages of the agent
// and server, a list of the Go modules in each binary, and SHA256SUMS.
//
//	go run ./tools/release -version 1.4.0
//
// Run it from the repository root. Everything is built from the source in
// the working tree with the Go toolchain alone.
package main

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/debpkg"
)

const module = "github.com/nasi-lemak/armada"

var (
	agentPlatforms  = []string{"linux/amd64", "linux/arm64", "linux/arm", "linux/386", "linux/riscv64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"}
	serverPlatforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"}
	debAgent        = []string{"amd64", "arm64", "arm", "386", "riscv64"}
	debServer       = []string{"amd64", "arm64"}
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func run() error {
	version := flag.String("version", "", "release version, e.g. 1.4.0 or v1.4.0-rc1 (required)")
	out := flag.String("out", "", "output directory (default dist/<version>)")
	maintainer := flag.String("maintainer", "Armada <noreply@example.invalid>", "Maintainer field of the packages")
	flag.Parse()
	v := strings.TrimPrefix(*version, "v")
	if v == "" || strings.ContainsAny(v, "/ _") {
		return errors.New("-version is required, e.g. -version 1.4.0")
	}
	if *out == "" {
		*out = filepath.Join("dist", v)
	}
	if _, err := os.Stat("go.mod"); err != nil {
		return errors.New("run this from the repository root")
	}
	if dirty, _ := exec.Command("git", "status", "--porcelain").Output(); len(dirty) > 0 {
		fmt.Fprintln(os.Stderr, "warning: the working tree has uncommitted changes; they are in this build")
	}
	mtime := time.Now()
	if ct, err := exec.Command("git", "log", "-1", "--format=%ct").Output(); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(ct)), 10, 64); err == nil {
			mtime = time.Unix(n, 0) // reproducible package timestamps
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}

	// Binaries.
	type job struct{ cmd, plat string }
	var jobs []job
	for _, p := range agentPlatforms {
		jobs = append(jobs, job{"armada-agent", p})
	}
	for _, p := range serverPlatforms {
		jobs = append(jobs, job{"armada-server", p}, job{"armadactl", p})
	}
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[i] = build(j.cmd, j.plat, v, *out)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	fmt.Printf("built %d binaries\n", len(jobs))

	// Packages.
	for _, arch := range debAgent {
		if err := agentDeb(v, arch, *out, *maintainer, mtime); err != nil {
			return err
		}
	}
	for _, arch := range debServer {
		if err := serverDeb(v, arch, *out, *maintainer, mtime); err != nil {
			return err
		}
	}
	fmt.Printf("built %d .deb packages\n", len(debAgent)+len(debServer))

	if err := dependencies(v, *out); err != nil {
		return err
	}
	if err := checksums(*out); err != nil {
		return err
	}
	fmt.Printf(`
Release %s is in %s.

Next:
  - upload the agents so devices can install and update to them:
      armadactl releases upload-agents %s
  - build the server image: docker build --build-arg VERSION=%s -t armada-server:%s .
`, v, *out, *out, v, v)
	return nil
}

func binName(cmd, v, plat string) string {
	goos, goarch, _ := strings.Cut(plat, "/")
	name := fmt.Sprintf("%s_%s_%s_%s", cmd, v, goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func build(cmd, plat, v, out string) error {
	goos, goarch, _ := strings.Cut(plat, "/")
	ld := fmt.Sprintf("-s -w -X main.version=%s -X %s/internal/agent.Version=%s", v, module, v)
	c := exec.Command("go", "build", "-trimpath", "-ldflags", ld, "-o", filepath.Join(out, binName(cmd, v, plat)), "./cmd/"+cmd)
	c.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch, "GOARM=7")
	if b, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("build %s for %s: %v\n%s", cmd, plat, err, b)
	}
	return nil
}

func read(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return b
}

// unit adapts a unit file in deploy/ to the packaged paths.
func unit(path string) []byte {
	return bytes.ReplaceAll(read(path), []byte("/usr/local/bin/"), []byte("/usr/bin/"))
}

func agentDeb(v, goarch, out, maintainer string, mtime time.Time) error {
	arch, err := debpkg.Arch(goarch)
	if err != nil {
		return err
	}
	p := &debpkg.Package{
		Name: "armada-agent", Version: debpkg.Version(v), Architecture: arch, Maintainer: maintainer,
		Section: "admin", Homepage: "https://" + module,
		Summary: "Armada fleet agent",
		Description: `Runs on each managed device: checks in with the Armada server, reports
health, installs software updates (including updates to itself) and runs
remote actions.

Write /etc/armada/agent.json (see /usr/share/doc/armada-agent/), or install
with ARMADA_SERVER_URL and ARMADA_ENROLLMENT_TOKEN set to have it written.`,
		Files: []debpkg.File{
			{Path: "/usr/bin/armada-agent", Mode: 0o755, Data: read(filepath.Join(out, binName("armada-agent", v, "linux/"+goarch)))},
			{Path: "/lib/systemd/system/armada-agent.service", Mode: 0o644, Data: unit("deploy/armada-agent.service")},
			{Path: "/usr/share/doc/armada-agent/agent.example.json", Mode: 0o644, Data: read("deploy/agent.example.json")},
		},
		Dirs:    []debpkg.Dir{{Path: "/etc/armada", Mode: 0o700}, {Path: "/var/lib/armada", Mode: 0o700}},
		Scripts: map[string]string{"postinst": agentPostinst, "prerm": stopOnRemove("armada-agent"), "postrm": agentPostrm},
		ModTime: mtime,
	}
	return writeDeb(p, out)
}

func serverDeb(v, goarch, out, maintainer string, mtime time.Time) error {
	arch, err := debpkg.Arch(goarch)
	if err != nil {
		return err
	}
	svc := unit("deploy/armada-server.service")
	// Settings come from /etc/default/armada-server.
	svc = bytes.Replace(svc, []byte("[Service]\n"), []byte("[Service]\nEnvironmentFile=-/etc/default/armada-server\n"), 1)
	p := &debpkg.Package{
		Name: "armada-server", Version: debpkg.Version(v), Architecture: arch, Maintainer: maintainer,
		Section: "admin", Homepage: "https://" + module,
		Summary: "Armada fleet management server",
		Description: `The Armada control plane: dashboard, REST API, rollout engine, alerts and
artifact store, with its database in /var/lib/armada-server. Includes
armadactl, the command-line client.

Set ARMADA_PUBLIC_URL and other settings in /etc/default/armada-server, then
start it: systemctl start armada-server.`,
		Files: []debpkg.File{
			{Path: "/usr/bin/armada-server", Mode: 0o755, Data: read(filepath.Join(out, binName("armada-server", v, "linux/"+goarch)))},
			{Path: "/usr/bin/armadactl", Mode: 0o755, Data: read(filepath.Join(out, binName("armadactl", v, "linux/"+goarch)))},
			{Path: "/lib/systemd/system/armada-server.service", Mode: 0o644, Data: svc},
			{Path: "/etc/default/armada-server", Mode: 0o644, Data: []byte(serverDefaults), Conffile: true},
		},
		Scripts: map[string]string{"postinst": serverPostinst, "prerm": stopOnRemove("armada-server"), "postrm": serverPostrm},
		ModTime: mtime,
	}
	return writeDeb(p, out)
}

func writeDeb(p *debpkg.Package, out string) error {
	name := fmt.Sprintf("%s_%s_%s.deb", p.Name, p.Version, p.Architecture)
	f, err := os.Create(filepath.Join(out, name))
	if err != nil {
		return err
	}
	if err := p.Write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// dependencies lists the Go modules compiled into each program.
func dependencies(v, out string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Go modules in Armada %s, from each binary's build information.\n", v)
	for _, cmd := range []string{"armada-server", "armada-agent", "armadactl"} {
		bi, err := buildinfo.ReadFile(filepath.Join(out, binName(cmd, v, "linux/amd64")))
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "\n%s (built with %s)\n", cmd, bi.GoVersion)
		for _, d := range bi.Deps {
			if d.Replace != nil {
				d = d.Replace
			}
			fmt.Fprintf(&b, "  %s %s %s\n", d.Path, d.Version, d.Sum)
		}
	}
	return os.WriteFile(filepath.Join(out, "dependencies.txt"), []byte(b.String()), 0o644)
}

func checksums(out string) error {
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && e.Name() != "SHA256SUMS" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		f, err := os.Open(filepath.Join(out, n))
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%x  %s\n", h.Sum(nil), n)
	}
	return os.WriteFile(filepath.Join(out, "SHA256SUMS"), []byte(b.String()), 0o644)
}

// ---- maintainer scripts ----

// dpkg calls postinst with "configure" on install and upgrade, prerm with
// "remove" or "upgrade", postrm with "remove", "purge" or "upgrade".

const agentPostinst = `#!/bin/sh
set -e
if [ "$1" = configure ]; then
	mkdir -p /etc/armada /var/lib/armada
	chmod 0700 /etc/armada /var/lib/armada
	conf=/etc/armada/agent.json
	if [ ! -f "$conf" ] && [ -n "${ARMADA_SERVER_URL:-}" ] && [ -n "${ARMADA_ENROLLMENT_TOKEN:-}" ]; then
		case "$ARMADA_SERVER_URL$ARMADA_ENROLLMENT_TOKEN" in
		*'"'* | *'\'*) echo "armada-agent: ARMADA_SERVER_URL or ARMADA_ENROLLMENT_TOKEN has quotes or backslashes; not writing $conf" >&2 ;;
		*)
			(umask 077 && cat >"$conf" <<JSON
{
  "server_url": "${ARMADA_SERVER_URL%/}",
  "enrollment_token": "$ARMADA_ENROLLMENT_TOKEN",
  "state_dir": "/var/lib/armada",
  "auto_update": true,
  "allow_reboot": true,
  "allow_exec": false
}
JSON
			)
			echo "armada-agent: wrote $conf"
			;;
		esac
	fi
	if [ -d /run/systemd/system ]; then
		systemctl daemon-reload >/dev/null || true
		systemctl enable armada-agent.service >/dev/null 2>&1 || true
		if [ -f "$conf" ]; then
			systemctl restart armada-agent.service || true
		else
			echo "armada-agent: write $conf (see /usr/share/doc/armada-agent/agent.example.json), then: systemctl start armada-agent"
		fi
	fi
fi
exit 0
`

const agentPostrm = `#!/bin/sh
set -e
if [ "$1" = purge ]; then
	rm -rf /etc/armada /var/lib/armada
fi
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null || true
fi
exit 0
`

const serverPostinst = `#!/bin/sh
set -e
if [ "$1" = configure ] && [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null || true
	systemctl enable armada-server.service >/dev/null 2>&1 || true
	if [ -z "$2" ]; then
		echo "armada-server: set ARMADA_PUBLIC_URL and other settings in /etc/default/armada-server, then: systemctl start armada-server"
	else
		systemctl try-restart armada-server.service || true # an upgrade: restart if running
	fi
fi
exit 0
`

const serverPostrm = `#!/bin/sh
set -e
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null || true
fi
# The database in /var/lib/armada-server is kept, even on purge: delete it
# yourself once you're sure (take a backup first).
exit 0
`

func stopOnRemove(svc string) string {
	return `#!/bin/sh
set -e
if [ "$1" = remove ] && [ -d /run/systemd/system ]; then
	systemctl stop ` + svc + `.service >/dev/null 2>&1 || true
	systemctl disable ` + svc + `.service >/dev/null 2>&1 || true
fi
exit 0
`
}

const serverDefaults = `# Settings for armada-server, read by its systemd service. Every flag has an
# environment variable; see armada-server -help and docs/server.md.

# The URL devices and browsers use to reach this server (required for
# single sign-on, passkeys and Android provisioning).
#ARMADA_PUBLIC_URL=https://fleet.example.com

# TLS, if the server terminates it itself (otherwise put it behind a proxy).
#ARMADA_TLS_CERT=/etc/armada-server/tls.crt
#ARMADA_TLS_KEY=/etc/armada-server/tls.key

# Automatic backups.
#ARMADA_BACKUP_DIR=/var/backups/armada

# The admin token. If unset, one is generated into
# /var/lib/armada-server/admin-token on first start.
#ARMADA_ADMIN_TOKEN=
`
