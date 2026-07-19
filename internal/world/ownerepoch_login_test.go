package world

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// ownerepoch_login_test.go pins the LOGIN half of the #432 fence: the claim in server.go.
//
// The claim is the point at which "a login is an ownership assertion" becomes true. Before it, the
// epoch was RESUMED at its stored value, so a second login on a different shard came up holding the
// same epoch as the copy it was displacing and the two force-wrote over each other. Two failure modes
// pull in opposite directions and both have to be pinned, because a fence that wedges legitimate
// players gets turned off:
//
//   - the STORE cannot mint -> refuse the login. A session that could not claim cannot be proven to
//     own the character, and the same outage will refuse its saves. Admitting it means hours of play
//     that is silently unpersistable.
//   - the DIRECTORY cannot be read -> admit the login anyway. The directory is evictable Redis
//     (#340) and contributes only a FLOOR; the authoritative high-water mark is the row itself. An
//     implementation that sourced the epoch from the directory alone would hand a live character an
//     epoch below the one its last owner held, and every save it made would be refused for the whole
//     session's lifetime — a wedge caused by a cache hiccup.

// epochLoginWorld serves a shard over bufconn with the given store + directory, and returns a client.
func epochLoginWorld(t *testing.T, store CharacterStore, ckpt Checkpointer, dir Locator) (playv1.PlayClient, *Shard) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	shard := NewShard("midgaard", "addr-a", dir, nil).WithPersistence(store, ckpt)
	shard.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	zctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go shard.Run(zctx)

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cc.Close() })
	return playv1.NewPlayClient(cc), shard
}

// TestLoginClaimFailureRefusesRatherThanAdmittingAnUnsaveableSession is the fail-closed rule, plus
// its companion: the directory must never be able to wedge a legitimate player.
func TestLoginClaimFailureRefusesRatherThanAdmittingAnUnsaveableSession(t *testing.T) {
	t.Run("a store that cannot mint refuses the login with Unavailable", func(t *testing.T) {
		mem := NewMemStore()
		ctx := context.Background()
		// A durable row must exist, or there is nothing to fence and the claim is correctly skipped.
		_, err := mem.CreateCharacter(ctx, "Refused", "midgaard", "midgaard:room:temple")
		require.NoError(t, err)

		client, _ := epochLoginWorld(t, claimFailingStore{MemStore: mem}, mem, epochStubLocator{})
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		stream, err := client.Connect(cctx)
		require.NoError(t, err)
		send(t, stream, attach("Refused"))

		_, err = stream.Recv()
		require.Error(t, err,
			"a login whose ownership claim failed was ADMITTED. It cannot be proven to own the character and "+
				"the same outage will refuse its saves, so it would play for hours unpersistably (#432)")
		require.Equal(t, codes.Unavailable, status.Code(err),
			"the refusal must be Unavailable — the code the gate already retries on — not an opaque failure")
	})

	t.Run("a broken directory still admits the player, and their first flush LANDS", func(t *testing.T) {
		mem := NewMemStore()
		ctx := context.Background()
		pid, err := mem.CreateCharacter(ctx, "Wedged", "midgaard", "midgaard:room:temple")
		require.NoError(t, err)
		// Drive the ROW's epoch well above anything a directory read could contribute. This is the case
		// that discriminates: an implementation that took its epoch from the directory alone would come
		// up at 1 here (the read errored -> resume 0 -> seed 1) and be outranked by its own row.
		rowEpoch, err := mem.ClaimCharacter(ctx, pid, 50)
		require.NoError(t, err)
		require.EqualValues(t, 51, rowEpoch)

		client, shard := epochLoginWorld(t, mem, mem, epochBrokenDirectory{})
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		stream, err := client.Connect(cctx)
		require.NoError(t, err)
		send(t, stream, attach("Wedged"))
		recvAttached(t, stream)

		// The flush must LAND: the session's epoch has to outrank the row it loaded, which can only come
		// from a claim minted off the row. Wait on EITHER outcome — the write landing, or the session
		// being evicted as not-owner — so a regression fails on the wedge assertion below rather than
		// timing out with a misleading message.
		z := shard.Zone()
		z.post(drainFlushMsg{})
		waitCond(t, "the login's first flush to resolve (landing, or the session being evicted)", func() bool {
			snap, ok, _ := mem.LoadCharacter(ctx, "Wedged")
			return (ok && snap.StateVersion > 0) || !zoneHasPlayer(z, "Wedged")
		})

		require.True(t, zoneHasPlayer(z, "Wedged"),
			"the player was EVICTED on their first flush after a directory read failure (#432). Their save lost "+
				"the ownership fence to THEIR OWN ROW — the wedge an epoch sourced from the directory alone "+
				"produces: the directory is evictable Redis, so a cache miss must never outrank the row's "+
				"authoritative high-water mark")

		snap, ok, err := mem.LoadCharacter(ctx, "Wedged")
		require.NoError(t, err)
		require.True(t, ok)
		require.Positive(t, snap.StateVersion, "the login's first flush never landed")
		require.Greater(t, snap.OwnerEpoch, rowEpoch,
			"the session must hold an epoch minted ABOVE the row it loaded (row was %d)", rowEpoch)
	})
}
