// Package server implements the Armada control plane: the agent protocol,
// the admin REST API, the rollout engine and the embedded dashboard.
package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/attestation"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
	"github.com/nasi-lemak/armada/web"
)

// Config configures a Server.
type Config struct {
	DataDir           string
	AdminTokens       []string
	HeartbeatInterval time.Duration
	// OfflineAfter is how long without a heartbeat before a device shows as offline.
	OfflineAfter time.Duration
	// PeripheralOfflineAfter is the same for peripherals, which gateways report less often.
	PeripheralOfflineAfter time.Duration
	// UpdateTimeout fails updates stuck downloading/installing for this long.
	UpdateTimeout time.Duration
	// EngineInterval is how often rollouts are evaluated.
	EngineInterval time.Duration
	// MaxArtifactBytes caps release uploads.
	MaxArtifactBytes int64
	// EventRetention bounds the activity log.
	EventRetention time.Duration
	// DefaultStrategy is used for deployments created without one (including auto-deploys).
	DefaultStrategy api.Strategy
	Logger          *slog.Logger

	// Android.
	Android AndroidConfig
}

// AndroidConfig controls Android enrollment, licensing and provisioning.
type AndroidConfig struct {
	// AgentPackage is the Android agent's application ID. Its attested keys
	// must be created by this package, and it is the only Android package
	// served publicly for provisioning downloads.
	AgentPackage string
	// AdminReceiver is the agent's DeviceAdminReceiver class, used in the
	// provisioning QR code (e.g. ".AdminReceiver").
	AdminReceiver string
	// RequireAttestation rejects Android enrollments without hardware key attestation.
	RequireAttestation bool
	// RequireVerifiedBoot rejects devices with an unlocked bootloader or modified OS.
	RequireVerifiedBoot bool
	// RequireStrongBox requires keys in a dedicated security chip rather than the TEE.
	RequireStrongBox bool
	// ExtraRootsPEM adds attestation roots (tests or private PKI).
	ExtraRootsPEM []byte
	// StatusURL is Google's attestation revocation list; empty disables checking.
	StatusURL string
	// DisableDebugging tells agents to turn off USB debugging.
	DisableDebugging bool
	// PublicURL is the server URL devices use (for provisioning QR codes).
	// If empty it is derived from the request.
	PublicURL string
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		DataDir:                "./data",
		HeartbeatInterval:      30 * time.Second,
		OfflineAfter:           2 * time.Minute,
		PeripheralOfflineAfter: 20 * time.Minute,
		UpdateTimeout:          time.Hour,
		EngineInterval:         10 * time.Second,
		MaxArtifactBytes:       2 << 30,
		EventRetention:         30 * 24 * time.Hour,
		DefaultStrategy: api.Strategy{
			Stages:               []int{5, 25, 50, 100},
			StageDurationSeconds: 15 * 60,
			AutoAdvance:          true,
			MaxFailureRate:       0.2,
			MinSamples:           3,
		},
		Android: AndroidConfig{
			AgentPackage:        "dev.armada.agent",
			AdminReceiver:       ".AdminReceiver",
			RequireAttestation:  true,
			RequireVerifiedBoot: true,
			StatusURL:           attestation.GoogleStatusURL,
			DisableDebugging:    true,
		},
	}
}

// Server is the Armada control plane.
type Server struct {
	cfg         Config
	store       *store.Store
	signKey     ed25519.PrivateKey
	pubKey      ed25519.PublicKey
	log         *slog.Logger
	now         func() time.Time
	artifactDir string

	licenseKey  ed25519.PrivateKey
	verifier    *attestation.Verifier
	revocations *attestation.RevocationList
	challenges  *challengeStore

	notifyQueue  chan notifyJob
	notifyClient *http.Client
}

