// Command armada-server runs the Armada fleet control plane.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nasi-lemak/armada/internal/server"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "armada-server:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func run() error {
	cfg := server.DefaultConfig()
	var (
		listen      = flag.String("listen", env("ARMADA_LISTEN", ":8080"), "address to listen on (ARMADA_LISTEN)")
		adminToken  = flag.String("admin-token", os.Getenv("ARMADA_ADMIN_TOKEN"), "admin API token; generated into <data-dir>/admin-token if unset (ARMADA_ADMIN_TOKEN)")
		tlsCert     = flag.String("tls-cert", os.Getenv("ARMADA_TLS_CERT"), "TLS certificate file (ARMADA_TLS_CERT)")
		tlsKey      = flag.String("tls-key", os.Getenv("ARMADA_TLS_KEY"), "TLS key file (ARMADA_TLS_KEY)")
		stages      = flag.String("default-stages", env("ARMADA_DEFAULT_STAGES", "5,25,50,100"), "default rollout stages, cumulative percentages")
		showVersion = flag.Bool("version", false, "print version and exit")
		maxArtifact = flag.Int64("max-artifact-mb", 2048, "maximum release artifact size in MiB")
		jsonLogs    = flag.Bool("json-logs", os.Getenv("ARMADA_JSON_LOGS") != "", "log as JSON (ARMADA_JSON_LOGS)")
	)
	flag.StringVar(&cfg.DataDir, "data-dir", env("ARMADA_DATA_DIR", "./data"), "directory for the database, artifacts and keys (ARMADA_DATA_DIR)")
	flag.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", envDur("ARMADA_HEARTBEAT_INTERVAL", cfg.HeartbeatInterval), "how often agents check in")
	flag.DurationVar(&cfg.OfflineAfter, "offline-after", envDur("ARMADA_OFFLINE_AFTER", cfg.OfflineAfter), "mark devices offline after this long without a heartbeat")
	flag.DurationVar(&cfg.EngineInterval, "engine-interval", envDur("ARMADA_ENGINE_INTERVAL", cfg.EngineInterval), "how often rollouts are evaluated")
	flag.DurationVar(&cfg.UpdateTimeout, "update-timeout", envDur("ARMADA_UPDATE_TIMEOUT", cfg.UpdateTimeout), "fail device updates stuck downloading/installing this long")
	var stageDur time.Duration
	flag.DurationVar(&stageDur, "default-stage-duration", envDur("ARMADA_DEFAULT_STAGE_DURATION", time.Duration(cfg.DefaultStrategy.StageDurationSeconds)*time.Second), "default soak time per rollout stage")
	flag.Float64Var(&cfg.DefaultStrategy.MaxFailureRate, "default-max-failure-rate", cfg.DefaultStrategy.MaxFailureRate, "default failure rate (0-1) that auto-pauses a rollout")
	flag.Parse()

	if *showVersion {
		fmt.Println("armada-server", version)
		return nil
	}

	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *jsonLogs {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(handler)
	cfg.Logger = log
	cfg.MaxArtifactBytes = *maxArtifact << 20
	cfg.DefaultStrategy.StageDurationSeconds = int64(stageDur.Seconds())
	cfg.DefaultStrategy.Stages = nil
	for _, p := range strings.Split(*stages, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return fmt.Errorf("invalid -default-stages: %w", err)
		}
		cfg.DefaultStrategy.Stages = append(cfg.DefaultStrategy.Stages, n)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	tok, err := resolveAdminToken(*adminToken, cfg.DataDir, log)
	if err != nil {
		return err
	}
	cfg.AdminTokens = []string{tok}

	srv, err := server.New(cfg)
	if err != nil {
		return err
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.Run(ctx)

	hs := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("armada-server listening", "addr", *listen, "version", version, "data_dir", cfg.DataDir, "tls", *tlsCert != "")
		if *tlsCert != "" {
			errc <- hs.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			errc <- hs.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return hs.Shutdown(sctx)
	}
	return nil
}

func resolveAdminToken(flagVal, dataDir string, log *slog.Logger) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	path := filepath.Join(dataDir, "admin-token")
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := "at_" + base64.RawURLEncoding.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	log.Info("generated admin token; use it to sign in to the dashboard and CLI", "file", path)
	fmt.Fprintf(os.Stderr, "\n  Admin token: %s\n\n", tok)
	return tok, nil
}
