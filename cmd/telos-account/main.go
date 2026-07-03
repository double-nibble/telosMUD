// Command telos-account runs the accounts/auth service (docs/ACCOUNT.md). It is the only service that
// touches OAuth providers + identities; telos-gate calls its Account gRPC API to run the browser OAuth
// device login (start/poll), list/create characters, and mint session assertions. The world never calls it
// on the hot path — it trusts the signed session assertion (§9). The deployable fourth-and-a-half alongside
// gate/world/director; the Phase-15 OAuth broker (the one-click login page) serves from this same process.
//
// Startup: load config -> obs.Init -> open the Postgres store (the account/character tables) -> serve the
// Account gRPC API. SIGINT/SIGTERM gracefully stops the server.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/account"
	"github.com/double-nibble/telosmud/internal/assertion"
	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/obs"
	"github.com/double-nibble/telosmud/internal/store"
	"github.com/double-nibble/telosmud/internal/web"
)

func main() {
	cfg, err := config.Load(config.PathFromEnv())
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	if cfg.Service == "telos" {
		cfg.Service = "telos-account"
	}
	shutdown := obs.Init(cfg.Service, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The account/character tables are the service's reason for being — without a DSN there is nothing to
	// authenticate against, so this is fatal.
	if cfg.Postgres.DSN == "" {
		slog.Error("telos-account needs a Postgres DSN (accounts have no durable home without it)")
		os.Exit(1)
	}
	openCtx, cancelOpen := context.WithTimeout(ctx, 15*time.Second)
	pool, err := store.Open(openCtx, cfg.Postgres.DSN)
	cancelOpen()
	if err != nil {
		slog.Error("store open failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	// A freshly-created character starts in the demo pack's start room (Phase 14.8 may let content choose).
	svc := account.New(pool, slog.Default(), "midgaard", "midgaard:room:temple")

	svc.WithMaxCharacters(cfg.MaxCharacters)

	// Content (Phase 14.8/15.4 + #27/#29): load the pack once — it feeds BOTH the chargen flow and the trust
	// ladder (tier validation + promote authz). A content reload needs a restart to take effect here.
	lc, err := content.Load(ctx, pool, []string{content.DemoPack})
	if err != nil {
		slog.Warn("content load failed; using defaults (no chargen flow, built-in trust ladder)", "err", err)
		lc = &content.LoadedContent{}
	}
	if flow, options, ok := chargenFrom(lc); ok {
		svc.WithChargen(flow, options)
		slog.Info("chargen flow loaded", "steps", len(flow.Steps), "bundle_options", len(options))
	} else {
		slog.Warn("no chargen flow in content: the gate offers no create-character flow")
	}
	// Trust ladder (#27/#29 Slice 0b): SetAccountTier validates + authorizes promotes against it. An empty
	// content ladder falls back to the built-in player/builder/admin ladder (round-8 authz).
	svc.WithTrustLadder(lc.TrustTiers)
	if n := len(lc.TrustTiers); n > 0 {
		slog.Info("content trust ladder loaded", "tiers", n)
	}

	// Redis backs the Phase-15 device-auth sessions (the terminal OAuth bridge). Without Redis the device
	// login + the broker are unavailable (the gate's OAuth login needs the store).
	redisUp := false
	if cfg.Redis.Addr != "" {
		rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
		defer func() { _ = rdb.Close() }()
		svc.WithDeviceAuth(account.NewRedisDeviceAuth(rdb), cfg.WebPublicURL)
		redisUp = true
		slog.Info("device auth enabled (redis)", "addr", cfg.Redis.Addr, "public_url", cfg.WebPublicURL)
	} else {
		slog.Warn("no Redis configured: device auth + broker disabled")
	}

	// Session-assertion signing (Phase 14.3): load the Ed25519 private key if configured. Without it,
	// IssueSessionAssertion returns an empty token and the world runs unverified (dev / pre-14.3).
	if cfg.AccountSigningKey != "" {
		priv, err := assertion.ParsePrivateKey(cfg.AccountSigningKey)
		if err != nil {
			slog.Error("invalid account signing key", "err", err)
			os.Exit(1)
		}
		svc.WithSigningKey(priv)
		slog.Info("session-assertion signing enabled (ed25519)")
	} else {
		slog.Warn("no signing key configured: session assertions disabled (world runs unverified)")
	}

	lis, err := net.Listen("tcp", cfg.AccountListen)
	if err != nil {
		slog.Error("listen failed", "addr", cfg.AccountListen, "err", err)
		os.Exit(1)
	}
	gs := grpc.NewServer()
	accountv1.RegisterAccountServer(gs, svc)

	// OAuth broker (Phase 15): served on cfg.WebListen when configured. It needs the device-auth store (Redis)
	// to mark sessions authed, plus GitHub OAuth credentials (from the gitignored auth.local.env). Without a
	// listen addr or Redis the broker is off (the gRPC API still serves).
	var webSrv *http.Server
	if cfg.WebListen != "" && redisUp {
		broker := newBroker(cfg, pool, svc)
		webSrv = &http.Server{Addr: cfg.WebListen, Handler: broker.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			slog.Info("oauth broker listening", "addr", cfg.WebListen, "oauth", cfg.GithubClientID != "")
			if err := webSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("broker serve failed", "err", err)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		if webSrv != nil {
			sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			_ = webSrv.Shutdown(sctx)
			c()
		}
		gs.GracefulStop()
	}()

	slog.Info("starting", "env", cfg.Env, "listen", cfg.AccountListen)
	if err := gs.Serve(lis); err != nil {
		slog.Error("serve failed", "err", err)
	}
	if err := shutdown(context.Background()); err != nil {
		slog.Error("shutdown error", "err", err)
	}
}

// newBroker builds the Phase-15 OAuth broker over the store (identity resolution) + the device authorizer (the
// account Service, in-process). The OAuth callback derives from the broker's public URL; the session key signs
// the OAuth-flow cookie (config or ephemeral — a restart only drops in-flight logins, which is fine).
func newBroker(cfg config.Config, st web.Store, authorizer web.DeviceAuthorizer) *web.Server {
	return web.New(st, web.Config{
		Provider:       web.GitHubProvider(cfg.GithubClientID, cfg.GithubClientSecret, cfg.WebPublicURL+"/auth/github/callback"),
		Authorizer:     authorizer,
		SessionKey:     webSessionKey(cfg.WebSessionKey),
		SecureCookies:  cfg.WebSecureCookies, // secure-by-default (config); dev over plain http sets TELOS_WEB_SECURE_COOKIES=0
		Dev:            cfg.Env == "dev",     // renders the -dev logo badge
		BootstrapAdmin: cfg.BootstrapAdmin,   // config-pin: first account matching this OAuth login → admin (#27)
		Log:            slog.Default(),
	})
}

// chargenFrom extracts the chargen flow + the selectable bundle options (race/class/…) the gate's
// prompt-driven chargen renders from already-loaded content. ok=false when content is absent or defines no
// chargen flow. Pure (no I/O) so the caller loads the pack once and feeds both chargen and the trust ladder.
func chargenFrom(lc *content.LoadedContent) (content.ChargenDTO, []content.ChargenBundleOption, bool) {
	if lc == nil || len(lc.Chargens) == 0 {
		return content.ChargenDTO{}, nil, false
	}
	flow := lc.Chargens[0] // one flow per pack by convention
	options := make([]content.ChargenBundleOption, 0, len(lc.Bundles))
	for _, b := range lc.Bundles {
		// Only race/class/background-style bundles are chargen picks; a profession bundle is learned in-world.
		if b.Kind == "profession" {
			continue
		}
		options = append(options, content.ChargenBundleOption{Ref: b.Ref, Kind: b.Kind, Label: titleize(b.Ref)})
	}
	return flow, options, true
}

// titleize upper-cases the first letter of a bundle ref for a display label ("fighter" -> "Fighter"). A
// content-supplied display name would be richer; the ref is a serviceable label until then.
func titleize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// webSessionKey decodes the configured base64 HMAC key, or generates an ephemeral one (with a warning).
func webSessionKey(b64 string) []byte {
	if b64 != "" {
		if k, err := base64.StdEncoding.DecodeString(b64); err == nil && len(k) >= 16 {
			return k
		}
		slog.Warn("invalid TELOS_WEB_SESSION_KEY (need >=16 bytes base64); using an ephemeral key")
	} else {
		slog.Warn("no web session key configured: using an EPHEMERAL key (web sessions drop on restart)")
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}
