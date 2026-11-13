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
	"github.com/nasi-lemak/armada/internal/store"
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
		migStatus   = flag.Bool("migrate-status", false, "print the database schema version and pending migrations, change nothing, and exit")
		maxArtifact = flag.Int64("max-artifact-mb", 2048, "maximum release artifact size in MiB")
		jsonLogs    = flag.Bool("json-logs", os.Getenv("ARMADA_JSON_LOGS") != "", "log as JSON (ARMADA_JSON_LOGS)")
	)
	flag.StringVar(&cfg.DataDir, "data-dir", env("ARMADA_DATA_DIR", "./data"), "directory for the database, artifacts and keys (ARMADA_DATA_DIR)")
	flag.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", envDur("ARMADA_HEARTBEAT_INTERVAL", cfg.HeartbeatInterval), "how often agents check in")
	flag.DurationVar(&cfg.OfflineAfter, "offline-after", envDur("ARMADA_OFFLINE_AFTER", cfg.OfflineAfter), "mark devices offline after this long without a heartbeat")
	flag.DurationVar(&cfg.EngineInterval, "engine-interval", envDur("ARMADA_ENGINE_INTERVAL", cfg.EngineInterval), "how often rollouts are evaluated")
	flag.DurationVar(&cfg.HistoryResolution, "history-resolution", envDur("ARMADA_HISTORY_RESOLUTION", cfg.HistoryResolution), "how often health history is sampled per check (0 disables history)")
	flag.DurationVar(&cfg.HistoryRawRetention, "history-raw-retention", envDur("ARMADA_HISTORY_RAW_RETENTION", cfg.HistoryRawRetention), "keep full-resolution history this long; older history is kept hourly")
	flag.DurationVar(&cfg.HistoryRetention, "history-retention", envDur("ARMADA_HISTORY_RETENTION", cfg.HistoryRetention), "keep hourly history this long")
	flag.DurationVar(&cfg.PushHold, "push-hold", envDur("ARMADA_PUSH_HOLD", cfg.PushHold), "how long agents' long-poll connections are held open, so actions reach them instantly (0 disables; keep below proxy idle timeouts)")
	flag.DurationVar(&cfg.SessionTTL, "session-ttl", envDur("ARMADA_SESSION_TTL", cfg.SessionTTL), "sign users out of the dashboard after this long without activity")
	flag.DurationVar(&cfg.UpdateTimeout, "update-timeout", envDur("ARMADA_UPDATE_TIMEOUT", cfg.UpdateTimeout), "fail device updates stuck downloading/installing this long")
	var stageDur time.Duration
	flag.DurationVar(&stageDur, "default-stage-duration", envDur("ARMADA_DEFAULT_STAGE_DURATION", time.Duration(cfg.DefaultStrategy.StageDurationSeconds)*time.Second), "default soak time per rollout stage")
	flag.Float64Var(&cfg.DefaultStrategy.MaxFailureRate, "default-max-failure-rate", cfg.DefaultStrategy.MaxFailureRate, "default failure rate (0-1) that auto-pauses a rollout")
	flag.StringVar(&cfg.Android.AgentPackage, "android-agent-package", env("ARMADA_ANDROID_AGENT_PACKAGE", cfg.Android.AgentPackage), "application ID of the Android agent")
	flag.StringVar(&cfg.Android.AdminReceiver, "android-admin-receiver", cfg.Android.AdminReceiver, "Android agent DeviceAdminReceiver class (for provisioning QR codes)")
	flag.StringVar(&cfg.Android.PublicURL, "public-url", os.Getenv("ARMADA_PUBLIC_URL"), "URL devices use to reach this server, e.g. https://fleet.example.com (ARMADA_PUBLIC_URL)")
	allowUnverified := flag.Bool("android-allow-unverified-boot", false, "accept Android devices with an unlocked bootloader or custom OS (development only)")
	allowNoAttest := flag.Bool("android-allow-unattested", false, "accept Android enrollments without hardware key attestation (development only; such devices cannot get licenses)")
	allowADB := flag.Bool("android-allow-adb", false, "do not disable USB debugging on Android devices")
	flag.BoolVar(&cfg.Android.RequireStrongBox, "android-require-strongbox", false, "require attested keys to live in a StrongBox security chip, not just the TEE")
	flag.StringVar(&cfg.Android.StatusURL, "attestation-status-url", cfg.Android.StatusURL, "Google attestation revocation list URL; empty disables revocation checks")
	envBool := func(k string, def bool) bool {
		if v := os.Getenv(k); v != "" {
			b, err := strconv.ParseBool(v)
			if err == nil {
				return b
			}
		}
		return def
	}
	flag.BoolVar(&cfg.PasswordLogin, "password-login", envBool("ARMADA_PASSWORD_LOGIN", true), "allow email and password sign-in; turn off when everyone uses single sign-on (the admin token still works) (ARMADA_PASSWORD_LOGIN)")
	flag.BoolVar(&cfg.Require2FA, "require-2fa", envBool("ARMADA_REQUIRE_2FA", false), "make password users set up an authenticator app before using the dashboard or API (ARMADA_REQUIRE_2FA)")
	flag.StringVar(&cfg.TOTPIssuer, "totp-issuer", env("ARMADA_TOTP_ISSUER", "Armada"), "name shown in authenticator apps")
	flag.StringVar(&cfg.OIDC.Issuer, "oidc-issuer", os.Getenv("ARMADA_OIDC_ISSUER"), "OpenID Connect issuer URL for single sign-on, e.g. https://accounts.google.com (ARMADA_OIDC_ISSUER)")
	flag.StringVar(&cfg.OIDC.ClientID, "oidc-client-id", os.Getenv("ARMADA_OIDC_CLIENT_ID"), "OIDC client ID (ARMADA_OIDC_CLIENT_ID)")
	flag.StringVar(&cfg.OIDC.RedirectURL, "oidc-redirect-url", os.Getenv("ARMADA_OIDC_REDIRECT_URL"), "OIDC redirect URL; default <public-url>/api/v1/auth/sso/callback")
	flag.StringVar(&cfg.OIDC.Name, "oidc-name", env("ARMADA_OIDC_NAME", ""), "name on the sign-in button, e.g. Google or Okta")
	oidcScopes := flag.String("oidc-scopes", env("ARMADA_OIDC_SCOPES", ""), "comma-separated scopes (default openid,email,profile; add groups if your provider needs it)")
	oidcDomains := flag.String("oidc-allowed-domains", env("ARMADA_OIDC_ALLOWED_DOMAINS", ""), "comma-separated email domains allowed to sign in with SSO")
	flag.StringVar(&cfg.OIDC.CreateRole, "oidc-create-role", env("ARMADA_OIDC_CREATE_ROLE", ""), "create unknown SSO users with this role (viewer, operator or admin); empty: only users an admin added")
	flag.StringVar(&cfg.OIDC.GroupsClaim, "oidc-groups-claim", env("ARMADA_OIDC_GROUPS_CLAIM", "groups"), "ID token claim with the user's groups")
	flag.StringVar(&cfg.SCIMDefaultRole, "scim-default-role", env("ARMADA_SCIM_DEFAULT_ROLE", "viewer"), "role of users your directory provisions over SCIM while no group mappings exist (with mappings, access comes only from mapped groups)")
	oidcGroupRoles := flag.String("oidc-group-roles", env("ARMADA_OIDC_GROUP_ROLES", ""), "map groups to roles, e.g. fleet-admins=admin,fleet-ops=operator,support=viewer; users in none can't sign in")
	flag.BoolVar(&cfg.AdminTokenDisabled, "disable-admin-token", envBool("ARMADA_DISABLE_ADMIN_TOKEN", false), "turn the admin token off once admin users exist, so every action is tied to a person (ARMADA_DISABLE_ADMIN_TOKEN)")
	rotateToken := flag.Bool("rotate-admin-token", false, "replace the generated admin token with a new one at startup (the old one stops working)")
	trusted := flag.String("trusted-proxies", env("ARMADA_TRUSTED_PROXIES", ""), "comma-separated IPs/CIDRs of reverse proxies whose X-Forwarded-* headers are trusted, e.g. 10.0.0.0/8 (ARMADA_TRUSTED_PROXIES)")
	flag.StringVar(&cfg.RolloutApproval, "rollout-approval", env("ARMADA_ROLLOUT_APPROVAL", "second-person"), "second-person: rollouts stop after their canary until someone other than their creator approves; off: no approval")
	flag.IntVar(&cfg.UnapprovedMaxPercent, "unapproved-max-percent", 10, "largest first stage a rollout may run before approval")
	flag.Parse()
	if cfg.RolloutApproval == "off" || cfg.RolloutApproval == "none" {
		cfg.RolloutApproval = ""
	}
	var perr error
	if cfg.TrustedProxies, perr = server.ParseTrustedProxies(*trusted); perr != nil {
		return perr
	}
	// The client secret only comes from the environment, never argv (visible in ps).
	cfg.OIDC.ClientSecret = os.Getenv("ARMADA_OIDC_CLIENT_SECRET")
	split := func(s string) []string {
		var out []string
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	cfg.OIDC.Scopes, cfg.OIDC.AllowedDomains = split(*oidcScopes), split(*oidcDomains)
	for _, kv := range split(*oidcGroupRoles) {
		g, r, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("invalid -oidc-group-roles entry %q (want group=role)", kv)
		}
		if cfg.OIDC.GroupRoles == nil {
			cfg.OIDC.GroupRoles = map[string]string{}
		}
		cfg.OIDC.GroupRoles[strings.TrimSpace(g)] = strings.TrimSpace(r)
	}
	cfg.Android.RequireVerifiedBoot = !*allowUnverified
	cfg.Android.RequireAttestation = !*allowNoAttest
	cfg.Android.DisableDebugging = !*allowADB

	if *migStatus {
		path := filepath.Join(cfg.DataDir, "armada.db")
		cur, pending, err := store.SchemaStatus(path)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("no database at %s yet; the first start creates schema version %d\n", path, store.LatestSchemaVersion())
			return nil
		} else if err != nil {
			return err
		}
		fmt.Printf("database: %s\nschema version: %d (this build: %d)\n", path, cur, store.LatestSchemaVersion())
		switch {
		case cur > store.LatestSchemaVersion():
			fmt.Println("the database is NEWER than this build: run a newer armada-server")
		case len(pending) == 0:
			fmt.Println("up to date")
		default:
			fmt.Println("pending (applied at next start, after an automatic backup):")
			for _, p := range pending {
				fmt.Println("  " + p)
			}
		}
		return nil
	}
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
	if *rotateToken {
		if *adminToken != "" {
			return errors.New("-rotate-admin-token only applies to the generated token in <data-dir>/admin-token; change ARMADA_ADMIN_TOKEN yourself")
		}
		if err := os.Remove(filepath.Join(cfg.DataDir, "admin-token")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		log.Warn("admin token rotated: the old one no longer works")
	}
	if cfg.Android.PublicURL == "" && (cfg.OIDC.Issuer != "" || cfg.PasswordLogin) {
		log.Warn("-public-url is not set: single sign-on and passkeys stay off until it is")
	}
	if !cfg.AdminTokenDisabled {
		tok, err := resolveAdminToken(*adminToken, cfg.DataDir, log)
		if err != nil {
			return err
		}
		cfg.AdminTokens = []string{tok}
	} else {
		log.Info("admin token disabled; sign in as an admin user")
	}

	srv, err := server.New(cfg)
	if err != nil {
		return err
	}
	defer srv.Close()
	if m := srv.Migration(); len(m.Applied) > 0 {
		log.Info("database schema migrated", "from", m.From, "to", m.To, "applied", strings.Join(m.Applied, "; "), "backup", m.Backup)
	} else {
		log.Info("database schema up to date", "version", m.To)
	}

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
