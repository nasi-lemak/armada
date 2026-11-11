// Command armada-agent runs on each managed device.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nasi-lemak/armada/internal/agent"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "armada-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", envOr("ARMADA_CONFIG", "/etc/armada/agent.json"), "path to the JSON config file (ARMADA_CONFIG)")
		serverURL   = flag.String("server", os.Getenv("ARMADA_SERVER_URL"), "server URL, overrides config (ARMADA_SERVER_URL)")
		token       = flag.String("token", os.Getenv("ARMADA_ENROLLMENT_TOKEN"), "enrollment token, overrides config (ARMADA_ENROLLMENT_TOKEN)")
		stateDir    = flag.String("state-dir", os.Getenv("ARMADA_STATE_DIR"), "state directory, overrides config (ARMADA_STATE_DIR)")
		name        = flag.String("name", os.Getenv("ARMADA_DEVICE_NAME"), "device name shown in the dashboard (ARMADA_DEVICE_NAME)")
		once        = flag.Bool("once", false, "check in once, wait for any update to finish, and exit")
		showVersion = flag.Bool("version", false, "print version and exit")
		jsonLogs    = flag.Bool("json-logs", os.Getenv("ARMADA_JSON_LOGS") != "", "log as JSON (ARMADA_JSON_LOGS)")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println("armada-agent", agent.Version)
		return nil
	}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *jsonLogs {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(handler)

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
		if err := a.CheckIn(ctx); err != nil {
			return err
		}
		a.Wait()
		return nil
	}
	return a.Run(ctx)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
