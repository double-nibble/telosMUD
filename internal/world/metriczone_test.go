package world

import (
	"net"
	"strings"
	"testing"
)

// TestMetricZoneReportsTemplateForAuthoredZone. A plain authored zone IS its own content: metricZone and id
// agree, so the label is the zone's own ref.
func TestMetricZoneReportsTemplateForAuthoredZone(t *testing.T) {
	z := newZone("midgaard")
	if got := z.metricZone(); got != "midgaard" {
		t.Fatalf("metricZone() = %q, want %q", got, "midgaard")
	}
}

// TestMetricZoneReportsTemplateForMintedInstance is the load-bearing guard behind #470: metricZone() is a
// SECURITY boundary, not a formatting helper. Instance ids are player-mintable (mintInstanceID → a fresh 128
// random bits per mint), so if the zone-label helper returned the id instead of the template a player could
// spray unbounded distinct Prometheus label values — a cardinality bomb / monitoring outage they can trigger
// on demand. This test fails the instant metricZone is "simplified" to return z.id.
func TestMetricZoneReportsTemplateForMintedInstance(t *testing.T) {
	// A real minted-shaped id: `<template>#<hex>`. Use the actual minter so the test tracks the real shape.
	id, err := mintInstanceID("darkwood")
	if err != nil {
		t.Fatalf("mintInstanceID: %v", err)
	}
	if !strings.Contains(id, instanceSep) || id == "darkwood" {
		t.Fatalf("minted id %q is not instance-shaped", id)
	}

	z := newInstanceZone(id, "darkwood")

	if !z.isInstance() {
		t.Fatalf("newInstanceZone(%q) did not produce an instance", id)
	}
	if got := z.metricZone(); got != "darkwood" {
		t.Fatalf("metricZone() = %q, want the template %q — an instance must NOT label metrics by its "+
			"player-mintable id (#470)", got, "darkwood")
	}
	if z.metricZone() == z.id {
		t.Fatalf("metricZone() returned the instance id %q — that is the cardinality bomb #470 forbids", z.id)
	}
}

// stubAddr is a net.Addr whose String() we control, to drive gateMetricHost for the non-TCP / portless cases.
type stubAddr struct{ s string }

func (a stubAddr) Network() string { return "stub" }
func (a stubAddr) String() string  { return a.s }

// TestGateMetricHostStripsEphemeralPort guards the stream-stall metric's `gate` label cardinality (a #470
// sibling): the label must be the gate HOST, never host:port. A gRPC peer address carries the gate's
// EPHEMERAL source port, which changes on every reconnect — as a Prometheus label that is unbounded,
// ops-driven cardinality (one dead series per gate dial). This fails the instant the code labels by the full
// peer address again.
func TestGateMetricHostStripsEphemeralPort(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr
		want string
	}{
		{"ipv4 tcp", &net.TCPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 54321}, "10.0.0.7"},
		{"ipv6 tcp", &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 40000}, "2001:db8::1"},
		{"host:port string", stubAddr{"gate-3.internal:443"}, "gate-3.internal"},
		{"portless falls back whole", stubAddr{"/tmp/gate.sock"}, "/tmp/gate.sock"},
		{"nil addr", nil, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gateMetricHost(c.addr); got != c.want {
				t.Fatalf("gateMetricHost(%v) = %q, want %q", c.addr, got, c.want)
			}
		})
	}

	// The load-bearing property in one line: two connections from the SAME gate host on DIFFERENT ephemeral
	// ports must collapse to ONE label value, or the metric is a cardinality bomb on gate churn.
	a := gateMetricHost(&net.TCPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 111})
	b := gateMetricHost(&net.TCPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 222})
	if a != b {
		t.Fatalf("same gate host on two ports produced two labels %q vs %q — unbounded cardinality", a, b)
	}
}
