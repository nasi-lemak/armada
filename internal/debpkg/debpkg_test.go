package debpkg

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersion(t *testing.T) {
	for in, want := range map[string]string{"v1.2.0": "1.2.0", "1.2.0-rc1": "1.2.0~rc1", "1.2.0-rc.1-x": "1.2.0~rc.1.x", "1.2.0+git5": "1.2.0+git5"} {
		if got := Version(in); got != want {
			t.Errorf("Version(%q) = %q, want %q", in, got, want)
		}
	}
	// dpkg agrees a pre-release sorts first.
	if _, err := exec.LookPath("dpkg"); err == nil {
		if err := exec.Command("dpkg", "--compare-versions", Version("1.2.0-rc1"), "lt", Version("1.2.0")).Run(); err != nil {
			t.Error("dpkg does not sort 1.2.0~rc1 before 1.2.0")
		}
	}
}

func testPackage() *Package {
	return &Package{
		Name: "armada-test", Version: "1.2.0~rc1", Architecture: "amd64", Maintainer: "Armada <noreply@example.invalid>",
		Section: "admin", Summary: "Test package", Description: "First paragraph.\n\nSecond paragraph.",
		Files: []File{
			{Path: "/usr/bin/armada-test", Mode: 0o755, Data: []byte("#!/bin/sh\necho hi\n")},
			{Path: "/etc/default/armada-test", Mode: 0o644, Data: []byte("# settings\n"), Conffile: true},
		},
		Dirs:    []Dir{{Path: "/var/lib/armada-test", Mode: 0o700}},
		Scripts: map[string]string{"postinst": "#!/bin/sh\nset -e\nexit 0\n"},
		ModTime: time.Unix(1700000000, 0),
	}
}

func TestWriteIsReproducible(t *testing.T) {
	var a, b bytes.Buffer
	if err := testPackage().Write(&a); err != nil {
		t.Fatal(err)
	}
	testPackage().Write(&b)
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("the same package came out different")
	}
	if !strings.HasPrefix(a.String(), "!<arch>\ndebian-binary") {
		t.Errorf("not an ar archive: %q", a.String()[:30])
	}
}

// TestDpkgReadsIt checks the package with dpkg-deb, where it's installed.
func TestDpkgReadsIt(t *testing.T) {
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb not installed")
	}
	dir := t.TempDir()
	deb := filepath.Join(dir, "test.deb")
	f, _ := os.Create(deb)
	if err := testPackage().Write(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	info, err := exec.Command("dpkg-deb", "--info", deb).CombinedOutput()
	if err != nil {
		t.Fatalf("dpkg-deb --info: %v\n%s", err, info)
	}
	for _, want := range []string{"Package: armada-test", "Version: 1.2.0~rc1", "Architecture: amd64", "Second paragraph.", "postinst", "conffiles"} {
		if !strings.Contains(string(info), want) {
			t.Errorf("--info lacks %q:\n%s", want, info)
		}
	}
	list, err := exec.Command("dpkg-deb", "--contents", deb).CombinedOutput()
	if err != nil {
		t.Fatalf("dpkg-deb --contents: %v\n%s", err, list)
	}
	for _, want := range []string{"-rwxr-xr-x root/root", "./usr/bin/armada-test", "drwx------ root/root", "./var/lib/armada-test/"} {
		if !strings.Contains(string(list), want) {
			t.Errorf("--contents lacks %q:\n%s", want, list)
		}
	}
	out := filepath.Join(dir, "x")
	if b, err := exec.Command("dpkg-deb", "-x", deb, out).CombinedOutput(); err != nil {
		t.Fatalf("dpkg-deb -x: %v\n%s", err, b)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "usr/bin/armada-test")); string(b) != "#!/bin/sh\necho hi\n" {
		t.Errorf("extracted %q", b)
	}
	if b, err := exec.Command("dpkg-deb", "--field", deb, "Installed-Size").Output(); err != nil || strings.TrimSpace(string(b)) != "1" {
		t.Errorf("Installed-Size %q %v", b, err)
	}
}
