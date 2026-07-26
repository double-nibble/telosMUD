package commbus

import "github.com/nats-io/nats.go"

// dial.go carries the NATS dial-time configuration shared by every commbus connection site — the
// transient bus (nats.go's connect) and BOTH durable JetStream streams (jetstream_nats.go's
// dialJetStream). Its only concern today is the per-identity broker credentials (#552); keeping it in
// one place means a credential (or any future dial-time concern) is threaded once and applied
// identically across the transient and durable transports, which a single process opens as separate
// connections under the SAME identity.

// DialOption tunes how commbus dials NATS. Zero options preserves the historical anonymous connect, so
// every existing caller and test is unaffected — credentials are strictly additive (#552).
type DialOption func(*dialConfig)

type dialConfig struct {
	user     string
	password string
}

// WithUserPassword supplies NATS user/password credentials (nats.UserInfo) for the dial. BOTH empty is
// treated as anonymous — NO credential option is added, so a missing credential degrades to today's
// no-auth behavior rather than erroring. This is the never-fatal rule (#552): the realistic failure is
// an operator credential typo, and against an auth-enabled broker a WRONG credential surfaces as an
// ASYNC authorization violation on the live/disabled bus, never a boot failure. A credentialed client
// against today's no-auth server simply connects, which is why threading credentials ahead of the
// broker-side matrix (slices 2-3) is inert and independently deployable.
func WithUserPassword(user, password string) DialOption {
	return func(c *dialConfig) {
		c.user = user
		c.password = password
	}
}

// buildDialConfig folds the options into a dialConfig. Kept separate from the nats.Option translation
// so the credential decision is unit-testable without a live broker.
func buildDialConfig(opts []DialOption) dialConfig {
	var c dialConfig
	for _, o := range opts {
		o(&c)
	}
	return c
}

// natsOptions returns the nats.Option list this dial config contributes, or nil when anonymous (both
// credential fields empty). Appended to each dial site's base options so credentials thread identically
// through the transient and durable transports.
func (c dialConfig) natsOptions() []nats.Option {
	if c.user == "" && c.password == "" {
		return nil
	}
	return []nats.Option{nats.UserInfo(c.user, c.password)}
}
