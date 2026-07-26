package contentbus

import "github.com/nats-io/nats.go"

// dial.go carries the NATS dial-time configuration for the content-invalidation bus. Its only concern
// today is the per-identity broker credentials (#552): worlds subscribe to content.invalidate and the
// operator/CI seed tool publishes it, and each supplies its OWN identity's credentials so the broker
// can enforce a per-role matrix. It mirrors commbus's dial.go — a sibling bus with its own connection,
// so credentials are threaded here independently rather than shared across the package boundary.

// DialOption tunes how contentbus dials NATS. Zero options preserves the historical anonymous connect,
// so every existing caller and test is unaffected — credentials are strictly additive (#552).
type DialOption func(*dialConfig)

type dialConfig struct {
	user     string
	password string
}

// WithUserPassword supplies NATS user/password credentials (nats.UserInfo). BOTH empty is anonymous —
// no credential option is added, so a missing credential degrades to today's no-auth behavior rather
// than erroring (the never-fatal rule, #552). See commbus.WithUserPassword for the full rationale.
func WithUserPassword(user, password string) DialOption {
	return func(c *dialConfig) {
		c.user = user
		c.password = password
	}
}

// buildDialConfig folds the options into a dialConfig (unit-testable without a live broker).
func buildDialConfig(opts []DialOption) dialConfig {
	var c dialConfig
	for _, o := range opts {
		o(&c)
	}
	return c
}

// natsOptions returns the nats.Option list this dial config contributes, or nil when anonymous.
func (c dialConfig) natsOptions() []nats.Option {
	if c.user == "" && c.password == "" {
		return nil
	}
	return []nats.Option{nats.UserInfo(c.user, c.password)}
}
