// Package gate is the edge service: it accepts telnet connections, runs a minimal
// login, and proxies each player to a world shard over the gRPC Play stream.
// TLS/SSH, GMCP, and real auth arrive in later phases (docs/ACCOUNT.md, GMCP.md).
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

// Server accepts telnet connections and bridges them to world shards.
type Server struct {
	listen string
	dir    directory.Directory
	client playv1.PlayClient
}

// New builds a gate. cc is a (lazy) gRPC connection to the world; for Phase 1 the
// directory simply validates a shard exists and the single client conn is reused.
func New(listen string, dir directory.Directory, client playv1.PlayClient) *Server {
	return &Server{listen: listen, dir: dir, client: client}
}

// ListenAndServe accepts connections until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return err
	}
	slog.Info("gate listening", "addr", s.listen)
	go func() { <-ctx.Done(); _ = ln.Close() }()

	for {
		conn, err := ln.Accept()
		if err != nil {
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

func (s *Server) handle(ctx context.Context, nc net.Conn) {
	defer nc.Close()
	tc := telnet.New(nc)

	_ = tc.Write("\r\nWelcome to TelosMUD.\r\nBy what name shall you be known? ")
	name, err := tc.ReadLine()
	if err != nil {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Wanderer"
	}

	if _, ok := s.dir.ShardForCharacter(name); !ok {
		_ = tc.Write("\r\nNo world is available right now. Goodbye.\r\n")
		return
	}

	stream, err := s.client.Connect(ctx)
	if err != nil {
		_ = tc.Write("\r\nThe world is unreachable. Goodbye.\r\n")
		return
	}

	if err := stream.Send(&playv1.ClientFrame{Payload: &playv1.ClientFrame_Attach{Attach: &playv1.Attach{
		SessionId:   uuid.NewString(),
		CharacterId: name,
	}}}); err != nil {
		return
	}

	// Writer goroutine: world frames -> telnet.
	go func() {
		for {
			f, err := stream.Recv()
			if err != nil {
				_ = nc.Close()
				return
			}
			s.render(tc, f)
			if f.GetDisconnect() != nil {
				_ = nc.Close()
				return
			}
		}
	}()

	// Reader loop: telnet lines -> world input.
	var seq uint64
	for {
		line, err := tc.ReadLine()
		if err != nil {
			_ = stream.CloseSend()
			return
		}
		seq++
		if err := stream.Send(&playv1.ClientFrame{Payload: &playv1.ClientFrame_Input{Input: &playv1.InputLine{
			Seq:  seq,
			Text: line,
		}}}); err != nil {
			return
		}
	}
}

func (s *Server) render(tc *telnet.Conn, f *playv1.ServerFrame) {
	switch pl := f.Payload.(type) {
	case *playv1.ServerFrame_Output:
		if pl.Output.GetNoNewline() {
			_ = tc.Write(pl.Output.GetMarkup())
		} else {
			_ = tc.Write(pl.Output.GetMarkup() + "\r\n")
		}
	case *playv1.ServerFrame_Prompt:
		_ = tc.Write(pl.Prompt.GetMarkup())
	case *playv1.ServerFrame_Disconnect:
		_ = tc.Write("\r\n" + pl.Disconnect.GetReason() + "\r\n")
	case *playv1.ServerFrame_Attached:
		// Phase 1: nothing to show.
	}
}
