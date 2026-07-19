// Package config loads service configuration from an optional YAML file with
// environment overrides. Precedence: built-in defaults < YAML file < TELOS_* env.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the shared configuration surface for all TelosMUD services.
type Config struct {
	Service  string         `yaml:"service"`
	Env      string         `yaml:"env"`
	LogLevel string         `yaml:"log_level"`
	Postgres PostgresConfig `yaml:"postgres"`
	Redis    RedisConfig    `yaml:"redis"`
	NATS     NATSConfig     `yaml:"nats"`

	// Transport posture: TLS telnet is the encrypted default; PLAIN telnet is OFF unless
	// explicitly enabled (play would otherwise cross the wire in cleartext).
	GateAllowPlaintext         bool   `yaml:"gate_allow_plaintext"`            // enable the unencrypted telnet listener (default false)
	GateTLSListen              string `yaml:"gate_tls_listen"`                 // TLS telnet listen, e.g. ":4443" (needs cert+key)
	GateTLSCert                string `yaml:"gate_tls_cert"`                   // PEM cert file
	GateTLSKey                 string `yaml:"gate_tls_key"`                    // PEM key file
	DevAutoAuth                bool   `yaml:"dev_auto_auth"`                   // Phase 15.6: bypass OAuth with the bare name login (DEV/TEST ONLY)
	DevAutoAuthAllowRemoteBind bool   `yaml:"dev_auto_auth_allow_remote_bind"` // permit a dev-autoauth gate on a non-loopback bind (sandboxed orchestration only; see gate bind guard)

	// Tunables are the operator-adjustable engine limits (#368). Defaults match the compiled-in values, so
	// an untouched deployment behaves exactly as before.
	//
	// NOT every timeout in the engine belongs here, and that is deliberate. Several have ordering or
	// relationship invariants — adoptConfirmDeadline must be >= pendingTTL, the drain-reservation TTL is
	// derived from the drain deadline (#334) — and exposing those as free knobs would let a plausible-looking
	// value break an invariant a test exists to pin. What is exposed here are bounds whose only relationship
	// is to the workload.
	Tunables TunablesConfig `yaml:"tunables"`

	// GateWriteTimeout (Phase 16.3) bounds a single outbound write to a telnet client; a wedged client that
	// blocks a write past this is disconnected so it can't pin a writer goroutine / hold its slot. 0 disables.
	GateWriteTimeout time.Duration `yaml:"gate_write_timeout"`

	// Phase 1 service addresses.
	GateListen  string `yaml:"gate_listen"`  // telnet listen, e.g. ":4000"
	WorldListen string `yaml:"world_listen"` // gRPC Play listen, e.g. ":9090"
	WorldTarget string `yaml:"world_target"` // the gate dials this world shard

	// Phase 14 auth service (telos-account).
	AccountListen string `yaml:"account_listen"` // account gRPC listen, e.g. ":9100"
	AccountTarget string `yaml:"account_target"` // the gate dials telos-account here ("" => no account service, stub login)
	// AccountCallerToken (#247) is a shared secret authenticating the CALLER of the account gRPC API: the gate
	// sends it on every call, and telos-account requires it, so only the trusted gate can reach the privileged
	// RPCs (SetAccountTier's caller-asserted actor; IssueSessionAssertion's signing oracle). High-entropy;
	// sourced from the gitignored env file like GithubClientSecret. REQUIRED outside dev (telos-account refuses
	// to serve without it); in dev an empty token allows the open listener with a loud warning (local rigs +
	// the TELOS_DEV_AUTOAUTH stub path, which never dials gRPC anyway).
	AccountCallerToken string `yaml:"account_caller_token"`
	// AllowInsecure (#247/#251) explicitly opts INTO running security-sensitive services without their
	// cluster secrets: telos-account with no caller token (an OPEN gRPC API) and a discoverable world shard
	// with no handoff verify key (UNAUTHENTICATED Prepare). It defaults FALSE so the ABSENCE of config fails
	// CLOSED — a production deploy that simply forgot the secret refuses to boot, rather than silently serving
	// unauthenticated. It is DELIBERATELY separate from Env (which defaults to "dev"): keying the insecure
	// allowance off Env would make the default env select the insecure branch. Set TELOS_ALLOW_INSECURE=1 only
	// on a trusted local/dev rig (the dev docker-compose does).
	AllowInsecure bool `yaml:"allow_insecure"`
	// Session-assertion keys (Phase 14.3, ACCOUNT.md §9). AccountSigningKey is account's Ed25519 PRIVATE key
	// (base64; the 64-byte key or the 32-byte seed) used to SIGN assertions; AccountVerifyKey is the matching
	// PUBLIC key (base64) the WORLD verifies with offline. Both empty => assertions are off (the gate trusts
	// the account_id directly, the world skips verification — dev / pre-14.3 behavior).
	AccountSigningKey string `yaml:"account_signing_key"`
	AccountVerifyKey  string `yaml:"account_verify_key"`

	// Cross-shard handoff keypair (docs/REMAINING.md §1). A SHARED cluster Ed25519 keypair (base64; the
	// signing key is the 64-byte key or 32-byte seed) every world shard holds: a shard SIGNS its outgoing
	// Handoff.Prepare snapshots with HandoffSigningKey and VERIFIES incoming ones with HandoffVerifyKey, so
	// a forged Prepare cannot inject arbitrary player state. Both empty => handoff signing is off (dev/test;
	// the pre-signing behavior). Distinct from the account keys above (a different trust boundary).
	HandoffSigningKey string `yaml:"handoff_signing_key"`
	HandoffVerifyKey  string `yaml:"handoff_verify_key"`

	// Phase 14.7 / 15 OAuth broker (served by telos-account).
	WebListen          string `yaml:"web_listen"`           // broker listen, e.g. ":8080" ("" => broker off)
	WebPublicURL       string `yaml:"web_public_url"`       // the broker's externally-visible base URL (the /login/<code> link + OAuth callback derive from it)
	WebSessionKey      string `yaml:"web_session_key"`      // base64 HMAC key for the signed OAuth-flow cookie (ephemeral if empty)
	WebSecureCookies   bool   `yaml:"web_secure_cookies"`   // set Secure on cookies (default true; dev over plain http sets 0)
	GithubClientID     string `yaml:"github_client_id"`     // GitHub OAuth app client id
	GithubClientSecret string `yaml:"github_client_secret"` // GitHub OAuth app client secret (from a gitignored env file)
	MaxCharacters      int    `yaml:"max_characters"`       // per-account character cap (Phase 15.4; 0 => the service default)
	BootstrapAdmin     string `yaml:"bootstrap_admin"`      // config-pin (#27): the OAuth login whose first account becomes admin ("" disables)

	// Phase 2 shard identity (multi-shard + handoff).
	ShardID   string   `yaml:"shard_id"`   // this shard's id, e.g. "shard-a"
	ShardAddr string   `yaml:"shard_addr"` // public address others dial (gate + peer handoff)
	Zones     []string `yaml:"zones"`      // zone ids this shard hosts

	// External versioned content store (#212 slice 3): a git repo whose tags/SHAs are published
	// content versions. Empty Content.URL => embedded/seeded content only (today's behavior).
	Content ContentStoreConfig `yaml:"content"`
	// ContentPacks is the world's enabled pack set. Empty => the default (the demo pack for a bare
	// dev run; a pulled version is manifest-driven, so the importer reads the pack list from there).
	ContentPacks []string `yaml:"content_packs"`
}

