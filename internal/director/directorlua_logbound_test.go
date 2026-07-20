package director

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/luasandbox"
)

// directorlua_logbound_test.go — #456 at the director tier. director.log is length-capped, labelled
// source=builder_lua, and rate-limited per call so a flooding world script trips the breaker (a
// single director process has a wider blast radius than a per-zone VM).

func newTestAPI() *API {
	return &API{d: New("", newMemStore(), discardLog()), ctx: context.Background()}
}

// TestDirectorLogCappedAndLabelled: director.log truncates a huge message and labels it.
func TestDirectorLogCappedAndLabelled(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	ld, err := newLuaDirector(log, worldScriptKey,
		`function on_signal(e, p) director.log(string.rep("Q", 5000)) end`)
	if err != nil {
		t.Fatal(err)
	}
	ld.OnSignal(newTestAPI(), "x", nil)

	out := buf.String()
	if !strings.Contains(out, "source=builder_lua") {
		t.Errorf("director.log must be labelled source=builder_lua, got:\n%s", out[:min(len(out), 300)])
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("an over-cap director.log must be truncated")
	}
	if n := strings.Count(out, "Q"); n > luasandbox.MaxLogMsgBytes+8 {
		t.Errorf("director.log emitted %d 'Q's; not length-capped", n)
	}
}

// TestDirectorLogFloodTripsBreaker: a world script that floods director.log within one signal aborts
// and, over repeated signals, trips the breaker — observable as the TRIPPED ops line and, thereafter,
// the handler no longer producing log output (CallGlobal short-circuits on a disabled breaker).
func TestDirectorLogFloodTripsBreaker(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	ld, err := newLuaDirector(log, worldScriptKey,
		`function on_signal(e, p) for i = 1, 250 do director.log("spam") end end`)
	if err != nil {
		t.Fatal(err)
	}
	api := newTestAPI()

	tripped := false
	for i := 0; i < 50; i++ {
		ld.OnSignal(api, "x", nil)
		if strings.Contains(buf.String(), "TRIPPED") {
			tripped = true
			break
		}
	}
	if !tripped {
		t.Fatal("a world script flooding director.log every signal must trip the breaker")
	}
}
