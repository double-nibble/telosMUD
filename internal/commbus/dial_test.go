package commbus

import (
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// applyOptions folds the dial config's nats.Options into a nats.Options struct so a test can inspect
// what the credentials actually set on the connection — the same funcs nats.Connect applies internally.
func applyOptions(t *testing.T, dc dialConfig) nats.Options {
	t.Helper()
	var o nats.Options
	for _, opt := range dc.natsOptions() {
		require.NoError(t, opt(&o))
	}
	return o
}

// TestDialConfigCredentials is the #552 slice-1 builder test: WithUserPassword sets nats.UserInfo when
// EITHER field is non-empty, and contributes NO option (anonymous — today's no-auth default) only when
// BOTH are empty. This is the single credential chokepoint every dial site (connect, dialJetStream)
// shares, so it covers "each site passes credentials when set and omits them when empty."
//
// Mutation-resistance: the empty-case row asserts len==0, so a builder that unconditionally emitted a
// UserInfo option would fail it; the populated rows assert the exact User/Password reach the option, so
// a builder that dropped or swapped them fails those.
func TestDialConfigCredentials(t *testing.T) {
	tests := []struct {
		name         string
		opts         []DialOption
		wantOptCount int
		wantUser     string
		wantPassword string
	}{
		{"no options is anonymous", nil, 0, "", ""},
		{"empty user+password is anonymous", []DialOption{WithUserPassword("", "")}, 0, "", ""},
		{"user+password sets UserInfo", []DialOption{WithUserPassword("world", "s3cret")}, 1, "world", "s3cret"},
		{"user only still authenticates", []DialOption{WithUserPassword("gate", "")}, 1, "gate", ""},
		{"password only still authenticates", []DialOption{WithUserPassword("", "tokenish")}, 1, "", "tokenish"},
		{"last option wins", []DialOption{WithUserPassword("first", "a"), WithUserPassword("second", "b")}, 1, "second", "b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dc := buildDialConfig(tc.opts)
			assert.Len(t, dc.natsOptions(), tc.wantOptCount)
			o := applyOptions(t, dc)
			assert.Equal(t, tc.wantUser, o.User)
			assert.Equal(t, tc.wantPassword, o.Password)
		})
	}
}