// ContentStoreConfig configures the external versioned content store (#212 slice 3). An empty URL
// disables it — the shard keeps loading embedded/seeded content, exactly as before.
type ContentStoreConfig struct {
	URL      string `yaml:"url"`       // git remote of the content repo ("" => external store off)
	Version  string `yaml:"version"`   // the pinned tag/SHA telos-pull imports
	Token    string `yaml:"token"`     // PAT for a PRIVATE content repo (from a gitignored env; never embed it in URL)
	CacheDir string `yaml:"cache_dir"` // on-disk checkout cache ("" => a user-cache-dir default)
}

// PostgresConfig configures the Postgres connection (the content/character store).
type PostgresConfig struct {
	DSN string `yaml:"dsn"`
}

// RedisConfig configures the Redis connection (the directory).
type RedisConfig struct {
	Addr string `yaml:"addr"`
}

// NATSConfig configures the NATS connection (the comms/events bus).
type NATSConfig struct {
	URL string `yaml:"url"`
}

// Default returns the baseline config pointing at the local dev stack
// (deploy/docker-compose.yml).
func Default() Config {
	return Config{
		Service:  "telos",
		Env:      "dev",
		LogLevel: "info",
		Postgres: PostgresConfig{DSN: "postgres://telos:telos@localhost:5432/telosmud?sslmode=disable"}, //nolint:gosec // local-dev default DSN; prod creds come from env
		Redis:    RedisConfig{Addr: "localhost:6379"},
		NATS:     NATSConfig{URL: "nats://localhost:4222"},

		GateListen:       ":4000",
		WorldListen:      ":9090",
		WorldTarget:      "localhost:9090",
		GateWriteTimeout: 30 * time.Second, // Phase 16.3: a wedged client is reclaimed after 30s of a blocked write

		AccountListen: ":9100",
		AccountTarget: "", // empty by default: the gate uses the stub login until an account service is wired

		WebSecureCookies: true,                    // secure-by-default; dev over plain http opts out via TELOS_WEB_SECURE_COOKIES=0
		WebPublicURL:     "http://localhost:8080", // the broker's dev base URL (the /login link + OAuth callback derive from it)

		ShardID:   "shard-1",
		ShardAddr: "localhost:9090",
		Zones:     []string{"midgaard"},
	}
}

