// Package gate is the edge service: it accepts telnet connections, runs a minimal
// login, and proxies each player to a world shard over the gRPC Play stream.
// TLS/SSH, GMCP, and real auth arrive in later phases (docs/ACCOUNT.md, GMCP.md).
//
// # The edge invariant
//
// The gate holds the socket; the world holds the player (docs/ARCHITECTURE.md §2).
// The gate is stateless beyond its live sockets: it owns no player state, only the
// TCP connection, a session-scoped input buffer, and the gRPC stream bridging it to
// a shard. Rendering happens here at the edge — the world emits semantic
// ServerFrames and the gate turns them into bytes for this particular terminal
// (engine = mechanism, content = flavor).
//
// # Connection lifecycle
//
//  1. Accept a telnet connection (ListenAndServe -> handle).
//  2. Prompt for a name and read one line (the minimal stand-in for real auth).
//  3. Mint the session ONCE: a stable session_id and a session-scoped input seq
//     that survive re-dials (docs/PROTOCOL.md §5: the gate owns the input buffer).
//  4. Ask the directory which shard hosts that character; dial it via the pool.
//  5. Open a gRPC Play stream and send Attach as the first frame (Attach first;
//     see docs/PROTOCOL.md §1). Then run the bridge until the stream ends.
//  6. A bridge ending in a Redirect re-dials the target shard, re-Attaches with the
//     handoff token + resume seq, replays un-acked input, and resumes — the telnet
//     socket stays open the whole time; only the gRPC stream re-targets.
//
// # The cross-shard redirect (Phase 2 step 5)
//
// On a ServerFrame.Redirect the gate stops live forwarding, dials the target via the
// per-address client pool, opens a fresh Play stream, sends Attach{handoff_token,
// session_id, input_seq}, and replays every buffered input the new shard has not yet
// acked, in order, before resuming live forwarding. Lines typed DURING the redirect
// are buffered and queue AFTER the replay, never reaching the new shard out of order.
// The world dedups by seq (drops seq <= its high-water), so a replayed line that the
// destination already applied is harmless — exactly-once across the move.
//
// # The reader / writer model
//
// The reader loop (telnet -> world) lives at CONNECTION scope and persists across
// re-dials: it owns the session, assigns each line its seq, buffers it, and forwards
// it to whichever stream is currently live (swapped atomically on re-dial). Each
// stream has its own writer goroutine (world -> telnet) that renders frames, prunes
// the replay buffer on ack, and detects Redirect/Disconnect. Closing the telnet
// socket is the cross-signal that unblocks both ends together.
//
// # Debug logging
//
// Every control-flow point logs at slog.Debug (off unless DEBUG=1; see internal/obs)
// through a per-server scoped logger tagged component=gate, so DEBUG=1 narrates the
// whole edge: connection accepted, login name, directory lookup, stream opened,
// Attach sent, each input line (seq), each rendered frame, redirect received, re-dial
// target, replay count, buffer prune, and teardown.
package gate

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// maxNameLen caps the login name length. The name becomes the in-world entity
// name and a targeting keyword, so it must be short enough to render and type. 20
// runes is generous for a stand-in login (real auth/chargen lands in Phase 13).
const maxNameLen = 20

// Server accepts telnet connections and bridges them to world shards. It is
// stateless beyond its live sockets: a listen address, the directory seam (initial
// shard resolution), and a per-address Play client pool for dialing shards.
type Server struct {
	listen string
	dir    directory.Directory
	pool   *pool
	log    *slog.Logger // scoped logger, tagged component=gate
}

// New builds a gate over a real (insecure) client pool. dir resolves the initial
// shard for a login; the pool caches one gRPC conn per shard address, dialed on
// demand as players walk to new shards.
func New(listen string, dir directory.Directory) *Server {
	return newServer(listen, dir, newPool())
}

// newServer is the injectable constructor: tests pass a pool wired to in-process
// bufconn shards.
func newServer(listen string, dir directory.Directory, p *pool) *Server {
	return &Server{
		listen: listen,
		dir:    dir,
		pool:   p,
		log:    slog.With("component", "gate"),
	}
}