// New opens the data directory (creating the database and signing key if
// needed) and returns a ready Server.
func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if len(cfg.AdminTokens) == 0 {
		return nil, errors.New("at least one admin token is required")
	}
	if err := rollout.ValidateStrategy(&cfg.DefaultStrategy); err != nil {
		return nil, fmt.Errorf("default rollout strategy: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	artifactDir := filepath.Join(cfg.DataDir, "artifacts")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "armada.db"))
	if err != nil {
		return nil, err
	}
	key, err := loadOrCreateSigningKey(filepath.Join(cfg.DataDir, "signing.key"))
	if err != nil {
		st.Close()
		return nil, err
	}
	// Licenses use their own key so a leak of one key does not compromise the other.
	licKey, err := loadOrCreateSigningKey(filepath.Join(cfg.DataDir, "license.key"))
	if err != nil {
		st.Close()
		return nil, err
	}
	revocations := &attestation.RevocationList{}
	verifier, err := attestation.NewVerifier(cfg.Android.ExtraRootsPEM, revocations.IsRevoked)
	if err != nil {
		st.Close()
		return nil, err
	}
	s := &Server{
		cfg:         cfg,
		store:       st,
		signKey:     key,
		pubKey:      key.Public().(ed25519.PublicKey),
		log:         cfg.Logger,
		now:         func() time.Time { return time.Now().UTC() },
		artifactDir: artifactDir,
		licenseKey:  licKey,
		verifier:    verifier,
		revocations: revocations,
		challenges:  newChallengeStore(),
	}
	s.notifyQueue = make(chan notifyJob, 1000)
	s.notifyClient = &http.Client{Timeout: 15 * time.Second}
	go s.notifyLoop()
	return s, nil
}

// Close releases resources.
func (s *Server) Close() error {
	close(s.notifyQueue)
	return s.store.Close()
}

// PublicKey returns the base64 release-signing public key.
func (s *Server) PublicKey() string { return base64.StdEncoding.EncodeToString(s.pubKey) }

func loadOrCreateSigningKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("signing key %s is corrupt", path)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"
	if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
		return nil, err
	}
	return priv, nil
}

// Handler returns the HTTP handler for the whole server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Device onboarding (public: the agent binary is not secret; enrollment still needs a token).
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		_, _ = w.Write(web.InstallScript)
	})
	mux.HandleFunc("GET /api/v1/agent/download", s.handleAgentDownload)
	mux.HandleFunc("POST /api/v1/agent/challenge", s.handleChallenge)
	mux.HandleFunc("GET /api/v1/license/jwks", s.handleJWKS)

	// Agent protocol.
	mux.HandleFunc("POST /api/v1/agent/enroll", s.handleEnroll)
	mux.Handle("POST /api/v1/agent/heartbeat", s.deviceAuth(s.handleHeartbeat))
	mux.Handle("POST /api/v1/agent/updates", s.deviceAuth(s.handleUpdateReport))
	mux.Handle("GET /api/v1/agent/releases/{id}/artifact", s.deviceAuth(s.handleAgentArtifact))
	mux.Handle("POST /api/v1/agent/actions/{id}/result", s.deviceAuth(s.handleActionResult))
	mux.Handle("POST /api/v1/agent/license", s.deviceAuth(s.handleLicense))

	// Admin API.
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.adminAuth(h)) }
	admin("GET /api/v1/summary", s.handleSummary)
	admin("GET /api/v1/signing-key", s.handleSigningKey)
	admin("GET /api/v1/devices", s.handleListDevices)
	admin("GET /api/v1/devices/{id}", s.handleGetDevice)
	admin("PATCH /api/v1/devices/{id}", s.handlePatchDevice)
	admin("DELETE /api/v1/devices/{id}", s.handleDeleteDevice)
	admin("GET /api/v1/devices/{id}/actions", s.handleListActions)
	admin("POST /api/v1/devices/{id}/actions", s.handleCreateAction)
	admin("GET /api/v1/tokens", s.handleListTokens)
	admin("POST /api/v1/tokens", s.handleCreateToken)
	admin("DELETE /api/v1/tokens/{id}", s.handleRevokeToken)
	admin("GET /api/v1/releases", s.handleListReleases)
	admin("POST /api/v1/releases", s.handleUploadRelease)
	admin("GET /api/v1/releases/{id}", s.handleGetRelease)
	admin("GET /api/v1/releases/{id}/artifact", s.handleAdminArtifact)
	admin("DELETE /api/v1/releases/{id}", s.handleDeleteRelease)
	admin("GET /api/v1/deployments", s.handleListDeployments)
	admin("POST /api/v1/deployments", s.handleCreateDeployment)
	admin("GET /api/v1/deployments/{id}", s.handleGetDeployment)
	admin("POST /api/v1/deployments/{id}/{op}", s.handleDeploymentOp)
	admin("GET /api/v1/events", s.handleListEvents)
	admin("GET /api/v1/monitors", s.handleListMonitors)
	admin("POST /api/v1/monitors", s.handleCreateMonitor)
	admin("PUT /api/v1/monitors/{id}", s.handleUpdateMonitor)
	admin("DELETE /api/v1/monitors/{id}", s.handleDeleteMonitor)
	admin("GET /api/v1/channels", s.handleListChannels)
	admin("POST /api/v1/channels", s.handleSaveChannel)
	admin("PUT /api/v1/channels/{id}", s.handleSaveChannel)
	admin("DELETE /api/v1/channels/{id}", s.handleDeleteChannel)
	admin("POST /api/v1/channels/{id}/test", s.handleTestChannel)
	admin("GET /api/v1/alert-rules", s.handleListAlertRules)
	admin("POST /api/v1/alert-rules", s.handleSaveAlertRule)
	admin("PUT /api/v1/alert-rules/{id}", s.handleSaveAlertRule)
	admin("DELETE /api/v1/alert-rules/{id}", s.handleDeleteAlertRule)
	admin("GET /api/v1/alerts", s.handleListAlerts)
	admin("GET /api/v1/apps", s.handleListApps)
	admin("PUT /api/v1/apps/{package}", s.handlePutApp)
	admin("DELETE /api/v1/apps/{package}", s.handleDeleteApp)
	admin("POST /api/v1/android/provisioning", s.handleProvisioning)

	// Dashboard.
	static, _ := fs.Sub(web.FS, "static")
	mux.Handle("GET /", securityHeaders(http.FileServerFS(static)))

	return logRequests(s.log, mux)
}

