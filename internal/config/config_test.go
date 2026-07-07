package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultLoad(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Service != "telos" {
		t.Errorf("Service = %q, want telos", cfg.Service)
	}
	if cfg.NATS.URL == "" {
		t.Error("NATS.URL should have a default")
	}
}

func TestMissingFileIsNotError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err != nil {
		t.Errorf("missing file should not error, got %v", err)
	}
}

func TestYAMLOverride(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	body := "service: telos-world\nlog_level: debug\nredis:\n  addr: r:6379\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Service != "telos-world" {
		t.Errorf("Service = %q, want telos-world", cfg.Service)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.Redis.Addr != "r:6379" {
		t.Errorf("Redis.Addr = %q, want r:6379", cfg.Redis.Addr)
	}
	// Untouched field keeps its default.
	if cfg.NATS.URL == "" {
		t.Error("NATS.URL default lost after partial YAML")
	}
}

func TestZonesDefaultAndEnv(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Zones) != 1 || cfg.Zones[0] != "midgaard" {
		t.Errorf("default Zones = %v, want [midgaard]", cfg.Zones)
	}

	t.Setenv("TELOS_ZONES", "midgaard, darkwood ,, sewers")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"midgaard", "darkwood", "sewers"}
	if len(cfg.Zones) != len(want) {
		t.Fatalf("Zones = %v, want %v", cfg.Zones, want)
	}
	for i, z := range want {
		if cfg.Zones[i] != z {
			t.Fatalf("Zones = %v, want %v", cfg.Zones, want)
		}
	}
}

func TestContentStoreConfig(t *testing.T) {
	// Default: external store off (empty URL), no content-pack override.
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Content.URL != "" || len(cfg.ContentPacks) != 0 {
		t.Fatalf("default content store should be off: %+v packs=%v", cfg.Content, cfg.ContentPacks)
	}

	t.Setenv("TELOS_CONTENT_URL", "https://example.com/org/content.git")
	t.Setenv("TELOS_CONTENT_VERSION", "v1.4.0")
	t.Setenv("TELOS_CONTENT_TOKEN", "secret-pat")
	t.Setenv("TELOS_CONTENT_CACHE", "/var/lib/telos/content")
	t.Setenv("TELOS_CONTENT_PACKS", "mainland, underdark ,, ")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Content.URL != "https://example.com/org/content.git" || cfg.Content.Version != "v1.4.0" ||
		cfg.Content.Token != "secret-pat" || cfg.Content.CacheDir != "/var/lib/telos/content" {
		t.Fatalf("content env not applied: %+v", cfg.Content)
	}
	if want := []string{"mainland", "underdark"}; len(cfg.ContentPacks) != len(want) ||
		cfg.ContentPacks[0] != want[0] || cfg.ContentPacks[1] != want[1] {
		t.Fatalf("ContentPacks = %v, want %v", cfg.ContentPacks, want)
	}
}

func TestEnvOverridesYAML(t *testing.T) {
	t.Setenv("TELOS_NATS_URL", "nats://example:4222")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NATS.URL != "nats://example:4222" {
		t.Errorf("NATS.URL = %q, want env override", cfg.NATS.URL)
	}
}
