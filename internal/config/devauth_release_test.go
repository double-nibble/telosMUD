//go:build !telos_devauth

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDevAutoAuthEnvIgnoredInRelease (#96): in a release build (no telos_devauth tag) the TELOS_DEV_AUTOAUTH
// env override is inert — cfg.DevAutoAuth stays false whatever the environment says.
func TestDevAutoAuthEnvIgnoredInRelease(t *testing.T) {
	t.Setenv("TELOS_DEV_AUTOAUTH", "1")
	t.Setenv("TELOS_DEV_AUTOAUTH_ALLOW_REMOTE_BIND", "1")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DevAutoAuth || cfg.DevAutoAuthAllowRemoteBind {
		t.Fatalf("release build must ignore TELOS_DEV_AUTOAUTH env; got DevAutoAuth=%v AllowRemoteBind=%v",
			cfg.DevAutoAuth, cfg.DevAutoAuthAllowRemoteBind)
	}
}

// TestDevAutoAuthYAMLForcedFalseInRelease (#96, security review finding 4): even a YAML `dev_auto_auth: true`
// is force-pinned false in a release build, so the invariant "the bypass is off in release" rests on the
// config VALUE, not just the one gate call site — a future direct reader of cfg.DevAutoAuth can't be tripped.
func TestDevAutoAuthYAMLForcedFalseInRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(p, []byte("dev_auto_auth: true\ndev_auto_auth_allow_remote_bind: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DevAutoAuth || cfg.DevAutoAuthAllowRemoteBind {
		t.Fatalf("release build must force dev_auto_auth false even from YAML; got DevAutoAuth=%v AllowRemoteBind=%v",
			cfg.DevAutoAuth, cfg.DevAutoAuthAllowRemoteBind)
	}
}
