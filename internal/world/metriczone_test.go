package world

import (
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
