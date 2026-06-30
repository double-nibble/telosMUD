// Command telos-account runs the accounts/auth service (docs/ACCOUNT.md, Phase 14). It is the only service
// that touches OAuth providers + credentials; telos-gate calls its Account gRPC API to redeem link codes,
// verify passphrases, resolve SSH keys, and list/create characters. The world never calls it on the hot path
// — it trusts the signed session assertion (§9). The deployable fourth-and-a-half alongside gate/world/
// director; the website (14.7) attaches to this same service.
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
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/account"
	"github.com/double-nibble/telosmud/internal/assertion"
	"github.com/double-nibble/telosmud/internal/config"
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

	// Link codes (Phase 14.2) live in Redis (cross-process + native TTL). Without Redis the service still
	// boots, but Mint/RedeemLinkCode return Unavailable (the website's Play bridge needs a code store).
	var codes account.LinkCodeStore
	if cfg.Redis.Addr != "" {
		rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
		defer func() { _ = rdb.Close() }()
		codes = account.NewRedisLinkCodes(rdb)
		svc.WithLinkCodes(codes)
		slog.Info("link codes enabled (redis)", "addr", cfg.Redis.Addr)
	} else {
		slog.Warn("no Redis configured: link codes disabled (Mint/RedeemLinkCode unavailable)")
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

	// Website + OAuth (Phase 14.7): served on cfg.WebListen when configured. It needs the link-code store
	// (the Play button) and GitHub OAuth credentials (from the gitignored auth.local.env). Without a listen
	// addr the website is off (the gRPC API still serves).
	var webSrv *http.Server
	if cfg.WebListen != "" && codes != nil {
		web := newWebsite(cfg, pool, codes)
		webSrv = &http.Server{Addr: cfg.WebListen, Handler: web.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			slog.Info("website listening", "addr", cfg.WebListen, "oauth", cfg.GithubClientID != "")
			if err := webSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("website serve failed", "err", err)
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

// newWebsite builds the Phase-14.7 website over the store + the link-code minter. The OAuth redirect defaults
// to the dev callback; the session key is loaded from config or generated ephemeral (a restart then drops
// existing web sessions — fine for dev, set TELOS_WEB_SESSION_KEY in prod).
func newWebsite(cfg config.Config, pool *store.Pool, codes account.LinkCodeStore) *web.Server {
	redirect := cfg.OAuthRedirectURL
	if redirect == "" {
		redirect = "http://localhost:8080/auth/github/callback"
	}
	return web.New(pool, codes, web.Config{
		Provider:      web.GitHubProvider(cfg.GithubClientID, cfg.GithubClientSecret, redirect),
		SessionKey:    webSessionKey(cfg.WebSessionKey),
		SecureCookies: cfg.WebSecureCookies, // secure-by-default (config); dev over plain http sets TELOS_WEB_SECURE_COOKIES=0
		GateHint:      cfg.WebGateHint,
		Log:           slog.Default(),
	})
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