// TunablesConfig holds the operator-adjustable engine limits (#368). A zero field means "use the
// compiled-in default", so a partially-specified block is not a way to accidentally disable a bound.
//
// PLAIN DATA, validated by the subsystem that owns the bounds — world.SetLuaCaps for these two. This
// package stays a LEAF (stdlib + yaml only), which is not tidiness: importing internal/luasandbox from here
// to reach a pair of int constants linked a Lua interpreter into telos-gate, telos-migrate, telos-pull and
// telos-seed, none of which ever construct a VM. The next tunables in this block have their bounds in
// internal/world, so the same import would drag the whole world package into every binary.
//
// It also makes the safer thing structural: the setter returns an error, so a host cannot inject without
// validating, whereas a separate Validate() here is something a fourth binary can simply forget to call.
type TunablesConfig struct {
	// LuaInstrBudget is the per-call Lua VM instruction cap — the PRIMARY bound on a content script, and the
	// thing that stops a runaway loop from stalling a zone's actor goroutine. On this engine that means
	// stalling every player in that zone, which is why it is bounds-checked rather than taken as given.
	// TELOS_LUA_INSTR_BUDGET. 0 => the engine default (100k).
	LuaInstrBudget int `yaml:"lua_instr_budget"`

	// LuaCallDeadlineMS is the per-call Lua wall-clock deadline in milliseconds — the SECONDARY guard,
	// catching stalls the instruction count cannot see (a GC pause, host load, a slow builtin).
	// TELOS_LUA_CALL_DEADLINE_MS. 0 => the engine default, which is already scaled up under `-race`.
	//
	// Raising it is the usual operator need (a busy host tripping the stall guard on legitimate content).
	// LOWERING it below what a legitimate builtin needs spuriously aborts correct scripts, which is why the
	// validator has a floor.
	LuaCallDeadlineMS int `yaml:"lua_call_deadline_ms"`

	// err carries a malformed TELOS_* value forward so the host refuses the boot rather than silently
	// running the default. Unexported: it is not configuration, it is a parse outcome.
	err error
}

