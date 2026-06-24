// Package config loads service configuration from an optional YAML file with
// environment overrides. Precedence: built-in defaults < YAML file < TELOS_* env.
package config

import (
	"fmt"
	"os"

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

	// Phase 1 service addresses.
	GateListen  string `yaml:"gate_listen"`  // telnet listen, e.g. ":4000"
	WorldListen string `yaml:"world_listen"` // gRPC Play listen, e.g. ":9090"
	WorldTarget string `yaml:"world_target"` // the gate dials this world shard
}

type PostgresConfig struct {
	DSN string `yaml:"dsn"`
}

type RedisConfig struct {
	Addr string `yaml:"addr"`
}

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
		Postgres: PostgresConfig{DSN: "postgres://telos:telos@localhost:5432/telosmud?sslmode=disable"},
		Redis:    RedisConfig{Addr: "localhost:6379"},
		NATS:     NATSConfig{URL: "nats://localhost:4222"},

		GateListen:  ":4000",
		WorldListen: ":9090",
		WorldTarget: "localhost:9090",
	}
}

// PathFromEnv returns the config file path from TELOS_CONFIG (may be empty).
func PathFromEnv() string { return os.Getenv("TELOS_CONFIG") }

// Load returns defaults, overlaid by the YAML file at path (if present), then by
// TELOS_* environment variables. An empty or missing path is not an error.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
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
	if v, ok := os.LookupEnv("TELOS_WORLD_TARGET"); ok {
		c.WorldTarget = v
	}
}
