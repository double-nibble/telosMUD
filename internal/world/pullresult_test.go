package world

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// TestPullResultDelivery proves the zone-goroutine delivery of a coordinated-pull outcome (#230): a
// pullResultMsg for a resident builder is sent to their session; one for an absent player is a safe no-op
// (the single-writer guard over z.players — mirrors the reload readout).
func TestPullResultDelivery(t *testing.T) {
	z := newZone("test")
	s := &session{character: "Builder", out: make(chan *playv1.ServerFrame, 8), epoch: 1}
	z.players["Builder"] = s

	z.handle(pullResultMsg{player: "Builder", summary: "pull: content version \"v1\" installed."})
	select {
	case f := <-s.out:
		if f == nil || !strings.Contains(f.GetOutput().GetMarkup(), "installed") {
			t.Fatalf("resident builder got wrong/nil frame: %+v", f)
		}
	default:
		t.Fatal("resident builder should receive the pull result")
	}

	z.handle(pullResultMsg{player: "Ghost", summary: "pull: done."})
	select {
	case <-s.out:
		t.Fatal("pull result wrongly delivered for an absent player id")
	default:
	}
}

// TestDeliverPullResultRoutesAndFormats proves the shard-side fan-out (#230): a director's world-scope
// PullResultEvent broadcast is parsed, formatted into the pass/fail line, and posted to hosted zones, where
// the zone hosting the requesting builder delivers it. Covers success, a failure with detail, an empty-
// version rejection, and a blank-actor drop.
func TestDeliverPullResultRoutesAndFormats(t *testing.T) {
	z := newZone("test")
	s := &session{character: "Ada", out: make(chan *playv1.ServerFrame, 8), epoch: 1}
	z.players["Ada"] = s
	sh := &Shard{zones: map[string]*Zone{"test": z}}
	z.shard = sh
	sr := &scopeReplication{shard: sh, log: slog.Default()}

	cases := []struct {
		res  contentbus.PullResult
		want string
	}{
		{contentbus.PullResult{Version: "v1", Actor: "Ada", OK: true}, `content version "v1" installed`},
		{contentbus.PullResult{Version: "v2", Actor: "Ada", OK: false, Detail: "prune guard refused"}, `content version "v2" was not installed — prune guard refused`},
		{contentbus.PullResult{Version: "", Actor: "Ada", OK: false, Detail: "no version specified"}, `request rejected — no version specified`},
	}
	for _, tc := range cases {
		payload, err := json.Marshal(tc.res)
		if err != nil {
			t.Fatal(err)
		}
		sr.onScopeEvent("world", "", contentbus.PullResultEvent, payload)
		select {
		case m := <-z.inbox:
			z.handle(m) // process the posted pullResultMsg on the (test-driven) zone goroutine
		default:
			t.Fatalf("no message posted for %+v", tc.res)
		}
		select {
		case f := <-s.out:
			if got := f.GetOutput().GetMarkup(); !strings.Contains(got, tc.want) {
				t.Fatalf("summary = %q, want it to contain %q", got, tc.want)
			}
		default:
			t.Fatalf("Ada received no pull result for %+v", tc.res)
		}
	}

	// A blank actor has no one to notify — dropped, nothing posted.
	sr.onScopeEvent("world", "", contentbus.PullResultEvent, json.RawMessage(`{"version":"v9","actor":"","ok":true}`))
	select {
	case <-z.inbox:
		t.Fatal("a blank-actor pull result must be dropped, not posted")
	default:
	}
}
