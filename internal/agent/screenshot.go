package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// captureScreen returns a PNG (or JPEG) of the screen, and how it was taken.
func captureScreen(ctx context.Context, custom []string) ([]byte, string, error) {
	if len(custom) > 0 {
		img, err := runCapture(ctx, custom, "")
		return img, custom[0], err
	}
	dir, err := os.MkdirTemp("", "armada-shot-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "screen.png")
	var tries [][]string
	switch runtime.GOOS {
	case "linux":
		tries = [][]string{
			{"grim", "-"},                          // Wayland
			{"import", "-window", "root", "png:-"}, // X11, ImageMagick
			{"scrot", "-o", file},                  // X11
			{"gnome-screenshot", "-f", file},
		}
	case "darwin":
		tries = [][]string{{"screencapture", "-x", "-t", "png", file}}
	case "windows":
		ps := `Add-Type -AssemblyName System.Windows.Forms,System.Drawing;` +
			`$b=[System.Windows.Forms.SystemInformation]::VirtualScreen;` +
			`$i=New-Object System.Drawing.Bitmap $b.Width,$b.Height;` +
			`$g=[System.Drawing.Graphics]::FromImage($i);$g.CopyFromScreen($b.Left,$b.Top,0,0,$i.Size);` +
			`$i.Save('` + file + `',[System.Drawing.Imaging.ImageFormat]::Png)`
		tries = [][]string{{"powershell", "-NoProfile", "-NonInteractive", "-Command", ps}}
	default:
		return nil, "", fmt.Errorf("screenshots aren't supported on %s", runtime.GOOS)
	}
	var errs []string
	for _, t := range tries {
		if _, err := exec.LookPath(t[0]); err != nil {
			continue
		}
		out := ""
		for _, a := range t {
			if a == file {
				out = file
			}
		}
		img, err := runCapture(ctx, t, out)
		if err == nil {
			return img, t[0], nil
		}
		errs = append(errs, t[0]+": "+err.Error())
	}
	if len(errs) == 0 {
		return nil, "", errors.New("no screenshot tool found (install grim, ImageMagick or scrot, or set screenshot_command)")
	}
	return nil, "", errors.New(strings.Join(errs, "; "))
}

// runCapture runs a capture command; the image is its stdout, or file.
func runCapture(ctx context.Context, argv []string, file string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" {
		cmd.Env = append(cmd.Env, "DISPLAY=:0") // the usual local display, for X11 tools
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg != "" {
			return nil, fmt.Errorf("%v: %s", err, msg)
		}
		return nil, err
	}
	img := stdout.Bytes()
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		img = b
	}
	if ct := http.DetectContentType(img); ct != "image/png" && ct != "image/jpeg" {
		return nil, errors.New("the capture didn't produce a PNG or JPEG")
	}
	return img, nil
}
