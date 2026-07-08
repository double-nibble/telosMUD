// Package callerauth provides shared caller authentication for the account gRPC API (#247) — a server
// interceptor and matching client per-RPC credentials in one small package, so both telos-account (server) and
// the gate (client) agree on the metadata key without the edge importing the whole account service.
//
// The account service's listener would otherwise accept an UNAUTHENTICATED caller: SetAccountTier takes the
// acting principal from a caller-asserted field, and IssueSessionAssertion will sign a valid session assertion
// for ANY account id — so anyone who can dial the port could self-promote or mint an admin assertion. The fix
// is a shared caller TOKEN: the gate (the only intended caller, and already the system's identity broker)
// sends it on every RPC; the service requires it. This does not widen the trust model — the world already
// trusts the gate's asserted identity on the Play stream with no secret — it just stops UNtrusted principals
// from reaching the listener. A stronger transport (mTLS) composes with this later; the token is the complete
// fix on a trusted cluster network.
package callerauth

import (
	"context"
	"crypto/subtle"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// callerTokenMetadataKey is the gRPC metadata key (not a secret) carrying the shared caller token. Lowercase —
// gRPC normalizes metadata keys to lowercase, and a non-lowercase key panics at send time.
const callerTokenMetadataKey = "x-telos-caller-token" //nolint:gosec // G101: a metadata KEY name, not a credential

// Interceptor returns a unary server interceptor that requires every RPC to carry the shared caller
// token (#247). An empty configured token DISABLES the check (dev / no-auth) — the caller (main) is
// responsible for refusing to serve with an empty token outside dev, so this stays a pure predicate. The
// compare is constant-time so a token guess can't be timed. Applied to the WHOLE surface, not just
// SetAccountTier, because IssueSessionAssertion is an equally-privileged signing oracle.
func Interceptor(token string) grpc.UnaryServerInterceptor {
	want := []byte(token)
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if len(want) == 0 {
			return handler(ctx, req) // check disabled (dev); main gates this off in production
		}
		if !callerTokenPresent(ctx, want) {
			return nil, status.Error(codes.Unauthenticated, "caller token required")
		}
		return handler(ctx, req)
	}
}

// callerTokenPresent reports whether the incoming context carries a metadata token equal to want (constant
// time). Missing metadata, a missing key, or an empty/mismatched value all fail.
func callerTokenPresent(ctx context.Context, want []byte) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	vals := md.Get(callerTokenMetadataKey)
	if len(vals) == 0 {
		return false
	}
	// Compare the FIRST value only; a caller cannot smuggle extra values past the check by appending.
	return subtle.ConstantTimeCompare([]byte(vals[0]), want) == 1
}

// Credentials is the client-side PerRPCCredentials that attaches the shared caller token to every
// outgoing account RPC (#247). RequireTransportSecurity returns FALSE deliberately: the gate↔account hop is
// insecure transport on a trusted cluster network (mTLS is a later, composable layer), and gRPC refuses to
// send per-RPC credentials over an insecure connection unless this opts in.
type Credentials struct{ Token string }

// GetRequestMetadata returns the caller-token metadata for an outgoing RPC (empty when no token is set, so a
// dev gate with no token simply sends nothing and a dev service with no token does not check).
func (c Credentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	if c.Token == "" {
		return nil, nil
	}
	return map[string]string{callerTokenMetadataKey: c.Token}, nil
}

// RequireTransportSecurity reports false — see the type doc (insecure cluster hop; token is the control).
func (Credentials) RequireTransportSecurity() bool { return false }

// static assertion that the creds satisfy the gRPC interface.
var _ credentials.PerRPCCredentials = Credentials{}
