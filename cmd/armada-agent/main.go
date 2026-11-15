// Command armada-agent runs on each managed device.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nasi-lemak/armada/internal/agent"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "armada-agent:", err)
		os.Exit(1)
	}
}

func run() (err error) {
	var (
		configPath  = flag.String("config", envOr("ARMADA_CONFIG", agent.DefaultConfigPath()), "path to the JSON config file (ARMADA_CONFIG)")
		serverURL   = flag.String("server", os.Getenv("ARMADA_SERVER_URL"), "server URL, overrides config (ARMADA_SERVER_URL)")
		token       = flag.String("token", os.Getenv("ARMADA_ENROLLMENT_TOKEN"), "enrollment token, overrides config (ARMADA_ENROLLMENT_TOKEN)")
		stateDir    = flag.String("state-dir", os.Getenv("ARMADA_STATE_DIR"), "state directory, overrides config (ARMADA_STATE_DIR)")
		name        = flag.String("name", os.Getenv("ARMADA_DEVICE_NAME"), "device name shown in the dashboard (ARMADA_DEVICE_NAME)")
		once        = flag.Bool("once", false, "check in once, wait for any update to finish, and exit")
		showVersion = flag.Bool("version", false, "print version and exit")
		jsonLogs    = flag.Bool("json-logs", os.Getenv("ARMADA_JSON_LOGS") != "", "log as JSON (ARMADA_JSON_LOGS)")
		logFile     = flag.String("log-file", os.Getenv("ARMADA_LOG_FILE"), "log to this file instead of standard error; kept under 10 MB, with one older file (.1) (ARMADA_LOG_FILE)")
		installSvc  = flag.Bool("install-service", false, "Windows: install (or update) the armada-agent service, starting at boot with this -config, and exit")
		removeSvc   = flag.Bool("uninstall-service", false, "Windows: stop and remove the armada-agent service, and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println("armada-agent", agent.Version)
		return nil
	}
	if *installSvc || *removeSvc {
		abs, err := filepath.Abs(*configPath)
		if err != nil {
			return err
		}
		if *removeSvc {
			return uninstallService()
		}
		return installService(abs, *logFile)
	}
	service := isService()
	if service && *logFile == "" {
		*logFile = defaultServiceLog()
	}
	var out io.Writer = os.Stderr
	if *logFile != "" {
		f, err := openLog(*logFile)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	var handler slog.Handler = slog.NewTextHandler(out, nil)
	if *jsonLogs {
		handler = slog.NewJSONHandler(out, nil)
	}
	log := slog.New(handler)
	if *logFile != "" {
		// Nobody sees standard error under a service manager.
		defer func() {
			if err != nil {
				log.Error("armada-agent stopped", "err", err)
			}
		}()
	}

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *serverURL != "" {
		cfg.ServerURL = *serverURL
	}
	if *token != "" {
		cfg.EnrollmentToken = *token
	}
	if *stateDir != "" {
		cfg.StateDir = *stateDir
	}
	if *name != "" {
		cfg.Name = *name
	}
	a, err := agent.New(cfg, log)
	if err != nil {
		return err
	}
	if service {
		// Run under the Windows Service Control Manager, which stops the
		// agent by cancelling ctx.
		return runService(func(ctx context.Context) error { return a.Run(ctx) })
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		if err := a.Enroll(ctx); err != nil {
			return err
		}
		// Check in to learn the server's monitors, run all checks, then report them.
		if err := a.CheckIn(ctx); err != nil {
			return err
		}
		a.Wait()
		a.RunHealthChecks(ctx)
		a.ScanPeripherals(ctx)
		if err := a.CheckIn(ctx); err != nil {
			return err
		}
		a.Wait()
		a.FlashPeripherals(ctx)
		return nil
	}
	return a.Run(ctx)
}

// maxLogSize is when -log-file is started over (the old one kept as .1).
const maxLogSize = 10 << 20

// openLog opens the log file for appending. The agent runs for months, so
// the file is checked on start and, while open, every write; when it grows
// past maxLogSize it is moved to <file>.1 and started again.
func openLog(path string) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w := &rotatingLog{path: path}
	return w, w.open()
}

type rotatingLog struct {
	path string
	f    *os.File
	size int64
}

func (w *rotatingLog) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, fi.Size()
	if w.size > maxLogSize {
		return w.rotate()
	}
	return nil
}

func (w *rotatingLog) rotate() error {
	w.f.Close()
	_ = os.Remove(w.path + ".1")
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w.f, w.size = f, 0
	return nil
}

// Write is called by one slog handler, which serializes its writes.
func (w *rotatingLog) Write(p []byte) (int, error) {
	if w.size+int64(len(p)) > maxLogSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingLog) Close() error { return w.f.Close() }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
