// Package gate is the edge service: it accepts telnet connections, runs a minimal
// login, and proxies each player to a world shard over the gRPC Play stream.
// TLS/SSH, GMCP, and real auth arrive in later phases (docs/ACCOUNT.md, GMCP.md).
//
// # The edge invariant
//
// The gate holds the socket; the world holds the player (docs/ARCHITECTURE.md §2).
// The gate is stateless beyond its live sockets: it owns no player state, only the
// TCP connection and the gRPC stream bridging it to a shard. Rendering happens here
// at the edge — the world emits semantic ServerFrames and the gate turns them into
// bytes for this particular terminal (engine = mechanism, content = flavor).
//
// # Connection lifecycle (Phase 1)
//
//  1. Accept a telnet connection (ListenAndServe -> handle).
//  2. Prompt for a name and read one line (the minimal stand-in for real auth).
//  3. Ask the directory which shard hosts that character (the directory seam).
//  4. Open a gRPC Play stream to the world and send Attach as the first frame
//     (the protocol requires Attach first; see docs/PROTOCOL.md §1).
//  5. Run two goroutines over the stream:
//     - a writer goroutine pumping world ServerFrames -> telnet (render), and
//     - the reader loop (this goroutine) pumping telnet lines -> world InputLines.
//  6. Tear down when either side ends: socket EOF closes the send side; a stream
//     error or a Disconnect frame closes the socket, which unblocks the other loop.
//
// # The two-goroutine model
//
// Each connection is served by exactly two goroutines so neither direction blocks
// the other (a core edge invariant: never block the per-connection read/write
// loops). handle's own goroutine is the reader loop (telnet -> world); it spawns
// one writer goroutine (world -> telnet). Closing the shared net.Conn is the
// cross-signal that unblocks whichever loop is parked in a blocking Read/Recv, so
// both shut down together.
//
// # Debug logging
//
// Every control-flow point logs at slog.Debug (off unless DEBUG=1; see
// internal/obs) through a per-server scoped logger tagged component=gate, so
// DEBUG=1 narrates the whole edge: connection accepted, login name, directory
// lookup, stream opened, Attach sent, each input line (seq), each rendered frame
// (kind), and teardown. Verbose tracing is Debug; only genuinely notable events
// (listening, listener failure) are Info/Error.
package gate

import (
	"context"
	"log/slog"
	"net"
	"strings"

	"github.com/google/uuid"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// Server accepts telnet connections and bridges them to world shards. It is
// stateless beyond its live sockets: listen address, the directory seam, and a
// shared (lazy) gRPC client to the world.
type Server struct {
	listen string
	dir    directory.Directory
	client playv1.PlayClient
	log    *slog.Logger // scoped logger, tagged component=gate
}

// New builds a gate. client is a (lazy) gRPC connection to the world; for Phase 1
// the directory simply validates a shard exists and the single client conn is
// reused for every player. The server carries a logger scoped to component=gate so
// all edge tracing shares that attribute.
func New(listen string, dir directory.Directory, client playv1.PlayClient) *Server {
	return &Server{
		listen: listen,
		dir:    dir,
		client: client,
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
			// Distinguish a clean shutdown (ctx cancelled -> listener closed)
			// from a real accept error.
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

// handle serves one connection end to end: login, directory lookup, stream
// attach, then the two-goroutine bridge. It is the reader-loop goroutine itself
// (telnet -> world) and spawns the writer goroutine (world -> telnet). The
// deferred Close is the teardown backstop: when this loop returns, the socket
// closes, which unblocks the writer goroutine's Recv.
func (s *Server) handle(ctx context.Context, nc net.Conn) {
	defer nc.Close()
	remote := nc.RemoteAddr().String()
	log := s.log.With("remote", remote)
	log.Debug("connection accepted")

	tc := telnet.New(nc)

	// --- minimal login: read a name (Phase 1 stand-in for real auth) ---
	_ = tc.Write("\r\nWelcome to TelosMUD.\r\nBy what name shall you be known? ")
	name, err := tc.ReadLine()
	if err != nil {
		log.Debug("connection closed before login", "err", err)
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Wanderer"
	}
	log.Debug("login name received", "character", name)

	// --- directory seam: resolve which shard hosts this character ---
	addr, ok := s.dir.ShardForCharacter(name)
	if !ok {
		log.Debug("no shard available for character", "character", name)
		_ = tc.Write("\r\nNo world is available right now. Goodbye.\r\n")
		return
	}
	log.Debug("shard resolved", "character", name, "addr", addr)

	// --- open the Play stream to the world ---
	stream, err := s.client.Connect(ctx)
	if err != nil {
		log.Debug("play stream dial failed", "character", name, "err", err)
		_ = tc.Write("\r\nThe world is unreachable. Goodbye.\r\n")
		return
	}
	log.Debug("play stream opened", "character", name)

	// Attach MUST be the first frame on the stream (docs/PROTOCOL.md §1).
	session := uuid.NewString()
	if err := stream.Send(&playv1.ClientFrame{Payload: &playv1.ClientFrame_Attach{Attach: &playv1.Attach{
		SessionId:   session,
		CharacterId: name,
	}}}); err != nil {
		log.Debug("attach send failed", "character", name, "session", session, "err", err)
		return
	}
	log.Debug("attach sent", "session", session, "character", name)

	// --- writer goroutine: world ServerFrames -> telnet ---
	go func() {
		for {
			f, err := stream.Recv()
			if err != nil {
				// Stream ended (server close, EOF, or our own teardown).
				// Closing the socket unblocks the reader loop below.
				log.Debug("play stream recv ended, closing socket", "session", session, "err", err)
				_ = nc.Close()
				return
			}
			s.render(log, tc, f)
			if f.GetDisconnect() != nil {
				// Server-initiated disconnect: drop the socket so the reader
				// loop's ReadLine errors and handle returns.
				log.Debug("disconnect frame received, closing socket", "session", session)
				_ = nc.Close()
				return
			}
		}
	}()

	// --- reader loop: telnet lines -> world InputLines ---
	// seq is monotonic per session (docs/PROTOCOL.md §1: input sequencing).
	var seq uint64
	for {
		line, err := tc.ReadLine()
		if err != nil {
			// Socket EOF / read error: half-close the send side so the world
			// sees end-of-input. The writer goroutine ends on the matching
			// Recv error.
			log.Debug("read loop ended, half-closing send", "session", session, "err", err)
			_ = stream.CloseSend()
			return
		}
		seq++
		log.Debug("input forwarded", "session", session, "seq", seq)
		if err := stream.Send(&playv1.ClientFrame{Payload: &playv1.ClientFrame_Input{Input: &playv1.InputLine{
			Seq:  seq,
			Text: line,
		}}}); err != nil {
			log.Debug("input send failed", "session", session, "seq", seq, "err", err)
			return
		}
	}
}

// render turns one world ServerFrame into terminal bytes for this connection.
// This is where edge rendering happens: the world emits semantic markup (Output /
// PromptUpdate / Disconnect / Attached) and the gate decides how it lands on the
// wire. The frame kind is logged at Debug so DEBUG=1 shows what the world sent.
func (s *Server) render(log *slog.Logger, tc *telnet.Conn, f *playv1.ServerFrame) {
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
		// Phase 1: ack only, nothing to show.
		log.Debug("frame rendered", "frame", "attached")
	}
}