// Err reports a malformed tunable env value, if any. The host checks it alongside the subsystem's own
// validation.
func (t TunablesConfig) Err() error { return t.err }

// PathFromEnv returns the config file path from TELOS_CONFIG (may be empty).
func PathFromEnv() string { return os.Getenv("TELOS_CONFIG") }

// Load returns defaults, overlaid by the YAML file at path (if present), then by
// TELOS_* environment variables. An empty or missing path is not an error.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		path = filepath.Clean(path)
		data, err := os.ReadFile(path) //nolint:gosec // config path is operator-supplied (TELOS_CONFIG); cleaned above, no privilege boundary crossed.
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parse config %q: %w", path, err)
			}
		case !os.IsNotExist(err):
			return cfg, fmt.Errorf("read config %q: %w", path, err)
		}
	}
	cfg.applyEnv()
	return cfg, nil
}

func (c *Config) applyEnv() {
	if v, ok := os.LookupEnv("TELOS_SERVICE"); ok {
		c.Service = v
	}
	if v, ok := os.LookupEnv("TELOS_ENV"); ok {
		c.Env = v
	}
	if v, ok := os.LookupEnv("TELOS_LOG_LEVEL"); ok {
		c.LogLevel = v
	}
	if v, ok := os.LookupEnv("TELOS_POSTGRES_DSN"); ok {
		c.Postgres.DSN = v
	}
	if v, ok := os.LookupEnv("TELOS_REDIS_ADDR"); ok {
		c.Redis.Addr = v
	}
	if v, ok := os.LookupEnv("TELOS_NATS_URL"); ok {
		c.NATS.URL = v
	}
	if v, ok := os.LookupEnv("TELOS_GATE_LISTEN"); ok {
		c.GateListen = v
	}
	if v, ok := os.LookupEnv("TELOS_WORLD_LISTEN"); ok {
		c.WorldListen = v
	}
	if v, ok := os.LookupEnv("TELOS_ACCOUNT_LISTEN"); ok {
		c.AccountListen = v
	}
	if v, ok := os.LookupEnv("TELOS_ACCOUNT_TARGET"); ok {
		c.AccountTarget = v
	}
	if v, ok := os.LookupEnv("TELOS_ACCOUNT_CALLER_TOKEN"); ok {
		c.AccountCallerToken = v
	}
	if v, ok := os.LookupEnv("TELOS_ACCOUNT_SIGNING_KEY"); ok {
		c.AccountSigningKey = v
	}
	if v, ok := os.LookupEnv("TELOS_ACCOUNT_VERIFY_KEY"); ok {
		c.AccountVerifyKey = v
	}
	if v, ok := os.LookupEnv("TELOS_HANDOFF_SIGNING_KEY"); ok {
		c.HandoffSigningKey = v
	}
	if v, ok := os.LookupEnv("TELOS_HANDOFF_VERIFY_KEY"); ok {
		c.HandoffVerifyKey = v
	}
	if v, ok := os.LookupEnv("TELOS_GATE_ALLOW_PLAINTEXT"); ok {
		c.GateAllowPlaintext = v == "1" || strings.EqualFold(v, "true")
	}
	// The TELOS_DEV_AUTOAUTH* env reads are split across build tags (#96): devauth_dev.go applies them in a
	// `-tags telos_devauth` build; devauth_release.go leaves DevAutoAuth false (and warns if the operator set
	// the env) in the default release build, so the OAuth bypass cannot be turned on in production config.
	applyDevAuthEnv(c)
	if v, ok := os.LookupEnv("TELOS_ALLOW_INSECURE"); ok {
		c.AllowInsecure = v == "1" || strings.EqualFold(v, "true")
	}
	if v, ok := os.LookupEnv("TELOS_GATE_WRITE_TIMEOUT"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			c.GateWriteTimeout = d
		}
	}
	if v, ok := os.LookupEnv("TELOS_GATE_TLS_LISTEN"); ok {
		c.GateTLSListen = v
	}
	if v, ok := os.LookupEnv("TELOS_GATE_TLS_CERT"); ok {
		c.GateTLSCert = v
	}
	if v, ok := os.LookupEnv("TELOS_GATE_TLS_KEY"); ok {
		c.GateTLSKey = v
	}
	if v, ok := os.LookupEnv("TELOS_WEB_LISTEN"); ok {
		c.WebListen = v
	}
	if v, ok := os.LookupEnv("TELOS_WEB_SESSION_KEY"); ok {
		c.WebSessionKey = v
	}
	if v, ok := os.LookupEnv("TELOS_WEB_SECURE_COOKIES"); ok {
		c.WebSecureCookies = v != "0" && !strings.EqualFold(v, "false")
	}
	if v, ok := os.LookupEnv("TELOS_WEB_PUBLIC_URL"); ok {
		c.WebPublicURL = v
	}
	if v, ok := os.LookupEnv("TELOS_MAX_CHARACTERS"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxCharacters = n
		}
	}
	if v, ok := os.LookupEnv("TELOS_GITHUB_CLIENT_ID"); ok {
		c.GithubClientID = v
	}
	if v, ok := os.LookupEnv("TELOS_GITHUB_CLIENT_SECRET"); ok {
		c.GithubClientSecret = v
	}
	if v, ok := os.LookupEnv("TELOS_BOOTSTRAP_ADMIN"); ok {
		c.BootstrapAdmin = v
	}
	if v, ok := os.LookupEnv("TELOS_WORLD_TARGET"); ok {
		c.WorldTarget = v
	}
	if v, ok := os.LookupEnv("TELOS_SHARD_ID"); ok {
		c.ShardID = v
	}
	if v, ok := os.LookupEnv("TELOS_SHARD_ADDR"); ok {
		c.ShardAddr = v
	}
	if v, ok := os.LookupEnv("TELOS_ZONES"); ok {
		c.Zones = splitCSV(v)
	}
	if v, ok := os.LookupEnv("TELOS_CONTENT_URL"); ok {
		c.Content.URL = v
	}
	if v, ok := os.LookupEnv("TELOS_CONTENT_VERSION"); ok {
		c.Content.Version = v
	}
	if v, ok := os.LookupEnv("TELOS_CONTENT_TOKEN"); ok {
		c.Content.Token = v
	}
	if v, ok := os.LookupEnv("TELOS_CONTENT_CACHE"); ok {
		c.Content.CacheDir = v
	}
	if v, ok := os.LookupEnv("TELOS_CONTENT_PACKS"); ok {
		c.ContentPacks = splitCSV(v)
	}
	// Tunables (#368). A malformed value is IGNORED here rather than silently coerced to 0 — 0 means "use
	// the default", so parsing "abc" as 0 would quietly hand back the default while the operator believed
	// their setting had taken. Validate() then reports anything out of range.
	// A malformed value is RECORDED as an error rather than ignored. Atoi("abc") yields 0, and 0 means "use
	// the default" — so coercing would hand back the default while the operator believed their setting had
	// taken, which is the silent misconfiguration this whole feature exists to end. The host reports it and
	// refuses the boot.
	if v, ok := os.LookupEnv("TELOS_LUA_INSTR_BUDGET"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			c.Tunables.LuaInstrBudget = n
		} else {
			c.Tunables.err = fmt.Errorf("TELOS_LUA_INSTR_BUDGET=%q is not a number", v)
		}
	}
	if v, ok := os.LookupEnv("TELOS_LUA_CALL_DEADLINE_MS"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			c.Tunables.LuaCallDeadlineMS = n
		} else {
			c.Tunables.err = fmt.Errorf("TELOS_LUA_CALL_DEADLINE_MS=%q is not a number", v)
		}
	}
}

// splitCSV parses a comma-separated env value into a trimmed, non-empty list.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
