// Package debpkg writes Debian packages (.deb) without external tools.
//
// A .deb is an ar archive of three members: "debian-binary" (the format
// version), control.tar.gz (metadata and maintainer scripts) and
// data.tar.gz (the files to install). See deb(5).
package debpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// File is one file the package installs.
type File struct {
	Path string // absolute, e.g. /usr/bin/armada-agent
	Mode int64  // e.g. 0o755
	Data []byte
	// Conffile marks configuration files that upgrades must not overwrite
	// once an administrator has changed them.
	Conffile bool
}

// Package describes a package.
type Package struct {
	Name         string
	Version      string // Debian version, see Version
	Architecture string // Debian name: amd64, arm64, armhf, i386, riscv64
	Maintainer   string
	Section      string
	Homepage     string
	Summary      string
	Description  string // extended description; paragraphs separated by blank lines
	Depends      string
	Files        []File
	Dirs         []Dir // directories to create (beyond the files' parents)
	// Maintainer scripts (shell), run by dpkg: postinst, prerm, postrm.
	Scripts map[string]string
	// ModTime stamps every entry, so the same inputs make the same bytes.
	ModTime time.Time
}

// Dir is an empty directory the package creates.
type Dir struct {
	Path string
	Mode int64
}

// Version turns a release version (v1.2.0, 1.2.0-rc1) into a Debian one:
// no leading v, and pre-releases with "~" so 1.2.0~rc1 sorts before 1.2.0.
func Version(v string) string {
	v = strings.TrimPrefix(v, "v")
	if base, pre, ok := strings.Cut(v, "-"); ok {
		v = base + "~" + strings.ReplaceAll(pre, "-", ".")
	}
	return v
}

// Arch maps a Go GOARCH (and GOARM) to the Debian architecture name.
func Arch(goarch string) (string, error) {
	switch goarch {
	case "amd64", "arm64", "riscv64":
		return goarch, nil
	case "arm":
		return "armhf", nil // built with GOARM=7
	case "386":
		return "i386", nil
	}
	return "", fmt.Errorf("no Debian architecture for GOARCH %s", goarch)
}

// Write writes the package.
func (p *Package) Write(w io.Writer) error {
	if p.Name == "" || p.Version == "" || p.Architecture == "" || p.Maintainer == "" || p.Summary == "" {
		return errors.New("debpkg: name, version, architecture, maintainer and summary are required")
	}
	if v := p.Version; v[0] < '0' || v[0] > '9' {
		return fmt.Errorf("debpkg: version %q must start with a digit", v)
	}
	data, size, md5s, err := p.dataTar()
	if err != nil {
		return err
	}
	control, err := p.controlTar(size, md5s)
	if err != nil {
		return err
	}
	ar := &arWriter{w: w, mtime: p.ModTime}
	if err := ar.header(); err != nil {
		return err
	}
	for _, m := range []struct {
		name string
		data []byte
	}{{"debian-binary", []byte("2.0\n")}, {"control.tar.gz", control}, {"data.tar.gz", data}} {
		if err := ar.add(m.name, m.data); err != nil {
			return err
		}
	}
	return nil
}

func (p *Package) controlTar(installedSize int64, md5s string) ([]byte, error) {
	var c strings.Builder
	field := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&c, "%s: %s\n", k, v)
		}
	}
	field("Package", p.Name)
	field("Version", p.Version)
	field("Architecture", p.Architecture)
	field("Maintainer", p.Maintainer)
	field("Installed-Size", fmt.Sprint((installedSize+1023)/1024))
	field("Depends", p.Depends)
	field("Section", p.Section)
	field("Priority", "optional")
	field("Homepage", p.Homepage)
	c.WriteString("Description: " + p.Summary + "\n")
	for _, line := range strings.Split(strings.TrimSpace(p.Description), "\n") {
		if strings.TrimSpace(line) == "" {
			c.WriteString(" .\n")
		} else {
			c.WriteString(" " + line + "\n")
		}
	}
	tw := newTarGz(p.ModTime)
	tw.dir("./", 0o755)
	tw.file("./control", 0o644, []byte(c.String()))
	tw.file("./md5sums", 0o644, []byte(md5s))
	var conf []string
	for _, f := range p.Files {
		if f.Conffile {
			conf = append(conf, f.Path)
		}
	}
	if len(conf) > 0 {
		tw.file("./conffiles", 0o644, []byte(strings.Join(conf, "\n")+"\n"))
	}
	names := make([]string, 0, len(p.Scripts))
	for n := range p.Scripts {
		switch n {
		case "preinst", "postinst", "prerm", "postrm":
		default:
			return nil, fmt.Errorf("debpkg: unknown maintainer script %q", n)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		tw.file("./"+n, 0o755, []byte(p.Scripts[n]))
	}
	return tw.close()
}

