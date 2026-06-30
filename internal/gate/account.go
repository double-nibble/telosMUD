package gate

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
)

// account.go — the gate's seam to telos-account (Phase 14, docs/ACCOUNT.md). The gate calls the account
// service to list an account's characters (the select menu) and — in later slices — redeem link codes,
// verify passphrases, and resolve SSH keys. A stub backs it when no account service is configured, so the
// pre-Phase-14 "type a name" login keeps working; a gRPC client backs it when cfg.AccountTarget is set.

// AccountClient is the gate-side account API. It grows per slice (14.2 RedeemLinkCode, 14.5 VerifyPassphrase,
// 14.6 ResolveSSHKey); 14.1 lands ListCharacters + the wiring.
type AccountClient interface {
	ListCharacters(ctx context.Context, accountID string) ([]CharacterInfo, error)
	// Close releases any underlying connection (a no-op for the stub).
	Close() error
}

// CharacterInfo is the gate-side summary of a character returned by the account service.
type CharacterInfo struct {
	ID      string
	Name    string
	ZoneRef string
	RoomRef string
}

// stubAccountClient is the no-service fallback. It returns a single character whose name is the (legacy)
// connection-chosen name carried as the accountID, preserving today's "By what name shall you be known?"
// login until link codes (14.2) replace it.
type stubAccountClient struct{}

func (stubAccountClient) ListCharacters(_ context.Context, accountID string) ([]CharacterInfo, error) {
	return []CharacterInfo{{ID: accountID, Name: accountID}}, nil
}

func (stubAccountClient) Close() error { return nil }

// grpcAccountClient wraps the generated Account gRPC client.
type grpcAccountClient struct {
	cc  *grpc.ClientConn
	cli accountv1.AccountClient
}

// DialAccount opens a gRPC client to telos-account at target. The in-cluster hop is insecure transport — the
// world's trust comes from the signed session assertion (Phase 14.3), not this link; a cluster mTLS posture
// is a deployment concern, not the gate's.
func DialAccount(target string) (AccountClient, error) {
	cc, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &grpcAccountClient{cc: cc, cli: accountv1.NewAccountClient(cc)}, nil
}

func (g *grpcAccountClient) ListCharacters(ctx context.Context, accountID string) ([]CharacterInfo, error) {
	resp, err := g.cli.ListCharacters(ctx, &accountv1.ListCharactersRequest{AccountId: accountID})
	if err != nil {
		return nil, err
	}
	out := make([]CharacterInfo, 0, len(resp.GetCharacters()))
	for _, c := range resp.GetCharacters() {
		out = append(out, CharacterInfo{
			ID: c.GetId(), Name: c.GetName(), ZoneRef: c.GetZoneRef(), RoomRef: c.GetRoomRef(),
		})
	}
	return out, nil
}

// Close releases the gRPC connection.
func (g *grpcAccountClient) Close() error { return g.cc.Close() }
