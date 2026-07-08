package callerauth

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// okHandler is a sentinel grpc.UnaryHandler: it returns errHandlerRan so a test can tell "the interceptor
// ADMITTED the call (the handler ran)" from "the interceptor REJECTED it (Unauthenticated, handler never ran)".
var errHandlerRan = errors.New("handler ran")

func okHandler(context.Context, any) (any, error) { return nil, errHandlerRan }

// mdCtx builds an incoming-metadata context carrying the given key/value pairs (nil => no metadata at all).
func mdCtx(pairs ...string) context.Context {
	if len(pairs) == 0 {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
}

// TestInterceptorRequiresToken pins the server-side gate: a matching token admits the call (handler runs); a
// missing token, a wrong token, and absent metadata are all Unauthenticated (handler never runs).
func TestInterceptorRequiresToken(t *testing.T) {
	const token = "s3cr3t-caller-token"
	intercept := Interceptor(token)
	info := &grpc.UnaryServerInfo{FullMethod: "/telos.account.v1.Account/SetAccountTier"}

	run := func(ctx context.Context) error {
		_, err := intercept(ctx, nil, info, okHandler)
		return err
	}

	// Rejected: no metadata, no token, wrong token, empty-value token.
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"no metadata", mdCtx()},
		{"metadata without the token key", mdCtx("other", "x")},
		{"wrong token", mdCtx(callerTokenMetadataKey, "nope")},
		{"empty token value", mdCtx(callerTokenMetadataKey, "")},
	} {
		t.Run("rejected/"+tc.name, func(t *testing.T) {
			if got := status.Code(run(tc.ctx)); got != codes.Unauthenticated {
				t.Fatalf("want Unauthenticated, got %v", got)
			}
		})
	}

	// Admitted: the correct token lets the handler run (our sentinel error proves it did).
	t.Run("correct token admits", func(t *testing.T) {
		if err := run(mdCtx(callerTokenMetadataKey, token)); !errors.Is(err, errHandlerRan) {
			t.Fatalf("correct token must admit the call (handler runs), got %v", err)
		}
	})

	// An extra appended value cannot smuggle past: only the FIRST value is compared.
	t.Run("smuggled second value is ignored", func(t *testing.T) {
		if got := status.Code(run(mdCtx(callerTokenMetadataKey, "nope", callerTokenMetadataKey, token))); got != codes.Unauthenticated {
			t.Fatalf("only the first metadata value counts, got %v", got)
		}
	})
}

// TestInterceptorEmptyTokenDisablesCheck: an empty CONFIGURED token disables the gate (dev / no-auth) — every
// call is admitted regardless of metadata. main is responsible for refusing to serve tokenless in production.
func TestInterceptorEmptyTokenDisablesCheck(t *testing.T) {
	intercept := Interceptor("")
	info := &grpc.UnaryServerInfo{FullMethod: "/telos.account.v1.Account/IssueSessionAssertion"}
	if _, err := intercept(mdCtx(), nil, info, okHandler); !errors.Is(err, errHandlerRan) {
		t.Fatalf("an empty configured token must admit every call, got %v", err)
	}
}

// TestCredentialsMatchInterceptor proves the CLIENT credentials produce metadata the SERVER interceptor
// accepts end-to-end: feed the creds' output straight into the interceptor's incoming context.
func TestCredentialsMatchInterceptor(t *testing.T) {
	const token = "matched-token"
	md, err := Credentials{Token: token}.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the server's incoming metadata from the client's outgoing pairs.
	pairs := make([]string, 0, 2*len(md))
	for k, v := range md {
		pairs = append(pairs, k, v)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
	if _, err := Interceptor(token)(ctx, nil, &grpc.UnaryServerInfo{}, okHandler); !errors.Is(err, errHandlerRan) {
		t.Fatalf("client credentials must satisfy the server interceptor, got %v", err)
	}
}

// TestCredentialsMetadata pins the per-RPC creds surface: a set token yields the metadata pair; an empty token
// yields nothing (dev / no-auth); and transport security is not required (insecure cluster hop).
func TestCredentialsMetadata(t *testing.T) {
	md, err := Credentials{Token: "abc"}.GetRequestMetadata(context.Background())
	if err != nil || md[callerTokenMetadataKey] != "abc" {
		t.Fatalf("set token must yield the metadata pair, got md=%v err=%v", md, err)
	}
	if md, _ := (Credentials{}).GetRequestMetadata(context.Background()); md != nil {
		t.Fatalf("empty token must yield no metadata, got %v", md)
	}
	if (Credentials{Token: "x"}).RequireTransportSecurity() {
		t.Fatal("per-RPC creds must not require transport security (insecure cluster hop)")
	}
}
