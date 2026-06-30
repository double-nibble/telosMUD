package botswarm

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"
)

// fakeGate stands in for telos-gate's login + prompt protocol: greet with the name prompt, accept a name, then
// answer every command line with a room render ending in the "> " sync prompt.
func fakeGate(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = c.Write([]byte("\r\nWelcome to TelosMUD.\r\nBy what name shall you be known? "))
				br := bufio.NewReader(c)
				if _, err := br.ReadString('\n'); err != nil { // the name line
					return
				}
				_, _ = c.Write([]byte("The Temple Square\r\nA broad plaza.\r\n> "))
				for {
					if _, err := br.ReadString('\n'); err != nil { // a command line
						return
					}
					_, _ = c.Write([]byte("You look around.\r\n> "))
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestBotSwarmDrivesTraffic(t *testing.T) {
	addr := fakeGate(t)
	s := Run(context.Background(), Config{
		Addr: addr, Bots: 5, Duration: 200 * time.Millisecond, Think: 5 * time.Millisecond, Prefix: "Bot",
	})
	if s.Connected != 5 {
		t.Fatalf("connected = %d, want 5", s.Connected)
	}
	if s.ConnectErrs != 0 {
		t.Fatalf("connect errors = %d, want 0", s.ConnectErrs)
	}
	if s.Commands == 0 {
		t.Fatal("no commands were driven")
	}
	if s.CommandErrs != 0 {
		t.Fatalf("command errors = %d, want 0", s.CommandErrs)
	}
	if p99 := s.Percentile(99); p99 <= 0 {
		t.Fatalf("p99 latency = %v, want > 0 (latencies recorded)", p99)
	}
}

func TestBotSwarmConnectErrorOnDeadAddr(t *testing.T) {
	// A port nobody is listening on: the bots count connect errors, not panic.
	s := Run(context.Background(), Config{Addr: "127.0.0.1:1", Bots: 3, Duration: 50 * time.Millisecond})
	if s.Connected != 0 || s.ConnectErrs != 3 {
		t.Fatalf("dead addr: connected=%d errs=%d, want 0/3", s.Connected, s.ConnectErrs)
	}
}