// ListenAndServe accepts connections until ctx is cancelled. Each accepted
// connection is handed to its own goroutine (handle); cancelling ctx closes the
// listener, which makes Accept fail and returns cleanly.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return err
	}
	slog.Info("gate listening", "addr", s.listen)
	// Closing the listener on ctx cancellation unblocks Accept below.
	go func() { <-ctx.Done(); _ = ln.Close() }()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Distinguish a clean shutdown (ctx cancelled -> listener closed) from
			// a real accept error.
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go s.handle(ctx, conn)
	}
}

// handle serves one connection end to end: login, mint the session, resolve the
// initial shard, then run the bridge — re-dialing on each Redirect — until the
// socket drops or the world disconnects. The deferred Close is the teardown
// backstop: when handle returns, the socket closes, which unblocks any in-flight
// writer goroutine's Recv.
func (s *Server) handle(ctx context.Context, nc net.Conn) {
	defer func() { _ = nc.Close() }()
	remote := nc.RemoteAddr().String()
	log := s.log.With("remote", remote)
	log.Debug("connection accepted")

	tc := telnet.New(nc)

	// --- minimal login: read a name (stand-in for real auth) ---
	// Loop until we get a name that is safe to render and safe to use as a
	// targeting keyword; an unsafe name re-prompts rather than dropping the
	// connection. ReadLine already strips control chars and caps length, but the
	// keyword-grammar rules (no leading '.'/digit, no embedded '.') are gate
	// policy and enforced here.
	_ = tc.Write("\r\nWelcome to TelosMUD.\r\n")
	var name string
	for {
		_ = tc.Write("By what name shall you be known? ")
		line, err := tc.ReadLine()
		if err != nil {
			log.Debug("connection closed before login", "err", err)
			return
		}
		candidate := strings.TrimSpace(line)
		if reason, ok := validateName(candidate); !ok {
			log.Debug("login name rejected", "reason", reason)
			_ = tc.Write("\r\nThat name won't do: " + reason + "\r\n")
			continue
		}
		name = candidate
		break
	}
	log.Debug("login name received", "character", name)

	// --- mint the session ONCE: stable across every re-dial (docs/PROTOCOL.md §5).
	sess := newSession(uuid.NewString())
	log = log.With("session", sess.id, "character", name)
	log.Debug("session minted")

	// --- directory seam: resolve the initial shard for this character ---
	addr, ok := s.dir.ShardForCharacter(name)
	if !ok {
		log.Debug("no shard available for character")
		_ = tc.Write("\r\nNo world is available right now. Goodbye.\r\n")
		return
	}
	log.Debug("initial shard resolved", "addr", addr)

	// One connection-scoped line pump reads telnet lines into a channel for the whole
	// connection (across every re-dial). Decoupling the blocking socket read from the
	// per-stream forwarding loop is what lets a redirect interrupt forwarding WITHOUT
	// waiting on the next keystroke: the forwarding loop selects between a new line and
	// the stream ending. The pump closes `lines` on socket EOF, which tears everything
	// down.
	lines := make(chan string)
	go func() {
		defer close(lines)
		for {
			line, err := tc.ReadLine()
			if err != nil {
				log.Debug("line pump ended", "err", err)
				return
			}
			lines <- line
		}
	}()

	// Outer re-dial loop. Each iteration binds the session to one shard (initial
	// Attach has no token; a re-dial carries the redirect's handoff token) and runs the
	// bridge until that stream ends. A bridge that ends in a Redirect hands back the
	// next target; otherwise we tear the connection down. The resume point is NOT
	// threaded from the redirect: the destination shard is authoritative and reports how
	// far it has applied on its Attached frame (ack_input_seq), which drives replay (see
	// runStream / doReplay).
	conn := &connState{
		log:   log,
		tc:    tc,
		nc:    nc,
		sess:  sess,
		name:  name,
		lines: lines,
	}
	var token string
	for {
		next, ok := s.runStream(ctx, conn, addr, token)
		if !ok {
			return // bridge ended without a redirect: socket/world closed.
		}
		// The actual replay keys off the DESTINATION's ack_input_seq on its first (Attached)
		// frame (runStream / doReplay); the redirect carries no resume point.
		log.Debug("redirect received", "target", next.addr)
		addr, token = next.addr, next.token
	}
}

