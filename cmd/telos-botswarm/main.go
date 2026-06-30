// Command telos-botswarm is the Phase-16.2 synthetic-client load generator. It opens N telnet sessions to a
// gate (the dev-autoauth name login), drives a realistic read-mostly command mix for a duration, and prints
// throughput + client-observed round-trip latency percentiles. The latency is the client's view (is the shard
// servicing commands); to see the zone heartbeat hold under the load, watch the server's telos.zone.tick_lag_ms
// metric (16.1) on the otel-collector's :8889 scrape endpoint while the run is in flight.
//
//	go run ./cmd/telos-botswarm -addr localhost:4000 -n 500 -duration 30s -ramp 5s
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/double-nibble/telosmud/internal/botswarm"
)

func main() {
	addr := flag.String("addr", env("TELOS_BOTSWARM_ADDR", "localhost:4000"), "gate address")
	bots := flag.Int("n", 200, "concurrent synthetic players")
	duration := flag.Duration("duration", 30*time.Second, "how long each bot drives traffic after login")
	ramp := flag.Duration("ramp", 5*time.Second, "spread bot connects over this window")
	think := flag.Duration("think", 150*time.Millisecond, "mean pause between a bot's commands")
	prefix := flag.String("prefix", "Bot", "character-name prefix")
	minConnect := flag.Float64("min-connect", 0.95, "exit non-zero if fewer than this fraction of bots connect+login")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "botswarm: %d bots -> %s for %s (ramp %s)\n", *bots, *addr, *duration, *ramp)
	start := time.Now()
	s := botswarm.Run(ctx, botswarm.Config{
		Addr: *addr, Bots: *bots, Duration: *duration, Ramp: *ramp, Prefix: *prefix, Think: *think,
	})
	elapsed := time.Since(start)

	tput := 0.0
	if elapsed > 0 {
		tput = float64(s.Commands) / elapsed.Seconds()
	}
	fmt.Printf("connected:    %d (errors %d)\n", s.Connected, s.ConnectErrs)
	fmt.Printf("commands:     %d (errors %d)\n", s.Commands, s.CommandErrs)
	fmt.Printf("throughput:   %.0f cmd/s over %s\n", tput, elapsed.Round(time.Millisecond))
	fmt.Printf("latency p50:  %s\n", s.Percentile(50).Round(time.Microsecond))
	fmt.Printf("latency p99:  %s\n", s.Percentile(99).Round(time.Microsecond))
	fmt.Printf("latency max:  %s\n", s.Percentile(100).Round(time.Microsecond))

	// A non-zero exit when too few bots connect/login makes the target usable as a CI / capstone gate: the
	// "N players sustain the tick rate" goal is meaningless if half the swarm never got in.
	want := int(*minConnect * float64(*bots))
	if s.Connected < want {
		fmt.Fprintf(os.Stderr, "FAIL: only %d/%d bots connected (need >= %d, -min-connect=%.2f)\n",
			s.Connected, *bots, want, *minConnect)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
