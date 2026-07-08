//go:build !telos_devauth

package config

import (
	"log/slog"
	"os"
)

// applyDevAuthEnv is the release-build counterpart (#96): the TELOS_DEV_AUTOAUTH bypass is not compiled into a
// default build, so it is inert here. It force-pins BOTH dev-autoauth fields to false so the invariant "the
// bypass is off in release" rests on the config VALUE, not just the one gate call site — a config file with
// `dev_auto_auth: true` (unmarshaled before this runs) or the env var cannot leave the field set true for a
// future reader to trip over (security review, #96). Any attempt (env or YAML, either field) gets a loud
// warning that it is ignored. The dev-tagged counterpart (devauth_dev.go) applies the overrides for real.
func applyDevAuthEnv(c *Config) {
	_, envSet := os.LookupEnv("TELOS_DEV_AUTOAUTH")
	_, remoteEnvSet := os.LookupEnv("TELOS_DEV_AUTOAUTH_ALLOW_REMOTE_BIND")
	if envSet || remoteEnvSet || c.DevAutoAuth || c.DevAutoAuthAllowRemoteBind {
		slog.Warn("TELOS_DEV_AUTOAUTH / dev_auto_auth set but this is a RELEASE build (compiled without " +
			"-tags telos_devauth) — IGNORED; the no-OAuth bypass is absent and OAuth stays enforced (#96).")
	}
	c.DevAutoAuth = false
	c.DevAutoAuthAllowRemoteBind = false
}