// connState bundles the per-connection state the bridge needs across re-dials: the
// scoped logger, the telnet conn (and raw socket for the cross-close), the session
// (input buffer + stable id), the character name carried into each Attach, and the
// connection-scoped line channel fed by the pump goroutine.
type connState struct {
	log   *slog.Logger
	tc    *telnet.Conn
	nc    net.Conn
	sess  *session
	name  string
	lines <-chan string
}

// redirectTarget is what a finished bridge reports when the world asked the gate to
// migrate: where to dial and the token to present. No resume point travels here — the
// replay keys off the DESTINATION's ack_input_seq on its first (Attached) frame (see
// runStream / doReplay).
type redirectTarget struct {
	addr  string
	token string
}

// runStream binds the session to the shard at addr and runs the bridge over a single
// Play stream. token is empty on the first attach and carries the redirect's handoff
// token on a re-dial.
//
// The resume point is deliberately NOT a parameter: on a re-dial the destination shard
// is authoritative about how much input it has applied and reports it on its first
// (Attached) frame as ServerFrame.ack_input_seq, which the writer feeds to doReplay. The
// redirect carries no resume point at all (the source-side estimate that once rode on
// Redirect.resume_input_seq was retired — it only ever fed a diagnostic log).
//
// It returns (target, true) when the stream ended in a Redirect — the caller re-dials
// target. It returns (_, false) when the bridge ended for any other reason (socket
// EOF, world disconnect, dial/attach failure): the caller tears the connection down.
func (s *Server) runStream(ctx context.Context, c *connState, addr, token string) (redirectTarget, bool) {
	log := c.log.With("addr", addr)

	cli, err := s.pool.client(addr)
	if err != nil {
		log.Debug("shard dial failed", "err", err)
		_ = c.tc.Write("\r\nThe world is unreachable. Goodbye.\r\n")
		return redirectTarget{}, false
	}
	stream, err := cli.Connect(ctx)
	if err != nil {
		log.Debug("play stream dial failed", "err", err)
		_ = c.tc.Write("\r\nThe world is unreachable. Goodbye.\r\n")
		return redirectTarget{}, false
	}
	log.Debug("play stream opened")

	// Attach MUST be the first frame (docs/PROTOCOL.md §1). On a re-dial it carries the
	// handoff token; input_seq is the next seq the gate will send (the resume point).
	attach := &playv1.Attach{
		SessionId:    c.sess.id,
		CharacterId:  c.name,
		HandoffToken: token,
		InputSeq:     c.sess.nextSeqValue(),
	}
	if err := stream.Send(&playv1.ClientFrame{Payload: &playv1.ClientFrame_Attach{Attach: attach}}); err != nil {
		log.Debug("attach send failed", "err", err)
		return redirectTarget{}, false
	}
	log.Debug("attach sent", "token_present", token != "", "input_seq", attach.InputSeq)

	// On a re-dial the session is frozen (live input is buffered, not forwarded). The
	// writer goroutine clears the freeze and replays the un-acked window once the new
	// shard's Attached reports its resume point.
	if token != "" {
		c.sess.freeze()
	}

	br := &bridge{
		log:    log,
		srv:    s,
		conn:   c,
		stream: stream,
		replay: token != "", // a re-dial must replay before resuming live forwarding
		done:   make(chan struct{}),
	}

	// Writer goroutine: world ServerFrames -> telnet, plus buffer prune, replay, and
	// redirect detection. It closes `done` when this stream ends (redirect flagged,
	// disconnect, or Recv error), which unblocks the forwarding loop's select.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(br.done)
		br.runWriter()
	}()

	// Forwarding loop: drain the connection-scoped line channel onto this stream until
	// the stream ends. It selects between a new line and `done`, so a redirect (or a
	// dropped stream) wakes it WITHOUT waiting on the next keystroke.
	br.runForwarder()

	// The forwarder returned: either the socket dropped (lines closed) or the writer
	// flagged a redirect/disconnect. Wait for the writer to finish so we read its result
	// without a race.
	wg.Wait()
	target, redirected := br.result()
	if !redirected {
		// A real teardown (a player quit / world Disconnect, or socket EOF), NOT a re-dial.
		// Half-close the world stream's send side so the shard's reader loop promptly sees
		// end-of-input and runs its leave/detach (and thus the logout DURABLE FLUSH) NOW,
		// rather than waiting for the per-connection context to be cancelled when runConn
		// unwinds — an asynchronous, racy teardown that left the logout flush deferred to the
		// 60s link-death reap (or lost on a crash in that window). The forwarder's own EOF path
		// already CloseSends on socket EOF; this covers the world-initiated Disconnect path,
		// where the forwarder returns via `done` without closing the send side. Idempotent: a
		// second CloseSend (the forwarder already closed it) is a harmless no-op error we drop.
		// A redirect must NOT take this path — the socket stays open for the re-dial.
		br.sendMu.Lock()
		_ = stream.CloseSend()
		br.sendMu.Unlock()
	}
	return target, redirected
}