func (p *Package) dataTar() ([]byte, int64, string, error) {
	dirs := map[string]int64{}
	for _, d := range p.Dirs {
		dirs[path.Clean(d.Path)] = d.Mode
	}
	for _, f := range p.Files {
		if !path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path {
			return nil, 0, "", fmt.Errorf("debpkg: file path %q must be absolute and clean", f.Path)
		}
		for d := path.Dir(f.Path); d != "/"; d = path.Dir(d) {
			if _, ok := dirs[d]; !ok {
				dirs[d] = 0o755
			}
		}
	}
	// Parents of the listed directories too.
	for d := range dirs {
		for q := path.Dir(d); q != "/"; q = path.Dir(q) {
			if _, ok := dirs[q]; !ok {
				dirs[q] = 0o755
			}
		}
	}
	dirList := make([]string, 0, len(dirs))
	for d := range dirs {
		dirList = append(dirList, d)
	}
	sort.Strings(dirList)
	files := append([]File(nil), p.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	tw := newTarGz(p.ModTime)
	tw.dir("./", 0o755)
	for _, d := range dirList {
		tw.dir("."+d+"/", dirs[d])
	}
	var size int64
	var md5s strings.Builder
	for _, f := range files {
		tw.file("."+f.Path, f.Mode, f.Data)
		size += int64(len(f.Data))
		fmt.Fprintf(&md5s, "%x  %s\n", md5.Sum(f.Data), strings.TrimPrefix(f.Path, "/"))
	}
	b, err := tw.close()
	return b, size, md5s.String(), err
}

// ---- tar.gz ----

type tarGz struct {
	buf   bytes.Buffer
	gz    *gzip.Writer
	tw    *tar.Writer
	mtime time.Time
	err   error
}

func newTarGz(mtime time.Time) *tarGz {
	t := &tarGz{mtime: mtime}
	t.gz, _ = gzip.NewWriterLevel(&t.buf, gzip.BestCompression)
	t.gz.ModTime = mtime
	t.tw = tar.NewWriter(t.gz)
	return t
}

func (t *tarGz) hdr(h *tar.Header) {
	h.Uid, h.Gid, h.Uname, h.Gname, h.ModTime, h.Format = 0, 0, "root", "root", t.mtime, tar.FormatGNU
	if t.err == nil {
		t.err = t.tw.WriteHeader(h)
	}
}

func (t *tarGz) dir(name string, mode int64) {
	t.hdr(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: mode})
}

func (t *tarGz) file(name string, mode int64, data []byte) {
	t.hdr(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data))})
	if t.err == nil {
		_, t.err = t.tw.Write(data)
	}
}

func (t *tarGz) close() ([]byte, error) {
	if t.err != nil {
		return nil, t.err
	}
	if err := t.tw.Close(); err != nil {
		return nil, err
	}
	if err := t.gz.Close(); err != nil {
		return nil, err
	}
	return t.buf.Bytes(), nil
}

// ---- ar ----

type arWriter struct {
	w     io.Writer
	mtime time.Time
}

func (a *arWriter) header() error {
	_, err := io.WriteString(a.w, "!<arch>\n")
	return err
}

func (a *arWriter) add(name string, data []byte) error {
	if len(name) > 16 {
		return fmt.Errorf("ar member name %q too long", name)
	}
	h := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d`\n", name, a.mtime.Unix(), 0, 0, 0o100644, len(data))
	if _, err := io.WriteString(a.w, h); err != nil {
		return err
	}
	if _, err := a.w.Write(data); err != nil {
		return err
	}
	if len(data)%2 == 1 {
		_, err := a.w.Write([]byte{'\n'})
		return err
	}
	return nil
}
