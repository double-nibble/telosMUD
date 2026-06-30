// Package botswarm is the Phase-16.2 synthetic-client load generator: it opens N telnet sessions to a gate,
// logs each in (the dev-autoauth name login), drives a realistic command mix, and reports throughput +
// client-observed round-trip latency. It is the LOAD half of "N players sustain the tick rate"; the OTel
// metrics (16.1) are the MEASURE half. The two are orthogonal on purpose: command dispatch is synchronous off
// the zone inbox, so a bot's send->prompt RTT measures network + queueing + dispatch (is the shard SERVICING
// commands), while zone heartbeat health is read separately from the server-side telos.zone.tick_lag_ms
// metric. Watch BOTH: rising RTT means the accept/render path is saturating; rising tick-lag means the pulse
// itself is overrunning. Driven by cmd/telos-botswarm; the bot logic is here so it's hermetically testable
// against a fake gate.
package botswarm

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"sync"
	"time"
)

// Config tunes a load run.
type Config struct {
	Addr     string        // gate address, e.g. "localhost:4000"
	Bots     int           // concurrent synthetic players
	Duration time.Duration // how long each bot drives traffic after login
	Ramp     time.Duration // spread bot starts over this window (avoid a thundering connect herd)
	Prefix   string        // character-name prefix; each bot appends its index
	Think    time.Duration // mean pause between a bot's commands
}

// Stats is the aggregate result of a run.
type Stats struct {
	Connected   int
	ConnectErrs int
	Commands    int
	CommandErrs int
	Latencies   []time.Duration // per-command round-trip (send -> next prompt)
}

// prompt is the per-line sync token the gate renders; a bot reads up to it to time a command's round trip.
const prompt = "> "

// command mix a bot cycles through — cheap, read-mostly verbs that exercise the zone loop + render path.
var commands = []string{"look", "north", "look", "south", "look", "east", "who", "look", "west"}

// Run drives the load and returns the aggregate stats. It returns when ctx is cancelled or all bots finish
// their Duration.
func Run(ctx context.Context, cfg Config) Stats {
	if cfg.Bots <= 0 {
		cfg.Bots = 1
	}
	if cfg.Think <= 0 {
		cfg.Think = 100 * time.Millisecond
	}
	var (
		mu  sync.Mutex
		agg Stats
		wg  sync.WaitGroup
	)
	for i := 0; i < cfg.Bots; i++ {
		// Spread starts across the ramp window so 1000 bots don't SYN-flood the gate in one instant.
		var delay time.Duration
		if cfg.Ramp > 0 {
			delay = time.Duration(int64(cfg.Ramp) * int64(i) / int64(cfg.Bots))
		}
		wg.Add(1)
		go func(idx int, delay time.Duration) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			s := runBot(ctx, cfg, fmt.Sprintf("%s%d", cfg.Prefix, idx))
			mu.Lock()
			agg.Connected += s.Connected
			agg.ConnectErrs += s.ConnectErrs
			agg.Commands += s.Commands
			agg.CommandErrs += s.CommandErrs
			agg.Latencies = append(agg.Latencies, s.Latencies...)
			mu.Unlock()
		}(i, delay)
	}
	wg.Wait()
	return agg
}

// runBot connects, logs in by name, and drives the command mix for cfg.Duration.
func runBot(ctx context.Context, cfg Config, name string) Stats {
	var s Stats
	conn, err := net.DialTimeout("tcp", cfg.Addr, 5*time.Second)
	if err != nil {
		s.ConnectErrs++
		return s
	}
	defer func() { _ = conn.Close() }()
	s.Connected = 1

	// Login: wait for the name prompt, send the name, then wait for the first room prompt.
	if ok, _ := readUntil(conn, "known?", 5*time.Second); !ok {
		s.ConnectErrs++
		return s
	}
	if _, err := conn.Write([]byte(name + "\r\n")); err != nil {
		s.ConnectErrs++
		return s
	}
	if ok, _ := readUntil(conn, prompt, 5*time.Second); !ok {
		s.ConnectErrs++
		return s
	}

	deadline := time.Now().Add(cfg.Duration)
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(len(name)))) //nolint:gosec // load-gen jitter, not security
	for ctx.Err() == nil && time.Now().Before(deadline) {
		cmd := commands[rng.Intn(len(commands))]
		t0 := time.Now()
		if _, err := conn.Write([]byte(cmd + "\r\n")); err != nil {
			s.CommandErrs++
			return s
		}
		ok, _ := readUntil(conn, prompt, 5*time.Second)
		if !ok {
			s.CommandErrs++
			return s
		}
		s.Commands++
		s.Latencies = append(s.Latencies, time.Since(t0))

		// Think time with jitter (0.5x–1.5x), so bots don't march in lockstep.
		think := time.Duration(float64(cfg.Think) * (0.5 + rng.Float64()))
		select {
		case <-ctx.Done():
			return s
		case <-time.After(think):
		}
	}
	return s
}

// readUntil reads from conn until sub appears or the timeout elapses. Telnet IAC/GMCP bytes are read + ignored
// (the bot scans for the textual prompt, which never appears inside an IAC sequence). Only the tail is kept so
// a chatty stream can't grow the buffer unbounded.
func readUntil(conn net.Conn, sub string, timeout time.Duration) (bool, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	subB := []byte(sub)
	var acc []byte
	buf := make([]byte, 2048)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			acc = append(acc, buf[:n]...)
			if bytes.Contains(acc, subB) {
				return true, nil
			}
			if len(acc) > 8192 {
				acc = acc[len(acc)-len(subB):]
			}
		}
		if err != nil {
			return false, err // timeout / EOF
		}
	}
}

// Percentile returns the p-th percentile (0..100) of the recorded command latencies.
func (s Stats) Percentile(p float64) time.Duration {
	if len(s.Latencies) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), s.Latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(p / 100 * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