// bridge runs the two halves of one Play stream for a connection. It shares the
// connState (telnet + session) with sibling bridges across re-dials but owns this
// stream's lifecycle. The writer's result (redirect target, if any) is read after
// both loops finish.
type bridge struct {
	log    *slog.Logger
	srv    *Server
	conn   *connState
	stream playv1.Play_ConnectClient

	replay bool          // this stream must replay the un-acked window before live input
	done   chan struct{} // closed by the writer when this stream ends

	sendMu sync.Mutex // serializes stream.Send across the writer (replay) and forwarder (live)

	mu       sync.Mutex
	redirect *redirectTarget // set by the writer when a Redirect frame arrives
}

// runWriter pumps world frames to the terminal. On EVERY frame it prunes the
// session's replay buffer up to ack_input_seq. On the first frame of a re-dial it
// replays the un-acked window from the new shard's resume point (clearing the freeze,
// so the reader resumes live forwarding). A Redirect frame records the target and
// closes the send side so the reader unblocks; the socket stays open for the re-dial.
// A Disconnect closes the socket. Any Recv error (with no pending redirect) closes the
// socket so the reader's ReadLine errors and the connection tears down.
func (b *bridge) runWriter() {
	first := true
	for {
		f, err := b.stream.Recv()
		if err != nil {
			b.log.Debug("play stream recv ended", "err", err)
			// If we already flagged a redirect, the stream close is expected and the
			// socket must stay open for the re-dial. Otherwise drop the socket so the
			// reader loop unblocks and the connection tears down.
			if !b.takenRedirect() {
				_ = b.conn.nc.Close()
			}
			return
		}

		ack := f.GetAckInputSeq()
		if n := b.conn.sess.prune(ack); n > 0 {
			b.log.Debug("replay buffer pruned", "ack", ack, "removed", n)
		}

		if first {
			first = false
			if b.replay {
				b.doReplay(ack)
			}
		}

		if r := f.GetRedirect(); r != nil {
			b.log.Debug("redirect frame received", "target", r.GetTargetShardAddr())
			b.setRedirect(redirectTarget{
				addr:  r.GetTargetShardAddr(),
				token: r.GetHandoffToken(),
			})
			// Stop forwarding to this shard: close our send side so the forwarder's next
			// Send errors out (it then returns to re-dial, the line safely buffered).
			// gRPC forbids CloseSend concurrent with SendMsg, and the forwarder may be
			// mid-Send under sendMu — so take the lock here too.
			b.sendMu.Lock()
			_ = b.stream.CloseSend()
			b.sendMu.Unlock()
			return
		}

		b.srv.renderFrame(b.log, b.conn.tc, f)

		if f.GetDisconnect() != nil {
			b.log.Debug("disconnect frame received, closing socket")
			_ = b.conn.nc.Close()
			return
		}
	}
}

