// Package gate is the edge service: it accepts telnet connections (plain and TLS),
// runs the login (browser OAuth via telos-account's device flow when a real account
// client is wired, a bare-name dev stub otherwise — docs/ACCOUNT.md), speaks GMCP
// (docs/GMCP.md), and proxies each player to a world shard over the gRPC Play stream.
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
//  2. Run the login: the browser OAuth device flow + character select when a real
//     account client is wired, the bare-name dev stub otherwise.
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
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/metrics"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// maxNameLen caps a character name's length. The name becomes the in-world entity
// name and a targeting keyword, so it must be short enough to render and type. 20
// runes is generous; validateName enforces it on both the bare-name dev login and
// the chargen name prompt.
const maxNameLen = 20

// Server accepts telnet connections and bridges them to world shards. It is
// stateless beyond its live sockets: a listen address, the directory seam (initial
// shard resolution), a per-address Play client pool for dialing shards, and the
// Phase-8 comms bus (the gate is a comms SINK — RoleGate, subscribe-only on
// chan/tell; docs/PHASE8-PLAN §1, P8-D1-B).
type Server struct {
	listen  string
	dir     directory.Directory
	pool    *pool
	comms   commbus.Bus   // RoleGate comms handle; never nil (Disabled when NATS is down)
	account AccountClient // Phase 14 seam to telos-account; a stub when no account service is configured
	// commsExpected is true when comms are CONFIGURED (a broker URL is set) — so a comms.Available()==false
	// means "the broker is down" (warn the player) rather than "comms simply aren't wired" (a dev/test gate,
	// stay silent). Off by default so the comms-agnostic journey tests see no notice.
	commsExpected bool
	// accountConfigured is true once a REAL account client is wired (WithAccountClient). It switches the
	// login flow from the bare "type a name" prompt to the OAuth device flow (Phase 15).
	accountConfigured bool
	// devAutoAuth (Phase 15.6, TELOS_DEV_AUTOAUTH) bypasses the browser OAuth flow with the bare name login —
	// for HEADLESS smoke/e2e + local dev against an account-backed gate. INSECURE: never enable in prod.
	devAutoAuth bool
	// devAuthAllowRemoteBind is the deliberate acknowledgment that a dev-autoauth gate may bind a non-loopback
	// address (TELOS_DEV_AUTOAUTH_ALLOW_REMOTE_BIND). It exists ONLY for sandboxed orchestration — a container
	// MUST bind 0.0.0.0 for Docker port-publishing, and its exposure is governed by the publish mapping, not
	// the process bind. Without it, a dev-autoauth gate refuses any non-loopback bind (the bare-metal footgun
	// guard, ListenAndServe). Setting it says "the network boundary is handled outside this process."
	devAuthAllowRemoteBind bool

	// Phase 14.6 transport posture: TLS is the encrypted default; plain telnet is OFF unless allowPlaintext.
	allowPlaintext bool
	tlsListen      string
	tlsCert        string
	tlsKey         string

	// writeTimeout (Phase 16.3) bounds a single outbound telnet write; a wedged client that blocks a write
	// past this is disconnected (the writer goroutine sees the error and closes the socket), reclaiming the
	// slot. 0 disables the deadline (the pre-16.3 unbounded behavior, which tests using a plain pipe rely on).
	writeTimeout time.Duration

	log *slog.Logger // scoped logger, tagged component=gate
}

// WithWriteTimeout sets the Phase-16.3 per-write deadline applied to every telnet connection (slow-client
// backpressure). 0 disables it. Returns the Server for chaining at construction.
func (s *Server) WithWriteTimeout(d time.Duration) *Server {
	s.writeTimeout = d
	return s
}

// WithTransports configures the Phase-14.6 listeners: plain telnet is enabled only when allowPlaintext is
// true (default off); TLS telnet is enabled when tlsListen + a cert/key are given. Returns the Server for
// chaining at construction.
func (s *Server) WithTransports(allowPlaintext bool, tlsListen, tlsCert, tlsKey string) *Server {
	s.allowPlaintext = allowPlaintext
	s.tlsListen = tlsListen
	s.tlsCert = tlsCert
	s.tlsKey = tlsKey
	return s
}

// WithDevAutoAuth is split across build tags (#96): devauth_dev.go carries the real setter in a
// `-tags telos_devauth` build, and devauth_release.go compiles a hard-refuse no-op into the default (release)
// build so the OAuth bypass is physically absent from a production binary. devAuthActive() (same tagged pair)
// gates every read of the bypass and is a compile-time constant false in release.

