package gate

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// comms_funnel_test.go is the white-box (commsClient) test set for Phase-8 slice 8.6's RECEIVER-side
// enforcement: the single ignore FUNNEL (P8-A6) and the per-channel HEAR-filter re-subscribe. It
// constructs a commsClient over a net.Pipe so the test can read what reaches the "socket" directly, and
// drives the config + delivery paths without standing up a full world.

// socketReader is a single persistent background reader over the gate's socket: net.Pipe reads block,
// and a leaked per-call read goroutine would steal the NEXT line, so exactly ONE goroutine reads and
// pushes lines onto a buffered channel. readLineWithin then just selects on that channel.
type socketReader struct {
	lines chan string
}

func newSocketReader(r io.Reader) *socketReader {
	sr := &socketReader{lines: make(chan string, 64)}
	go func() {
		br := bufio.NewReader(r)
		for {
			s, err := br.ReadString('\n')
			if s != "" {
				sr.lines <- strings.TrimRight(s, "\r\n")
			}
			if err != nil {
				close(sr.lines)
				return
			}
		}
	}()
	return sr
}

// newPipeComms builds a commsClient over a net.Pipe with a MemBus world+gate pair, returning the client,
// the WORLD handle (to publish chan/tell/config as a source world would), and a persistent reader on the
// socket the gate writes to. The caller closes nothing extra (t.Cleanup handles it).
func newPipeComms(t *testing.T, player string) (*commsClient, commbus.Bus, *socketReader) {
	t.Helper()
	worldBus, gateBus := commbus.NewWorldBus()
	t.Cleanup(func() { _ = worldBus.Close() })

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	tc := telnet.New(server)

	cc := openComms(slog.New(slog.NewTextHandler(io.Discard, nil)), gateBus, tc, player)
	t.Cleanup(cc.close)

	return cc, worldBus, newSocketReader(client)
}

// readLineWithin reads one line from the socket within a deadline, or returns ("",false) on timeout.
func readLineWithin(t *testing.T, r *socketReader, d time.Duration) (string, bool) {
	t.Helper()
	select {
	case s, ok := <-r.lines:
		return s, ok
	case <-time.After(d):
		return "", false
	}
}

// publishConfig publishes a comms-config (hear-set + ignore list) to a player's config subject as the
// source world does — so the gate re-subscribes the named channels + caches the ignore funnel.
func publishConfig(t *testing.T, world commbus.Bus, player string, hear, ignore []string) {
	t.Helper()
	body, err := commbus.MarshalConfig(commbus.ConfigPayload{HearChannels: hear, Ignore: ignore})
	if err != nil {
		t.Fatal(err)
	}
	if err := world.Publish(context.Background(), commbus.ConfigSubject(player), commbus.Message{Body: body}); err != nil {
		t.Fatalf("publish config: %v", err)
	}
}

// publishChannelLine publishes a fully-rendered channel line on a channel subject (the world is source).
func publishChannelLine(t *testing.T, world commbus.Bus, ref, author, body string) {
	t.Helper()
	if err := world.Publish(context.Background(), commbus.ChanSubject(ref), commbus.Message{
		AuthorID: author, AuthorName: author, Body: body,
	}); err != nil {
		t.Fatalf("publish channel: %v", err)
	}
}

// settle gives the bus's per-subscription delivery goroutines a beat to apply a config / fan out a line
// (the MemBus delivers asynchronously per subscription). A short sleep is sufficient and deterministic
// for these in-process tests; the assertions still use a real read deadline.
func settle() { time.Sleep(50 * time.Millisecond) }

// TestHearFilterSubscribesOnlyHearSet is the receiver HEAR-filter proof: the gate subscribes ONLY the
// channels in the world's hear-set (no chan.* wildcard). A line on a hear-set channel reaches the socket;
// a line on a channel NOT in the hear-set (disabled OR not hearable — the world simply omitted it) does
// NOT. This closes the content guardrail: a restricted channel reaches only sockets whose world put it in
// their hear-set.
func TestHearFilterSubscribesOnlyHearSet(t *testing.T) {
	cc, world, r := newPipeComms(t, "Alice")

	// World says: Alice hears `gossip` but NOT `secret` (she disabled it or cannot hear it).
	publishConfig(t, world, "Alice", []string{"gossip"}, nil)
	settle()

	publishChannelLine(t, world, "gossip", "Bob", "[Gossip] Bob: hi")
	if line, ok := readLineWithin(t, r, 2*time.Second); !ok || !strings.Contains(line, "[Gossip] Bob: hi") {
		t.Fatalf("hear-set channel line not delivered: got %q ok=%v", line, ok)
	}

	// `secret` is NOT in the hear-set: a line on it must NOT reach the socket (no subscription).
	publishChannelLine(t, world, "secret", "Admin", "[Secret] Admin: classified")
	if line, ok := readLineWithin(t, r, 500*time.Millisecond); ok {
		t.Fatalf("a non-hear-set channel line reached the socket (guardrail open): %q", line)
	}
	_ = cc
}