// doReplay re-sends every buffered input with seq > ack (the new shard's resume point),
// in order, then thaws the freeze so the forwarder resumes live forwarding. Because a
// line may arrive while a replay batch is in flight (the session stays frozen, so the
// forwarder buffers it), it loops: send the snapshot, then drain any tail that slipped
// in, retrying until thawIfDrained confirms the buffer is fully sent. The whole replay
// holds sendMu, so no live forward can interleave a Send mid-batch (gRPC Send is not
// concurrent-safe).
func (b *bridge) doReplay(ack uint64) {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()

	lines := b.conn.sess.replayFrom(ack)
	b.log.Debug("replaying un-acked input", "from_ack", ack, "count", len(lines))
	var sentThrough uint64
	for {
		for _, in := range lines {
			if err := b.sendInputLocked(in.seq, in.text); err != nil {
				b.log.Debug("replay send failed", "seq", in.seq, "err", err)
				return // stream is going away; the writer will tear down or re-dial.
			}
			sentThrough = in.seq
			b.log.Debug("replayed input", "seq", in.seq)
		}
		if b.conn.sess.thawIfDrained(sentThrough) {
			return
		}
		// A line arrived during the batch (and the forwarder buffered it under freeze);
		// send it too, then re-check.
		lines = b.conn.sess.tailAfter(sentThrough)
	}
}

// sendInputLocked sends one InputLine. The caller must hold sendMu.
func (b *bridge) sendInputLocked(seq uint64, text string) error {
	return b.stream.Send(&playv1.ClientFrame{Payload: &playv1.ClientFrame_Input{Input: &playv1.InputLine{
		Seq:  seq,
		Text: text,
	}}})
}

// runForwarder drains the connection-scoped line channel onto this stream. Each line
// gets the next session seq and is buffered for replay; then, unless the session is
// frozen (a re-dial in flight, lines wait behind the replay) it is forwarded live.
// It selects on `done` so a redirect — which closes the send side and signals done —
// wakes it immediately, without waiting on the next keystroke. On a redirect it returns
// so the connection re-dials, leaving every un-acked line in the buffer for replay.
// On socket EOF (lines closed) it half-closes the send side and returns.
func (b *bridge) runForwarder() {
	for {
		select {
		case <-b.done:
			// The stream ended under us (redirect flagged, disconnect, or Recv error).
			// Stop forwarding to it; any buffered input is replayed on the next shard.
			b.log.Debug("forwarder stopping (stream ended)")
			return
		case line, ok := <-b.conn.lines:
			if !ok {
				// Socket EOF: half-close the send side so the world sees end-of-input.
				// Hold sendMu — gRPC forbids CloseSend concurrent with a replay SendMsg.
				b.log.Debug("forwarder ending, half-closing send (socket EOF)")
				b.sendMu.Lock()
				_ = b.stream.CloseSend()
				b.sendMu.Unlock()
				return
			}

			seq, frozen := b.conn.sess.add(line)
			b.log.Debug("input buffered", "seq", seq, "frozen", frozen)

			// During a re-dial's freeze the line is buffered only; the writer replays the
			// un-acked window (including this line) once the new shard reports its resume
			// point. A line is never LOST. (One typed just after the redirect is flagged
			// but before freeze may also reach the old shard, which drops it.)
			if frozen {
				b.log.Debug("input held during freeze", "seq", seq)
				continue
			}

			// Serialize with replay sends (gRPC Send is not concurrent-safe). Once
			// thawed, replay is done, so this never contends for long.
			b.sendMu.Lock()
			err := b.sendInputLocked(seq, line)
			b.sendMu.Unlock()
			if err != nil {
				// Send failed — likely the writer flagged a redirect and closed the send
				// side concurrently. The line is already buffered for replay; return to
				// let the outer loop re-dial (or tear down if there is no redirect).
				b.log.Debug("input send failed", "seq", seq, "err", err)
				return
			}
			b.log.Debug("input forwarded", "seq", seq)
		}
	}
}