// WithDevAutoAuthAllowRemoteBind acknowledges that a dev-autoauth gate may bind a non-loopback address (for
// containerized dev/CI, where Docker requires a 0.0.0.0 bind and controls exposure via the port publish).
// Without it, ListenAndServe refuses a non-loopback bind while the bypass is on. Never set it on a bare host.
func (s *Server) WithDevAutoAuthAllowRemoteBind(on bool) *Server {
	s.devAuthAllowRemoteBind = on
	return s
}

// WithCommsExpected records that comms are CONFIGURED (a broker URL is set), so the gate warns a player
// at login when the bus is unavailable — a configured-but-down broker, not an unwired dev/test gate.
// cmd/telos-gate sets it from `cfg.NATS.URL != ""`. Returns the Server for chaining.
func (s *Server) WithCommsExpected(expected bool) *Server {
	s.commsExpected = expected
	return s
}

// WithAccountClient wires a real telos-account client (Phase 14); without it the Server keeps the stub set in
// newServer (the legacy "type a name" login). Returns the Server for chaining at construction.
func (s *Server) WithAccountClient(a AccountClient) *Server {
	if a != nil {
		s.account = a
		s.accountConfigured = true
	}
	return s
}

// New builds a gate over a real (insecure) client pool. dir resolves the initial
// shard for a login; the pool caches one gRPC conn per shard address, dialed on
// demand as players walk to new shards. comms is the gate's RoleGate comms bus
// (commbus.OpenGate from cmd/telos-gate — NEVER OpenWorld: a gate handed a world
// handle would defeat the publish ACL / impersonation gate; PHASE8-PLAN 8.1 review).
// A nil comms bus is normalized to a Disabled RoleGate no-op so comms degrades
// cleanly (NATS-down) rather than panicking.
func New(listen string, dir directory.Directory, comms commbus.Bus) *Server {
	return newServer(listen, dir, newPool(), comms)
}

// newServer is the injectable constructor: tests pass a pool wired to in-process
// bufconn shards and a MemBus GATE handle (commbus.NewWorldBus's gate side / a
// MemBus.GateHandle()) — NEVER a world handle, so a test exercises the same
// subscribe-only role production wires. A nil comms bus becomes a Disabled RoleGate
// bus so the gate still works exactly as pre-Phase-8 (the existing journey tests pass
// nil and stay green).
func newServer(listen string, dir directory.Directory, p *pool, comms commbus.Bus) *Server {
	if comms == nil {
		comms = commbus.Disabled(commbus.RoleGate)
	}
	return &Server{
		listen:  listen,
		dir:     dir,
		pool:    p,
		comms:   comms,
		account: stubAccountClient{}, // bare-name dev fallback; production replaces it via WithAccountClient
		log:     slog.With("component", "gate"),
	}
}