// ---- auth ----

type ctxKey int

const deviceCtxKey ctxKey = 1

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

func (s *Server) adminAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		ok := false
		for _, t := range s.cfg.AdminTokens {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(t)) == 1 {
				ok = true
			}
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, "admin token required")
			return
		}
		next(w, r)
	})
}

func (s *Server) deviceAuth(next func(http.ResponseWriter, *http.Request, *api.Device)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "device key required")
			return
		}
		dev, err := s.store.GetDeviceByKeyHash(r.Context(), hashSecret(tok))
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "unknown device key")
			return
		} else if err != nil {
			s.internalError(w, err)
			return
		}
		next(w, r, dev)
	})
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func newID(prefix string) string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + idEncoding.EncodeToString(b)
}

func newSecret(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(b)
}

// ---- engine ----

// Run evaluates rollouts until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.EngineInterval)
	defer t.Stop()
	lastPrune := time.Time{}
	lastRevocations := time.Time{}
	for {
		if s.cfg.Android.StatusURL != "" && time.Since(lastRevocations) > 12*time.Hour {
			lastRevocations = time.Now()
			fctx, cancel := context.WithTimeout(ctx, time.Minute)
			if err := s.revocations.Fetch(fctx, http.DefaultClient, s.cfg.Android.StatusURL); err != nil && ctx.Err() == nil {
				s.log.Warn("could not refresh attestation revocation list; will retry", "err", err)
				lastRevocations = time.Now().Add(-11 * time.Hour) // retry in an hour
			} else if err == nil {
				s.log.Info("attestation revocation list refreshed", "revoked", s.revocations.Len())
			}
			cancel()
		}
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("rollout engine tick failed", "err", err)
		}
		if time.Since(lastPrune) > time.Hour {
			lastPrune = time.Now()
			if err := s.store.PruneEvents(ctx, s.now().Add(-s.cfg.EventRetention)); err != nil && ctx.Err() == nil {
				s.log.Error("prune events failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs one evaluation pass over all running deployments.
func (s *Server) Tick(ctx context.Context) error {
	now := s.now()
	stale, err := s.store.FailStaleUpdates(ctx, now.Add(-s.cfg.UpdateTimeout), now)
	if err != nil {
		return err
	}
	for _, u := range stale {
		s.event(ctx, "update.failed", u.DeviceID, u.DeploymentID, fmt.Sprintf("%s %s update %s", u.Package, u.Version, u.Message))
	}
	deps, err := s.store.ListDeployments(ctx, api.DeploymentRunning)
	if err != nil {
		return err
	}
	for _, d := range deps {
		if err := s.evaluateDeployment(ctx, d, now); err != nil {
			return err
		}
	}
	return s.evaluateAlerts(ctx, now)
}
