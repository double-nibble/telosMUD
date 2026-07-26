package contentbus

import (
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDialConfigCredentials mirrors commbus's builder test (#552 slice 1): WithUserPassword sets
// nats.UserInfo when either field is non-empty, and contributes no option when both are empty
// (anonymous — the no-auth default). contentbus dials its own connection (worlds subscribe, seed
// publishes content.invalidate), so it carries the credential logic independently of commbus.
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
		{"user+password sets UserInfo", []DialOption{WithUserPassword("seed", "s3cret")}, 1, "seed", "s3cret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dc := buildDialConfig(tc.opts)
			assert.Len(t, dc.natsOptions(), tc.wantOptCount)
			var o nats.Options
			for _, opt := range dc.natsOptions() {
				require.NoError(t, opt(&o))
			}
			assert.Equal(t, tc.wantUser, o.User)
			assert.Equal(t, tc.wantPassword, o.Password)
		})
	}
}