// ListenAndServe starts the configured transport listeners and serves until ctx is cancelled.
// PLAIN telnet runs only when explicitly enabled (allowPlaintext); TLS telnet runs when a listen addr +
// cert/key are configured. At least one transport must be enabled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	// DEV-AUTOAUTH BIND GUARD: the TELOS_DEV_AUTOAUTH bypass replaces OAuth with the bare name login, so a
	// gate running it must NEVER be reachable off-host — a non-loopback bind would be an open, remotely
	// exploitable backdoor. Refuse to start (fail-closed) if the bypass is on and any enabled transport is
	// about to bind a non-loopback address. This makes the bypass safe for local dev / headless smoke+e2e
	// (loopback only) while making the dangerous misconfiguration impossible rather than merely discouraged.
	//
	// SCOPE: this is the RUNTIME fence against a loopback→wildcard bind slip in a dev-tagged build. The
	// COMPLETE mitigation (#96) is the build-tag split: in a default (release) build the bypass code is
	// physically absent and devAuthActive() is a constant false, so this whole block is dead — a release
	// binary cannot enable the bypass by any env/config means, and this guard only ever runs when the
	// bypass was actually compiled in (`-tags telos_devauth`).
	if s.devAuthActive() {
		if !s.devAuthAllowRemoteBind {
			for _, addr := range s.enabledListenAddrs() {
				if !isLoopbackListen(addr) {
					return fmt.Errorf("gate: TELOS_DEV_AUTOAUTH (the no-OAuth dev bypass) refuses to bind non-loopback address %q — it must be unreachable off-host; bind 127.0.0.1/::1, or set TELOS_DEV_AUTOAUTH_ALLOW_REMOTE_BIND=1 ONLY in sandboxed orchestration where exposure is controlled outside the process (e.g. a container's port publish)", addr)
				}
			}
			slog.Warn("TELOS_DEV_AUTOAUTH ENABLED — OAuth is BYPASSED (bare name login); loopback-only. NEVER enable in production.", "listen", s.listen, "tls_listen", s.tlsListen)
		} else {
			slog.Warn("TELOS_DEV_AUTOAUTH ENABLED with ALLOW_REMOTE_BIND — OAuth is BYPASSED on a non-loopback bind; exposure MUST be controlled outside the process (container port publish / firewall). NEVER on a public host.", "listen", s.listen, "tls_listen", s.tlsListen)
		}
	}

	var wg sync.WaitGroup
	started := 0

	// Plain telnet — OFF by default; play crosses the wire in cleartext when enabled.
	if s.allowPlaintext {
		ln, err := net.Listen("tcp", s.listen)
		if err != nil {
			return err
		}
		slog.Warn("PLAIN telnet enabled — play crosses the wire UNENCRYPTED", "addr", s.listen)
		wg.Add(1)
		go func() { defer wg.Done(); s.serveListener(ctx, ln, false) }()
		started++
	} else {
		slog.Info("plain telnet disabled (set TELOS_GATE_ALLOW_PLAINTEXT=1 to enable)", "addr", s.listen)
	}

	// TLS telnet (the recommended default encrypted transport).
	if s.tlsListen != "" && s.tlsCert != "" && s.tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(s.tlsCert, s.tlsKey)
		if err != nil {
			return fmt.Errorf("gate: load TLS cert/key: %w", err)
		}
		ln, err := tls.Listen("tcp", s.tlsListen, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		if err != nil {
			return err
		}
		slog.Info("gate listening (TLS)", "addr", s.tlsListen)
		wg.Add(1)
		go func() { defer wg.Done(); s.serveListener(ctx, ln, true) }()
		started++
	}

	if started == 0 {
		return fmt.Errorf("gate: no transport enabled — configure TLS (cert+key) or set TELOS_GATE_ALLOW_PLAINTEXT=1")
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// enabledListenAddrs returns the addresses ListenAndServe will actually bind, given the transport config:
// the plain listen only when plaintext is allowed, the TLS listen only when it has a cert+key. Used by the
// dev-autoauth bind guard so it checks exactly what's about to be exposed (not a disabled transport).
func (s *Server) enabledListenAddrs() []string {
	var addrs []string
	if s.allowPlaintext {
		addrs = append(addrs, s.listen)
	}
	if s.tlsListen != "" && s.tlsCert != "" && s.tlsKey != "" {
		addrs = append(addrs, s.tlsListen)
	}
	return addrs
}

// isLoopbackListen reports whether a listen address binds ONLY the loopback interface — i.e. it is
// unreachable from off-host. A bare ":4000" (empty host) or "0.0.0.0:4000" binds all interfaces and is NOT
// loopback. "localhost" and any explicit loopback IP (127.0.0.0/8, ::1) are. A non-localhost hostname is
// treated as non-loopback (fail-closed) since we don't resolve it here — the guard errs on the safe side.
//
// CAVEAT: "localhost" is trusted by NAME, not resolution — a host whose /etc/hosts remaps localhost to a
// routable IP could bind off-host while passing this check. That requires host-write access (already a
// compromise) and is vanishingly rare on sane images, so we accept it; operators wanting strictness should
// bind a literal 127.0.0.1/::1 rather than "localhost".
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false // unparseable → don't assume safe
	}
	if host == "" {
		return false // ":4000" / all-interfaces bind
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// serveListener runs one transport's accept loop, handing each connection to its own goroutine. `encrypted`
// records whether the transport is TLS; it is plumbed to the login flow, which currently ignores it (no
// credentials cross the telnet wire in the OAuth device flow — the browser carries them). Closing the
// listener on ctx cancellation unblocks Accept.
func (s *Server) serveListener(ctx context.Context, ln net.Listener, encrypted bool) {
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				s.log.Warn("accept error", "err", err)
				return
			}
		}
		go s.handle(ctx, conn, encrypted)
	}
}

