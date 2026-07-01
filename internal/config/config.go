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

	// Phase 14.6 transport posture: TLS + SSH are the encrypted defaults; PLAIN telnet is OFF unless
	// explicitly enabled (credentials/play would otherwise cross the wire in cleartext).
	GateAllowPlaintext bool   `yaml:"gate_allow_plaintext"` // enable the unencrypted telnet listener (default false)
	GateTLSListen      string `yaml:"gate_tls_listen"`      // TLS telnet listen, e.g. ":4443" (needs cert+key)
	GateTLSCert        string `yaml:"gate_tls_cert"`        // PEM cert file
	GateTLSKey         string `yaml:"gate_tls_key"`         // PEM key file
	DevAutoAuth        bool   `yaml:"dev_auto_auth"`        // Phase 15.6: bypass OAuth with the bare name login (DEV/TEST ONLY)

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

	// Phase 2 shard identity (multi-shard + handoff).
	ShardID   string   `yaml:"shard_id"`   // this shard's id, e.g. "shard-a"
	ShardAddr string   `yaml:"shard_addr"` // public address others dial (gate + peer handoff)
	Zones     []string `yaml:"zones"`      // zone ids this shard hosts
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
	if v, ok := os.LookupEnv("TELOS_DEV_AUTOAUTH"); ok {
		c.DevAutoAuth = v == "1" || strings.EqualFold(v, "true")
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