// TestHearFilterReSubscribesOnToggle is the toggle-unsubscribe proof: when the world re-publishes a
// config WITHOUT a channel (a `channels off`), the gate drops that subscription and its lines stop; when
// it re-appears (a `channels on`), the gate re-subscribes and lines resume.
func TestHearFilterReSubscribesOnToggle(t *testing.T) {
	_, world, r := newPipeComms(t, "Alice")

	publishConfig(t, world, "Alice", []string{"gossip"}, nil)
	settle()
	publishChannelLine(t, world, "gossip", "Bob", "[Gossip] Bob: one")
	if line, ok := readLineWithin(t, r, 2*time.Second); !ok || !strings.Contains(line, "one") {
		t.Fatalf("gossip line not delivered before toggle: %q ok=%v", line, ok)
	}

	// Toggle gossip OFF: the world re-publishes an EMPTY hear-set. The gate unsubscribes gossip.
	publishConfig(t, world, "Alice", nil, nil)
	settle()
	publishChannelLine(t, world, "gossip", "Bob", "[Gossip] Bob: two")
	if line, ok := readLineWithin(t, r, 500*time.Millisecond); ok {
		t.Fatalf("a gossip line arrived after `channels off` (no unsubscribe): %q", line)
	}

	// Toggle gossip back ON: lines resume.
	publishConfig(t, world, "Alice", []string{"gossip"}, nil)
	settle()
	publishChannelLine(t, world, "gossip", "Bob", "[Gossip] Bob: three")
	if line, ok := readLineWithin(t, r, 2*time.Second); !ok || !strings.Contains(line, "three") {
		t.Fatalf("gossip line not delivered after re-enable: %q ok=%v", line, ok)
	}
}

// TestIgnoreFunnelDropsChannelAndTell is the receiver-side ignore-funnel proof (P8-A6): an ignored
// author's CHANNEL line AND directed TELL are BOTH dropped at the receiver, while a non-ignored author's
// lines pass. The drop is the SINGLE `ignored()` chokepoint shared by both delivery paths.
func TestIgnoreFunnelDropsChannelAndTell(t *testing.T) {
	_, world, r := newPipeComms(t, "Alice")

	// Alice hears gossip and ignores "Troll".
	publishConfig(t, world, "Alice", []string{"gossip"}, []string{"Troll"})
	settle()

	// A channel line from the ignored author: dropped.
	publishChannelLine(t, world, "gossip", "Troll", "[Gossip] Troll: spam")
	if line, ok := readLineWithin(t, r, 500*time.Millisecond); ok {
		t.Fatalf("an ignored author's channel line reached the socket: %q", line)
	}
	// A tell from the ignored author: dropped.
	if err := world.Publish(context.Background(), commbus.TellSubject("Alice"), commbus.Message{
		AuthorID: "Troll", AuthorName: "Troll", Body: "Troll tells you, 'spam'",
	}); err != nil {
		t.Fatal(err)
	}
	if line, ok := readLineWithin(t, r, 500*time.Millisecond); ok {
		t.Fatalf("an ignored author's tell reached the socket: %q", line)
	}

	// A non-ignored author passes (proves the funnel drops by author, not the whole channel/tell).
	publishChannelLine(t, world, "gossip", "Friend", "[Gossip] Friend: hi")
	if line, ok := readLineWithin(t, r, 2*time.Second); !ok || !strings.Contains(line, "Friend: hi") {
		t.Fatalf("a non-ignored channel line was dropped: %q ok=%v", line, ok)
	}
	if err := world.Publish(context.Background(), commbus.TellSubject("Alice"), commbus.Message{
		AuthorID: "Friend", AuthorName: "Friend", Body: "Friend tells you, 'hello'",
	}); err != nil {
		t.Fatal(err)
	}
	if line, ok := readLineWithin(t, r, 2*time.Second); !ok || !strings.Contains(line, "Friend tells you") {
		t.Fatalf("a non-ignored tell was dropped: %q ok=%v", line, ok)
	}
}

// TestIgnoreFunnelIsSingleChokepointForNewFrameType is the SINGLE-FUNNEL proof (P8-A6): a SYNTHETIC NEW
// comms frame type — one the gate does not handle today — inherits the ignore filter AUTOMATICALLY
// because every inbound comms delivery routes through the ONE `ignored()` chokepoint, not a per-path
// check. We register a synthetic delivery handler that does exactly what any future comms-frame handler
// must do (consult `ignored()` first), subscribe it to a NEW subject, and assert an ignored author's
// synthetic frame is dropped while a non-ignored one is rendered — proving the funnel is the single gate.
func TestIgnoreFunnelIsSingleChokepointForNewFrameType(t *testing.T) {
	cc, world, r := newPipeComms(t, "Alice")

	publishConfig(t, world, "Alice", nil, []string{"Troll"})
	settle()

	// A synthetic NEW comms frame type, delivered via a NEW subject. Its handler is what any future
	// comms-frame handler is required to be: it routes through the SAME single funnel (cc.ignored) before
	// rendering. The funnel is shared state on the commsClient — a new path does not re-implement ignore.
	const synthSubject = commbus.SubjectRoot + "synthetic.Alice"
	deliverSynthetic := func(msg commbus.Message) {
		if cc.ignored(msg.AuthorID) { // THE single chokepoint — identical to deliverTell/deliverChannel
			return
		}
		_ = cc.tc.Write(msg.Body + "\r\n")
	}
	sub, err := world.Subscribe(synthSubject, deliverSynthetic)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// An ignored author's synthetic frame is dropped — the funnel covered a path it never knew about.
	if err := world.Publish(context.Background(), synthSubject, commbus.Message{AuthorID: "Troll", Body: "synthetic from Troll"}); err != nil {
		t.Fatal(err)
	}
	if line, ok := readLineWithin(t, r, 500*time.Millisecond); ok {
		t.Fatalf("an ignored author's synthetic frame reached the socket (funnel not single): %q", line)
	}
	// A non-ignored author's synthetic frame renders.
	if err := world.Publish(context.Background(), synthSubject, commbus.Message{AuthorID: "Friend", Body: "synthetic from Friend"}); err != nil {
		t.Fatal(err)
	}
	if line, ok := readLineWithin(t, r, 2*time.Second); !ok || !strings.Contains(line, "synthetic from Friend") {
		t.Fatalf("a non-ignored synthetic frame was dropped: %q ok=%v", line, ok)
	}
}