// handle serves one connection end to end: login, mint the session, resolve the
// initial shard, then run the bridge — re-dialing on each Redirect — until the
// socket drops or the world disconnects. The deferred Close is the teardown
// backstop: when handle returns, the socket closes, which unblocks any in-flight
// writer goroutine's Recv.
func (s *Server) handle(ctx context.Context, nc net.Conn, encrypted bool) {
	defer func() { _ = nc.Close() }()
	metrics.ConnOpened(ctx) // Phase 16.1: live gate connections
	defer metrics.ConnClosed(ctx)
	remote := nc.RemoteAddr().String()
	log := s.log.With("remote", remote)
	log.Debug("connection accepted")

	tc := telnet.New(nc)
	tc.SetWriteTimeout(s.writeTimeout) // Phase 16.3: bound writes so a wedged client is reclaimed, not pinned

	// --- GMCP: offer option 201 immediately and install the inbound Core.* handler, BEFORE the login
	// prompt, so a rich client (Mudlet) that negotiates + sends Core.Hello/Supports at connect is handled.
	// The handler runs on the line-pump's ReadLine goroutine as each IAC SB 201 message is parsed.
	gmcp := newGMCPState()
	_ = tc.OfferGMCP()
	// gmcpReq carries a WHITELISTED inbound GMCP request (e.g. Char.Items.Contents, #92) from the line-pump
	// (where gmcpHandler runs) to the bridge forwarder, which relays it to the world as a ClientFrame GmcpIn.
	// Fire-and-forget (no input seq / replay): a request lost on a redirect is simply re-asked by the client.
	gmcpReq := make(chan gmcpForward, 8)
	tc.SetGMCPHandler(gmcpHandler(gmcp, tc, gmcpReq, log))

	// --- login: the browser OAuth device flow (when an account service is wired) or the bare-name dev stub. ---
	_ = tc.Write("\r\nWelcome to TelosMUD.\r\n")
	name, account, ok := s.login(ctx, tc, log, remote, encrypted)
	if !ok {
		return // connection closed / aborted during login
	}
	log.Debug("login complete", "character", name)

	// --- mint the session ONCE: stable across every re-dial (docs/PROTOCOL.md §5).
	sess := newSession(uuid.NewString())
	log = log.With("session", sess.id, "character", name)
	log.Debug("session minted")

	// --- session assertion (Phase 14.3): account signs {account,character,session,exp}; the gate carries it
	// in every Attach and the world verifies it offline. Empty when auth is not configured (the stub returns
	// "" and the world skips verification — dev / pre-14.3). Issued ONCE here so a re-dial reuses the same
	// token (stable like the session id), within its short TTL.
	var assertion string
	if s.accountConfigured && account != "" { // account=="" is the stub / dev-autoauth path: no assertion
		actx, acancel := context.WithTimeout(ctx, 5*time.Second)
		tok, err := s.account.IssueSessionAssertion(actx, account, name, sess.id)
		acancel()
		if err != nil {
			log.Warn("issue session assertion failed", "err", err)
			_ = tc.Write("\r\nThe login service is unavailable right now. Goodbye.\r\n")
			return
		}
		assertion = tok
	}

	// --- restore the persisted terminal color preference (#23): color is an EDGE concern, so the gate reads
	// it DIRECTLY from telos-account (never through the world) and applies it BEFORE the first world frame is
	// rendered. Only the real account path carries a preference; the stub / dev-autoauth path (account=="")
	// reports "never set" and keeps the gate's default (color ON). A read failure is non-fatal — the session
	// simply keeps the default rather than dropping the login over a terminal cosmetic.
	if s.accountConfigured && account != "" {
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		enabled, set, err := s.account.GetColorPref(pctx, account)
		pcancel()
		switch {
		case err != nil:
			log.Warn("get color pref failed (keeping default)", "err", err)
		case set:
			tc.SetColor(enabled)
		}
	}

	// --- open the connection's comms client (Phase 8, P8-D1-B: the gate is the SINK).
	// This is established at CONNECTION scope — after login (the playerId is now known) and
	// OUTSIDE the re-dial loop below — and torn down by this single defer on the same return that
	// drops the session. That placement is the load-bearing handoff-transparency proof: a re-dial
	// (A→B) runs entirely inside runStream and never touches this subscription, so the player keeps
	// receiving comms across a cross-shard walk (PHASE8-PLAN §1, P8-D1; comms.go). A disabled bus
	// (NATS down) yields no-op subscriptions, so this is a clean no-op when comms are unavailable.
	cc := openComms(log, s.comms, tc, gmcp, name)
	defer cc.close()

	// Comms-down notice (#61): when comms are CONFIGURED but the bus is unavailable (broker down), the
	// player's channels + tells silently go nowhere. Tell them once, at login, so the silence isn't
	// mistaken for "nobody's talking." Suppressed when comms aren't configured at all (a dev/test gate),
	// so an unwired setup never nags.
	if s.commsExpected && !s.comms.Available() {
		_ = tc.Write("\r\nNotice: chat is currently offline — channels and tells are unavailable until it recovers.\r\n")
	}

	// Mid-session comms up/down notice (#80): #61 above is a one-shot LOGIN probe — it can't cover a bus that
	// drops (or recovers) AFTER login. Hook the bus's disconnect/reconnect transitions (driven by the NATS
	// callbacks, not a poll) so the player learns when chat goes offline or comes back. Gated on commsExpected
	// (don't nag an unwired dev gate) and on the concrete bus being able to transition (an AvailabilityWatcher
	// — only the NATS bus; a Mem/Disabled bus never loses/gains its transport, so this is a clean no-op).
	//
	// ISOLATION: the transition callback fires on the bus's SHARED NATS-callback goroutine (one per gate
	// process). It must NOT do the tc.Write there — a slow client's write (bounded only by the 30s
	// writeTimeout) would head-of-line-stall the notice for EVERY other session and the connection-lifecycle
	// callbacks, regressing the per-session isolation comms.go guarantees. So the callback does a NON-BLOCKING
	// COALESCING hand-off to a size-1 channel drained by this session's OWN goroutine, which does the actual
	// (bounded) write. Coalescing collapses a flap storm to the LATEST state rather than a serialized backlog
	// of stale booleans. cancelWatch + watchDone tear it all down at session end (same connection scope as the
	// comms subscription above — a cross-shard re-dial never touches it, so the notice survives a handoff).
	if s.commsExpected {
		if w, ok := s.comms.(commbus.AvailabilityWatcher); ok {
			avail := make(chan bool, 1)
			watchDone := make(chan struct{})
			go func() {
				for {
					select {
					case up := <-avail:
						if up {
							_ = tc.Write("\r\nNotice: chat is back online.\r\n")
						} else {
							_ = tc.Write("\r\nNotice: chat went offline — channels and tells are unavailable until it recovers.\r\n")
						}
						// Comm.Status for a client that advertised the umbrella Comm GMCP package. The sibling
						// Comm.Channel.* emitters gate on their leaf, but clients advertise the umbrella
						// ("Comm 1"), so "Comm" is the right gate for a new Comm.Status message.
						if gmcp.supported("Comm") {
							if up {
								_ = tc.WriteGMCP("Comm.Status", []byte(`{"available":true}`))
							} else {
								_ = tc.WriteGMCP("Comm.Status", []byte(`{"available":false}`))
							}
						}
					case <-watchDone:
						return
					}
				}
			}()
			cancelWatch := w.OnAvailabilityChange(func(available bool) {
				// Non-blocking coalescing push (runs on the shared NATS goroutine): keep only the LATEST state
				// so a flap can never back up that goroutine.
				select {
				case avail <- available:
				default:
					select { // full: drop the stale pending state
					case <-avail:
					default:
					}
					select { // then push the latest (best-effort; only this session pushes here)
					case avail <- available:
					default:
					}
				}
			})
			// Unregister the listener FIRST (no more pushes), THEN stop the drainer.
			defer func() { cancelWatch(); close(watchDone) }()
		}
	}

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
			// Edge-local commands (color) are a terminal concern, handled here and NOT forwarded to the
			// world — the gate owns rendering, the world owns game state.
			if handleColorCommand(ctx, tc, s.account, account, line, log) {
				continue
			}
			// promote/demote (#27): change an account's trust tier via the account service (authz enforced
			// there). Edge-local like color, since the account client lives at the gate.
			if handleTierCommand(ctx, tc, s.account, account, line, log) {
				continue
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
		log:       log,
		tc:        tc,
		nc:        nc,
		sess:      sess,
		name:      name,
		account:   account,
		assertion: assertion,
		lines:     lines,
		gmcp:      gmcp,
		gmcpReq:   gmcpReq,
	}
	var token string
	attachRetries := 0
	for {
		next, outcome := s.runStream(ctx, conn, addr, token)
		switch outcome {
		case outcomeRedirect:
			// The actual replay keys off the DESTINATION's ack_input_seq on its first (Attached)
			// frame (runStream / doReplay); the redirect carries no resume point.
			log.Debug("redirect received", "target", next.addr)
			addr, token = next.addr, next.token
			attachRetries = 0 // a successful bind clears the fresh-login refusal budget
		case outcomeUnavailable:
			// A fresh-login Attach was refused because the shard is draining (#324). Re-resolve through the
			// directory — whose zone leases have flipped to the peer — and re-dial on the SAME socket. The
			// token stays empty: this is still a fresh login, not a handoff bind. Bounded so a fully
			// saturated fleet can't spin here forever.
			attachRetries++
			if attachRetries > maxAttachRetries {
				log.Debug("attach kept returning Unavailable; giving up", "retries", attachRetries)
				_ = tc.Write("\r\nThe world is busy right now. Please reconnect in a moment.\r\n")
				return
			}
			select {
			case <-time.After(attachRetryBackoff):
			case <-ctx.Done():
				return
			}
			newAddr, ok := s.dir.ShardForCharacter(name)
			if !ok {
				log.Debug("no shard available on re-resolve after Unavailable")
				_ = tc.Write("\r\nNo world is available right now. Goodbye.\r\n")
				return
			}
			log.Debug("re-resolved after Unavailable", "old", addr, "new", newAddr, "retry", attachRetries)
			addr = newAddr
		default: // outcomeDone
			return // bridge ended without a redirect: socket/world closed.
		}
	}
}