func (b *bridge) setRedirect(t redirectTarget) {
	b.mu.Lock()
	b.redirect = &t
	b.mu.Unlock()
}

func (b *bridge) takenRedirect() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.redirect != nil
}

// result reports the bridge outcome: (target, true) if a redirect was flagged, else
// (_, false). Read only after both loops have finished.
func (b *bridge) result() (redirectTarget, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.redirect == nil {
		return redirectTarget{}, false
	}
	return *b.redirect, true
}

// renderFrame turns one world ServerFrame into terminal bytes for this connection.
// This is where edge rendering happens: the world emits semantic markup (Output /
// PromptUpdate / Disconnect / Attached) and the gate decides how it lands on the
// wire. Redirect is handled in runWriter, not here. The frame kind is logged at Debug
// so DEBUG=1 shows what the world sent.
func (s *Server) renderFrame(log *slog.Logger, tc *telnet.Conn, f *playv1.ServerFrame) {
	switch pl := f.Payload.(type) {
	case *playv1.ServerFrame_Output:
		// Text to show; append a newline unless the frame opts out (no_newline).
		log.Debug("frame rendered", "frame", "output", "no_newline", pl.Output.GetNoNewline())
		if pl.Output.GetNoNewline() {
			_ = tc.Write(pl.Output.GetMarkup())
		} else {
			_ = tc.Write(pl.Output.GetMarkup() + "\r\n")
		}
	case *playv1.ServerFrame_Prompt:
		// Prompts are emitted without a trailing newline (partial line).
		log.Debug("frame rendered", "frame", "prompt")
		_ = tc.Write(pl.Prompt.GetMarkup())
	case *playv1.ServerFrame_Disconnect:
		log.Debug("frame rendered", "frame", "disconnect", "reason", pl.Disconnect.GetReason())
		_ = tc.Write("\r\n" + pl.Disconnect.GetReason() + "\r\n")
	case *playv1.ServerFrame_Attached:
		// Ack only; nothing to show. The piggybacked ack_input_seq is the resume point.
		log.Debug("frame rendered", "frame", "attached")
	}
}

// validateName checks a login name is safe to render and to use as an in-world
// targeting keyword. It returns (reason, false) for a rejected name (the reason is
// shown to the player on re-prompt) or ("", true) for an accepted one.
//
// This is a deliberately minimal stopgap until Phase 13 real auth/chargen — just
// enough that a name cannot inject into a terminal or confuse the targeting
// grammar. The rules:
//
//   - non-empty after trimming;
//   - at most maxNameLen runes (so it renders and types cleanly);
//   - every rune printable and not a control rune (terminal-injection defense;
//     ReadLine already strips controls, but a name is too load-bearing to assume
//     that, and we also reject other non-graphic runes like odd spaces here);
//   - no leading '.' and no embedded '.', because targeting parses `N.kw` /
//     `all.kw` — a dotted name would split into a count/selector and a keyword;
//   - no leading digit, because a leading digit reads as the `N.` count in `N.kw`.
//
// Letters, digits-after-the-first, and intra-name punctuation other than '.' are
// allowed; this is intentionally permissive on charset beyond the grammar hazards.
func validateName(name string) (string, bool) {
	if name == "" {
		return "a name is required", false
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return "too long", false
	}
	if name[0] == '.' {
		return "it can't start with a dot", false
	}
	if r, _ := utf8.DecodeRuneInString(name); unicode.IsDigit(r) {
		return "it can't start with a digit", false
	}
	if strings.ContainsRune(name, '.') {
		return "it can't contain a dot", false
	}
	for _, r := range name {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return "it contains an invalid character", false
		}
	}
	return "", true
}
