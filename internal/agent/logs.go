package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultLogLines = 200
	maxLogScan      = 8 << 20   // bytes read from a file or journalctl before filtering
	maxLogOutput    = 256 << 10 // bytes sent to the server (the newest are kept)
)

type logRequest struct {
	source, unit, name, grep string
	lines                    int
	since                    time.Duration
}

func parseLogArgs(args []string) (logRequest, error) {
	req := logRequest{lines: defaultLogLines}
	if len(args) == 0 {
		return req, errors.New("no log source given")
	}
	req.source = args[0]
	for _, a := range args[1:] {
		k, v, _ := strings.Cut(a, "=")
		switch k {
		case "unit":
			req.unit = v
		case "name":
			req.name = v
		case "grep":
			req.grep = strings.ToLower(v)
		case "lines":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return req, fmt.Errorf("invalid lines %q", v)
			}
			req.lines = min(n, 5000)
		case "since":
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return req, fmt.Errorf("invalid since %q", v)
			}
			req.since = d
		default:
			return req, fmt.Errorf("unknown option %q", k)
		}
	}
	return req, nil
}

// collectLogs answers a "logs" action. Only the systemd journal and files
// the device's config names in log_files can be read.
func (a *Agent) collectLogs(ctx context.Context, args []string) (string, error) {
	req, err := parseLogArgs(args)
	if err != nil {
		return "", err
	}
	var raw []byte
	var header string
	switch req.source {
	case "journal":
		raw, header, err = readJournal(ctx, req, a.now())
	case "file":
		path, ok := a.cfg.LogFiles[req.name]
		if !ok {
			return "", fmt.Errorf("no log file named %q on this device; configured: %s", req.name, logFileNames(a.cfg.LogFiles))
		}
		header = "# " + path
		if req.since > 0 {
			header += " (since is ignored for files)"
		}
		raw, err = tailFile(path, maxLogScan)
	default:
		return "", fmt.Errorf("unknown log source %q", req.source)
	}
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if req.grep != "" {
		kept := lines[:0]
		for _, l := range lines {
			if strings.Contains(strings.ToLower(l), req.grep) {
				kept = append(kept, l)
			}
		}
		lines = kept
		header += fmt.Sprintf(" | lines containing %q", req.grep)
	}
	if len(lines) > req.lines {
		lines = lines[len(lines)-req.lines:]
	}
	out := header + "\n" + strings.Join(lines, "\n")
	if len(lines) == 0 {
		out += "(no matching lines)"
	}
	if len(out) > maxLogOutput {
		out = out[len(out)-maxLogOutput:]
	}
	return out, nil
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func logFileNames(m map[string]string) string {
	if len(m) == 0 {
		return "none (add log_files to the agent config)"
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func readJournal(ctx context.Context, req logRequest, now time.Time) ([]byte, string, error) {
	bin, err := exec.LookPath("journalctl")
	if err != nil || runtime.GOOS != "linux" {
		return nil, "", errors.New("this device has no systemd journal; list log files in the agent's log_files and use the file source")
	}
	n := req.lines
	if req.grep != "" {
		n = 50000 // filter a wider window, then keep the last lines that match
	}
	argv := []string{"--no-pager", "--output=short-iso", "--lines=" + strconv.Itoa(n)}
	if req.unit != "" {
		argv = append(argv, "--unit="+req.unit)
	}
	if req.since > 0 {
		argv = append(argv, "--since="+now.Add(-req.since).Local().Format("2006-01-02 15:04:05"))
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	var buf, stderr capBuffer
	buf.max, stderr.max = maxLogScan, 4096
	cmd.Stdout, cmd.Stderr = &buf, &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("journalctl: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return buf.Bytes(), "# journalctl " + strings.Join(argv[2:], " "), nil
}

// capBuffer keeps the last max bytes written.
type capBuffer struct {
	bytes.Buffer
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	n, _ := c.Buffer.Write(p)
	if over := c.Len() - c.max; over > 0 {
		c.Next(over)
	}
	return n, nil
}

// tailFile returns up to limit bytes from the end of a file, starting at a
// line boundary.
func tailFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	off := max(st.Size()-limit, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, err
	}
	if off > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b, nil
}