// connState bundles the per-connection state the bridge needs across re-dials: the
// scoped logger, the telnet conn (and raw socket for the cross-close), the session
// (input buffer + stable id), the character name carried into each Attach, and the
// connection-scoped line channel fed by the pump goroutine.
type connState struct {
	log       *slog.Logger
	tc        *telnet.Conn
	nc        net.Conn
	sess      *session
	name      string
	account   string // Phase 14.3: the authenticated account id (carried in Attach); "" on the legacy path
	assertion string // Phase 14.3: the signed session assertion (carried in every Attach); "" when auth off
	lines     <-chan string
	gmcp      *gmcpState         // per-connection GMCP negotiation state (Phase 9.1); never nil
	gmcpReq   <-chan gmcpForward // inbound whitelisted GMCP requests to relay to the world (#92); never nil
}

// redirectTarget is what a finished bridge reports when the world asked the gate to
// migrate: where to dial and the token to present. No resume point travels here — the
// replay keys off the DESTINATION's ack_input_seq on its first (Attached) frame (see
// runStream / doReplay).
type redirectTarget struct {
	addr  string
	token string
}

// streamOutcome is how a finished bridge tells the outer re-dial loop what to do next.
type streamOutcome int

const (
	outcomeDone        streamOutcome = iota // tear the connection down (socket EOF, world Disconnect, or a non-retryable error)
	outcomeRedirect                         // re-dial the target the world named (a cross-shard handoff bind — carries a token)
	outcomeUnavailable                      // a fresh-login Attach was refused with codes.Unavailable (shard draining); re-resolve + re-dial (#324)
)

