//go:build telos_devauth

package config

import "testing"

// TestDevAutoAuthEnvAppliedInDevBuild (#96): under -tags telos_devauth the bypass is compiled in, so the
// TELOS_DEV_AUTOAUTH env override is honored — cfg.DevAutoAuth reflects it. This is the smoke/e2e path.
func TestDevAutoAuthEnvAppliedInDevBuild(t *testing.T) {
	t.Setenv("TELOS_DEV_AUTOAUTH", "1")
	t.Setenv("TELOS_DEV_AUTOAUTH_ALLOW_REMOTE_BIND", "1")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DevAutoAuth || !cfg.DevAutoAuthAllowRemoteBind {
		t.Fatalf("dev-tagged build must apply TELOS_DEV_AUTOAUTH env; got DevAutoAuth=%v AllowRemoteBind=%v",
			cfg.DevAutoAuth, cfg.DevAutoAuthAllowRemoteBind)
	}
}
