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
	"errors"
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
	"github.com/double-nibble/telosmud/internal/callerauth"
	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/obs"
	"github.com/double-nibble/telosmud/internal/store"
	"github.com/double-nibble/telosmud/internal/web"
)

func main() {
	// Break-glass CLI (#108): `telos-account set-tier ...` runs a one-shot admin write and exits, instead of
	// serving. It is the sanctioned last-admin-lockout recovery — whoever can run this binary against the DB
	// already has host/DB access, which IS the authorization, so it bypasses the in-game admin check + ceilings.
	if len(os.Args) > 1 && os.Args[1] == "set-tier" {
		os.Exit(runSetTierCLI(os.Args[2:]))
	}

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

	// Content (Phase 14.8/15.4 + #27/#29 + #246): load the pack set that feeds BOTH the chargen flow and the
	// trust ladder. telos-account MUST resolve the SAME pack set the world does — the world applies the content
	// ladder as engine flags while this service authorizes promotes against it, so a divergent pack set would
	// let a promote through that the world then applies as a richer flag set (builder→admin). loadAccountContent
	// reads the registry, resolves via the shared content.ResolveEnabledPacks, and FAILS CLOSED (ladder marked
	// unavailable → SetAccountTier refuses) on a real content/registry error rather than silently serving a
	// possibly-divergent default. A content reload still needs a restart here (#248 tracks closing that window).
	lc, ladderVersion, ladderOK := loadAccountContent(ctx, pool, cfg.ContentPacks, cfg.AllowInsecure)
	if ladderOK {
		// Trust ladder (#27/#29 Slice 0b): SetAccountTier validates + authorizes promotes against it. An empty
		// content ladder falls back to the built-in player/builder/admin ladder (round-8 authz).
		svc.WithTrustLadder(lc.TrustTiers)
		// Staleness guard (#248): pin the version the ladder was loaded at + a live reader, so SetAccountTier
		// fails closed if a content reload bumps the version after boot (the ladder here is not hot-reloaded).
		svc.WithContentVersionGuard(ladderVersion, pool.ContentVersion)
		slog.Info("content trust ladder loaded", "tiers", len(lc.TrustTiers), "content_version", ladderVersion)
	} else {
		svc.WithTrustLadderUnavailable()
	}
	if flow, options, ok := chargenFrom(lc); ok {
		svc.WithChargen(flow, options)
		slog.Info("chargen flow loaded", "steps", len(flow.Steps), "bundle_options", len(options))
	} else {
		slog.Warn("no chargen flow in content: the gate offers no create-character flow")
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

	// Caller authentication (#247): require the shared caller token on every RPC so only the trusted gate can
	// reach the privileged API (SetAccountTier's caller-asserted actor; IssueSessionAssertion's signing
	// oracle). FAIL CLOSED by default — refuse to serve an OPEN listener — unless TELOS_ALLOW_INSECURE was
	// explicitly set (a trusted dev rig; the interceptor then no-ops and the TELOS_DEV_AUTOAUTH stub path never
	// dials gRPC anyway). The decision is factored into callerAuthGate so the fail-closed behavior is testable.
	if warn, fatal := callerAuthGate(cfg.AccountCallerToken, cfg.AllowInsecure); fatal != nil {
		slog.Error("refusing to start", "err", fatal)
		os.Exit(1)
	} else if warn != "" {
		slog.Warn(warn)
	}

	lis, err := net.Listen("tcp", cfg.AccountListen)
	if err != nil {
		slog.Error("listen failed", "addr", cfg.AccountListen, "err", err)
		os.Exit(1)
	}
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(callerauth.Interceptor(cfg.AccountCallerToken)),
		grpc.ChainStreamInterceptor(callerauth.StreamInterceptor(cfg.AccountCallerToken)), // can't-forget guard for a future streaming RPC
	)
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

// packSetGate is the FAIL-CLOSED boot decision for the #259 pack-set divergence check (content.
// CheckPackSetConsistency): an explicit TELOS_CONTENT_PACKS override that disagrees with the published set is
// fatal unless TELOS_ALLOW_INSECURE was explicitly set, so a telos-account that would authorize promotes
// against a different trust ladder than the world applies refuses to boot rather than silently diverge.
// Mirrors cmd/telos-world's gate. Both empty => consistent (or nothing published to compare).
func packSetGate(divergErr error, allowInsecure bool) (warn string, fatal error) {
	if divergErr == nil {
		return "", nil
	}
	if !allowInsecure {
		return "", divergErr
	}
	return "insecure content pack-set (TELOS_ALLOW_INSECURE): " + divergErr.Error(), nil
}

// callerAuthGate is the FAIL-CLOSED boot decision for the account gRPC caller token (#247), factored out of
// main so it is unit-testable. It returns a fatal error when the API would be OPEN (no caller token) and the
// insecure mode was NOT explicitly opted into — so a production deploy that merely forgot the token refuses to
// boot rather than silently serving unauthenticated. A non-empty warn string means "running open under an
// explicit TELOS_ALLOW_INSECURE opt-in". Both empty => a token is set and the server starts silently.
//
// Crucially the allowance keys on allowInsecure (TELOS_ALLOW_INSECURE, default false), NOT on the environment
// name — cfg.Env defaults to "dev", so keying off it would make the DEFAULT config select the insecure branch.
func callerAuthGate(callerToken string, allowInsecure bool) (warn string, fatal error) {
	if callerToken != "" {
		return "", nil
	}
	if !allowInsecure {
		return "", errors.New("no account caller token (TELOS_ACCOUNT_CALLER_TOKEN) — the gRPC API would accept " +
			"UNAUTHENTICATED callers (self-promote / assertion-mint); set a shared token, or TELOS_ALLOW_INSECURE=1 " +
			"on a trusted dev rig")
	}
	return "no account caller token: the gRPC API is OPEN (TELOS_ALLOW_INSECURE) — anyone who can dial it may assert any actor", nil
}

// loadAccountContent resolves + loads the content pack set telos-account serves (#246), returning the loaded
// content and whether the trust ladder is TRUSTED (safe to authorize promotes against). It mirrors the world's
// loadContent resolution so the two services never diverge:
//
//   - Read the content-version registry. A CLEAN read (verr==nil) gives the authoritative registered packs; a
//     FRESH DB (store.ErrNoContentVersion) means "never pulled" — the demo/override default is correct, and
//     the ladder is trusted. A REAL read error means we cannot know which packs the world loaded → FAIL CLOSED.
//   - Resolve the enabled packs with the shared content.ResolveEnabledPacks(explicit-override, registry).
//   - LoadWithCore that set (the same call the world uses, so the effective content matches). A load error →
//     FAIL CLOSED.
//
// Both reads get a bounded retry so a transient Postgres blip at boot doesn't wedge tier management for the
// process lifetime (a genuine outage still fails closed after the retries — the break-glass CLI, which bypasses
// the service, remains the recovery). "Fail closed" = returns ladderOK=false; the caller then marks the ladder
// unavailable so SetAccountTier refuses every tier change. lc is always non-nil (an empty LoadedContent on
// failure) so chargen degrades to unconfigured rather than crashing.
func loadAccountContent(ctx context.Context, pool *store.Pool, packsOverride []string, allowInsecure bool) (lc *content.LoadedContent, version uint64, ladderOK bool) {
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
				i = attempts // stop retrying on shutdown; fall through to fail-closed
			case <-time.After(2 * time.Second):
			}
			if i >= attempts {
				break
			}
		}
		registry, verr := pool.CurrentContentVersion(ctx)
		switch {
		case verr == nil:
			// authoritative registry (or a fresh DB's version-0 empty set — demo, correct + trusted).
		case errors.Is(verr, store.ErrNoContentVersion):
			// A fresh DB with no version row: demo is the right default; trusted. Not an error to retry.
		default:
			lastErr = verr
			slog.Warn("content version registry read failed; retrying", "attempt", i+1, "err", verr)
			continue
		}
		// #259: on a FRESH DB (nothing published) an explicit override is the legitimate bootstrap path but
		// cannot be cross-checked against the world — warn so an operator keeps both processes on the SAME set.
		if len(packsOverride) > 0 && len(registry.Packs) == 0 {
			slog.Warn("TELOS_CONTENT_PACKS is set but no content is published yet (empty registry) — the pack-set " +
				"cross-check (#259) cannot run on a fresh DB; ensure telos-world and telos-account pin the SAME set")
		}
		// #259: refuse to boot on a pack-set divergence — an explicit TELOS_CONTENT_PACKS that disagrees with
		// the published set would make this service authorize promotes against a different trust ladder than
		// the world applies (builder→admin escalation the same-version #248 guard misses). Fail closed unless
		// TELOS_ALLOW_INSECURE. Checked here where the authoritative registry is in hand.
		if warn, fatal := packSetGate(content.CheckPackSetConsistency(packsOverride, registry.Packs), allowInsecure); fatal != nil {
			slog.Error("refusing to start", "err", fatal)
			os.Exit(1)
		} else if warn != "" {
			slog.Warn(warn)
		}
		enabled := content.ResolveEnabledPacks(packsOverride, registry.Packs)
		loaded, lerr := content.LoadWithCore(ctx, pool, enabled)
		if lerr != nil {
			lastErr = lerr
			slog.Warn("content load failed; retrying", "attempt", i+1, "packs", enabled, "err", lerr)
			continue
		}
		slog.Info("content resolved for telos-account", "packs", enabled, "content_version", registry.Version)
		return loaded, registry.Version, true
	}
	// Exhausted the retries on a real error: refuse to authorize promotes against an unknown ladder.
	slog.Error("content unavailable after retries; trust-tier changes will be REFUSED until it loads (fail closed) — "+
		"break-glass recovery: `telos-account set-tier`", "err", lastErr)
	return &content.LoadedContent{}, 0, false
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