// attachRetry* bound the re-resolve loop a fresh-login Attach refusal drives (#324). A shard refuses a fresh
// login while it is draining so the arrival lands on the peer the zone leases have flipped to; the gate
// re-resolves through the directory and re-dials. The leases flip within a zone-lease TTL, so a handful of
// short backoffs comfortably covers the window without hanging a connect. Exhausting them means the fleet is
// genuinely saturated (every candidate draining), and the player is asked to reconnect.
const (
	maxAttachRetries   = 5
	attachRetryBackoff = 250 * time.Millisecond
)

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
// It returns outcomeRedirect (with the target) when the stream ended in a Redirect —
// the caller re-dials target. It returns outcomeUnavailable when a FRESH-LOGIN Attach
// was refused with codes.Unavailable (a draining shard) — the caller re-resolves and
// re-dials on the same socket (#324). It returns outcomeDone for any other end (socket
// EOF, world disconnect, dial/attach failure): the caller tears the connection down.
func (s *Server) runStream(ctx context.Context, c *connState, addr, token string) (redirectTarget, streamOutcome) {
	log := c.log.With("addr", addr)

	cli, err := s.pool.client(addr)
	if err != nil {
		log.Debug("shard dial failed", "err", err)
		_ = c.tc.Write("\r\nThe world is unreachable. Goodbye.\r\n")
		return redirectTarget{}, outcomeDone
	}
	stream, err := cli.Connect(ctx)
	if err != nil {
		log.Debug("play stream dial failed", "err", err)
		_ = c.tc.Write("\r\nThe world is unreachable. Goodbye.\r\n")
		return redirectTarget{}, outcomeDone
	}
	log.Debug("play stream opened")

	// Attach MUST be the first frame (docs/PROTOCOL.md §1). On a re-dial it carries the
	// handoff token; input_seq is the next seq the gate will send (the resume point).
	attach := &playv1.Attach{
		SessionId:        c.sess.id,
		AccountId:        c.account,
		CharacterId:      c.name,
		HandoffToken:     token,
		InputSeq:         c.sess.nextSeqValue(),
		SessionAssertion: c.assertion, // Phase 14.3: the world verifies this offline
	}
	if err := stream.Send(&playv1.ClientFrame{Payload: &playv1.ClientFrame_Attach{Attach: attach}}); err != nil {
		log.Debug("attach send failed", "err", err)
		return redirectTarget{}, outcomeDone
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
	target, outcome := br.result()
	if outcome == outcomeDone {
		// A real teardown (a player quit / world Disconnect, or socket EOF), NOT a re-dial.
		// Half-close the world stream's send side so the shard's reader loop promptly sees
		// end-of-input and runs its leave/detach (and thus the logout DURABLE FLUSH) NOW,
		// rather than waiting for the per-connection context to be cancelled when runConn
		// unwinds — an asynchronous, racy teardown that left the logout flush deferred to the
		// 60s link-death reap (or lost on a crash in that window). The forwarder's own EOF path
		// already CloseSends on socket EOF; this covers the world-initiated Disconnect path,
		// where the forwarder returns via `done` without closing the send side. Idempotent: a
		// second CloseSend (the forwarder already closed it) is a harmless no-op error we drop.
		// A redirect or a re-resolve (unavailable) must NOT take this path — the socket stays open
		// for the re-dial, and the refused stream is already dead (its Recv errored).
		br.sendMu.Lock()
		_ = stream.CloseSend()
		br.sendMu.Unlock()
	}
	return target, outcome
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

	mu          sync.Mutex
	redirect    *redirectTarget // set by the writer when a Redirect frame arrives
	unavailable bool            // set by the writer when a fresh-login Attach was refused with codes.Unavailable (#324)
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
			// A FRESH-LOGIN Attach refused with codes.Unavailable (the shard began draining between the
			// gate's resolve and the world's attach) is RETRYABLE (#324): the world refuses the arrival so
			// it lands on the peer the zone leases have flipped to, and expects the gate to re-resolve and
			// re-dial. Flag it and KEEP THE SOCKET OPEN for the outer loop to re-dial on.
			//
			// Guards: `first` (no frame ever arrived — so this is the Attach itself being refused, not a
			// mid-session drop) and `!b.replay` (a fresh login, no handoff token). A token-bearing re-dial
			// has exactly ONE valid destination — retrying it would race the pending-player TTL — so it must
			// tear down normally. Note gRPC also maps a transport failure (connection refused, server
			// graceful-stop) to codes.Unavailable, not just the world's explicit "shard draining" status; a
			// transport blip on a FRESH login is safe to re-resolve too (bounded, and it picks a live peer),
			// and `!b.replay` still keeps a handoff bind out of this path.
			if first && !b.replay && status.Code(err) == codes.Unavailable {
				b.log.Debug("fresh-login attach refused (shard draining); will re-resolve and re-dial")
				b.flagUnavailable()
				return
			}
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

		if err := b.srv.renderFrame(b.log, b.conn, f); err != nil {
			// A write error here is almost always the Phase-16.3 write-deadline firing on a wedged client
			// (or a dead socket). Either way the connection is unusable: close it so the reader's ReadLine
			// errors, the stream tears down, and the world reclaims the player's slot.
			b.log.Debug("frame write failed; closing socket (slow/dead client)", "err", err)
			_ = b.conn.nc.Close()
			return
		}

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

// sendGmcpIn relays one whitelisted inbound GMCP request to the world (#92). It takes sendMu to serialize
// with input/replay sends (gRPC Send is not concurrent-safe). Unlike input it carries NO seq and is NOT
// buffered for replay — a request in flight when the stream ends is simply dropped, and the client re-asks.
func (b *bridge) sendGmcpIn(f gmcpForward) error {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	return b.stream.Send(&playv1.ClientFrame{Payload: &playv1.ClientFrame_Gmcp{Gmcp: &playv1.GmcpIn{
		Pkg:  f.pkg,
		Json: f.json,
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
		case f := <-b.conn.gmcpReq:
			// A whitelisted inbound GMCP request (#92): relay it live (no seq, no replay). A send error
			// means the stream is ending — the writer's redirect/teardown path handles it; we just drop
			// this request (the client re-asks after the re-dial).
			if err := b.sendGmcpIn(f); err != nil {
				b.log.Debug("gmcp request forward failed", "pkg", f.pkg, "err", err)
			}
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

func (b *bridge) flagUnavailable() {
	b.mu.Lock()
	b.unavailable = true
	b.mu.Unlock()
}

// result reports the bridge outcome. Read only after both loops have finished. A redirect wins over an
// unavailable flag (they are mutually exclusive in practice, but redirect is the stronger signal).
func (b *bridge) result() (redirectTarget, streamOutcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.redirect != nil {
		return *b.redirect, outcomeRedirect
	}
	if b.unavailable {
		return redirectTarget{}, outcomeUnavailable
	}
	return redirectTarget{}, outcomeDone
}

// renderFrame turns one world ServerFrame into terminal bytes for this connection.
// This is where edge rendering happens: the world emits semantic markup (Output /
// PromptUpdate / Disconnect / Attached) and the gate decides how it lands on the
// wire. Redirect is handled in runWriter, not here. The frame kind is logged at Debug
// so DEBUG=1 shows what the world sent.
func (s *Server) renderFrame(log *slog.Logger, c *connState, f *playv1.ServerFrame) error {
	tc := c.tc
	switch pl := f.Payload.(type) {
	case *playv1.ServerFrame_Output:
		// Text to show; append a newline unless the frame opts out (no_newline).
		log.Debug("frame rendered", "frame", "output", "no_newline", pl.Output.GetNoNewline())
		if pl.Output.GetNoNewline() {
			return tc.Write(pl.Output.GetMarkup())
		}
		return tc.Write(pl.Output.GetMarkup() + "\r\n")
	case *playv1.ServerFrame_Prompt:
		// Prompts are emitted without a trailing newline (partial line).
		log.Debug("frame rendered", "frame", "prompt")
		return tc.Write(pl.Prompt.GetMarkup())
	case *playv1.ServerFrame_Gmcp:
		// Structured GMCP (Phase 9): emit only if the client advertised the package (or an ancestor) via
		// Core.Supports; the codec's WriteGMCP is itself a no-op until the client enabled GMCP. The
		// support filter keeps a client that asked for nothing silent.
		pkg := pl.Gmcp.GetPkg()
		switch {
		case !validGMCPPackage(pkg):
			// Defense-in-depth on the OUTBOUND side, symmetric with the inbound gate: the package is
			// engine-set today (trusted), but the moment a content/Lua path can name a GMCP package, a
			// CR/LF/ESC in the name would inject into the client's terminal. Drop+log (len only) so that
			// can never happen, regardless of who set the name.
			log.Debug("gmcp frame dropped: invalid package name from world", "len", len(pkg))
		case c.gmcp.supported(pkg):
			log.Debug("frame rendered", "frame", "gmcp", "pkg", pkg)
			return tc.WriteGMCP(pkg, pl.Gmcp.GetJson())
		default:
			log.Debug("gmcp frame dropped: package not advertised", "pkg", pkg)
		}
	case *playv1.ServerFrame_Screen:
		// Trusted full-screen/ANSI output (#31): write the raw bytes VERBATIM (IAC-escaped, but NOT
		// sanitized or color-rendered) so cursor/erase/scroll control survives. Provenance is the world's
		// responsibility — only engine-owned output or a trust-gated screen.* capability emits this frame,
		// so player text never reaches the raw path. No word-wrap, no trailing newline (the bytes are a
		// complete screen sequence).
		//
		// CLIENT CAPABILITY: a connection that disabled ANSI (`color off`) or a non-ANSI terminal must not
		// receive raw escapes — they would garble as literal bytes. We reuse the color toggle as the coarse
		// ANSI-capability signal (there is no finer TTYPE probe today) and DROP the frame when it is off, so
		// the raw path never reaches a plain client. Symmetric with Write, where `color off` suppresses SGR.
		if !tc.ColorEnabled() {
			log.Debug("screen frame dropped: client ANSI disabled", "bytes", len(pl.Screen.GetData()))
			return nil
		}
		log.Debug("frame rendered", "frame", "screen", "bytes", len(pl.Screen.GetData()))
		return tc.WriteScreen(pl.Screen.GetData())
	case *playv1.ServerFrame_Disconnect:
		log.Debug("frame rendered", "frame", "disconnect", "reason", pl.Disconnect.GetReason())
		return tc.Write("\r\n" + pl.Disconnect.GetReason() + "\r\n")
	case *playv1.ServerFrame_Attached:
		// Ack only; nothing to show. The piggybacked ack_input_seq is the resume point.
		log.Debug("frame rendered", "frame", "attached")
	}
	return nil
}

// validateName checks a login name is safe to render and to use as an in-world
// targeting keyword. It returns (reason, false) for a rejected name (the reason is
// shown to the player on re-prompt) or ("", true) for an accepted one.
//
// This is the shared name-safety gate for the bare-name dev login and the chargen
// name prompt — just enough that a name cannot inject into a terminal or confuse
// the targeting grammar. The rules:
//
//   - non-empty after trimming;
//   - at most maxNameLen runes (so it renders and types cleanly);
//   - every rune printable and not a control rune (terminal-injection defense;
//     ReadLine already strips controls, but a name is too load-bearing to assume
//     that, and we also reject other non-graphic runes like odd spaces here);
//   - no leading '.' and no embedded '.', because targeting parses `N.kw` /
//     `all.kw` — a dotted name would split into a count/selector and a keyword;
//   - no leading digit, because a leading digit reads as the `N.` count in `N.kw`;
//   - no '{' or '}', reserved for the {{TOKEN}} color markup — a name embedding a
//     known token would render colored on telnet and strip differently in GMCP
//     (an impersonation vector; mirrors account.ValidateCharacterName).
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
	if strings.ContainsAny(name, "{}") {
		return "it can't contain braces", false
	}
	for _, r := range name {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return "it contains an invalid character", false
		}
	}
	return "", true
}
