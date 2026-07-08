//go:build telos_devauth

package config

import (
	"os"
	"strings"
)

// applyDevAuthEnv reads the TELOS_DEV_AUTOAUTH* overrides in a dev/test build (`-tags telos_devauth`). The
// bypass they enable — the gate's bare-name login in place of OAuth — is compiled in only under this tag
// (#96), so reading the env is meaningful only here. The release counterpart (devauth_release.go) leaves the
// fields false and warns, so a production config cannot turn the bypass on.
func applyDevAuthEnv(c *Config) {
	if v, ok := os.LookupEnv("TELOS_DEV_AUTOAUTH"); ok {
		c.DevAutoAuth = v == "1" || strings.EqualFold(v, "true")
	}
	if v, ok := os.LookupEnv("TELOS_DEV_AUTOAUTH_ALLOW_REMOTE_BIND"); ok {
		c.DevAutoAuthAllowRemoteBind = v == "1" || strings.EqualFold(v, "true")
	}
}
