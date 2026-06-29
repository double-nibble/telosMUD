package gate

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/double-nibble/telosmud/internal/telnet"
	"github.com/double-nibble/telosmud/internal/textsan"
)

// gmcp.go is the gate-side half of GMCP (Phase 9.1): per-connection Core.Supports tracking, the
// inbound Core.* handler (Hello/Supports/Ping), and the outbound support FILTER. The telnet codec
// (internal/telnet/gmcp.go) frames the bytes; this layer owns the SEMANTICS. The gate is the GMCP
// authority for Core.* — Supports is gate state, Ping is answered locally — and forwards nothing to
// the world in 9.1 (no inbound package needs the world yet; Char.Login etc. arrive with auth).

// gmcpState is a connection's GMCP negotiation state: the set of packages the client advertised via
// Core.Supports (so the encoder can drop anything it didn't ask for) and the client name from
// Core.Hello (for logging). The handler runs on the connection's line-pump goroutine while the
// outbound filter (supported) runs on the per-stream writer goroutine, so it is mutex-guarded.
type gmcpState struct {
	mu       sync.Mutex
	supports map[string]struct{} // advertised package names, e.g. "Char", "Char.Vitals", "Room"
	client   string              // sanitized Core.Hello client name (logging only)
}

func newGMCPState() *gmcpState { return &gmcpState{supports: map[string]struct{}{}} }

// maxSupportsEntries caps the advertised-package set so a client cannot grow gate memory without bound
// by streaming Core.Supports.Add with ever-new names (the codec already caps a single frame at 8 KiB;
// this bounds the accumulation across frames). No legitimate client advertises anywhere near this many.
const maxSupportsEntries = 256

// supported reports whether the client advertised support for pkg OR an ancestor package — advertising
// "Char 1" implies its sub-messages (Char.Vitals, …), the GMCP convention. So "Char.Vitals" is supported
// if the set contains "Char.Vitals" or "Char".
func (g *gmcpState) supported(pkg string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for p := pkg; p != ""; p = parentPackage(p) {
		if _, ok := g.supports[p]; ok {
			return true
		}
	}
	return false
}

func (g *gmcpState) setSupports(pkgs []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.supports = map[string]struct{}{}
	g.addLocked(pkgs)
}

func (g *gmcpState) addSupports(pkgs []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.addLocked(pkgs)
}

// addLocked inserts pkgs into the set up to maxSupportsEntries (a NEW key past the cap is dropped; a
// re-add of an existing key always succeeds). Caller holds g.mu.
func (g *gmcpState) addLocked(pkgs []string) {
	for _, p := range pkgs {
		if _, ok := g.supports[p]; !ok && len(g.supports) >= maxSupportsEntries {
			continue
		}
		g.supports[p] = struct{}{}
	}
}

func (g *gmcpState) removeSupports(pkgs []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range pkgs {
		delete(g.supports, p)
	}
}

func (g *gmcpState) setClient(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.client = name
}

// parentPackage returns the dotted parent of a package name ("Char.Vitals" → "Char"; "Char" → "").
func parentPackage(pkg string) string {
	if i := strings.LastIndexByte(pkg, '.'); i >= 0 {
		return pkg[:i]
	}
	return ""
}

// validGMCPPackage gates a package name to a strict, log-safe charset before it is used or logged. The
// telnet codec passes the raw inbound package name through verbatim (an escaped 0xFF or a control byte
// can ride in — it bypasses ReadLine's sanitizeLine by design), so the gate MUST validate here:
// non-empty, ≤64 bytes, letters/digits/'.' only, no leading/trailing dot. Anything else is dropped, and
// because a name that passes is pure ASCII alnum+dot, it is safe to log verbatim.
func validGMCPPackage(pkg string) bool {
	if pkg == "" || len(pkg) > 64 {
		return false
	}
	if pkg[0] == '.' || pkg[len(pkg)-1] == '.' {
		return false
	}
	for i := 0; i < len(pkg); i++ {
		c := pkg[i]
		if c == '.' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// parseSupports decodes a Core.Supports payload — a JSON array of "Package Version" strings, e.g.
// ["Char 1","Char.Vitals 1","Room 1"] — into the bare, validated package names. Malformed JSON yields
// nothing; an entry whose name fails validGMCPPackage is skipped (never enters the set).
func parseSupports(jsonBytes []byte) []string {
	var arr []string
	if err := json.Unmarshal(jsonBytes, &arr); err != nil {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		name := e
		if i := strings.IndexByte(e, ' '); i >= 0 {
			name = e[:i] // strip the version suffix
		}
		if validGMCPPackage(name) {
			out = append(out, name)
		}
	}
	return out
}

// gmcpHandler builds the inbound GMCP sink installed on the telnet codec. It runs on the line-pump
// goroutine as each IAC SB 201 message is parsed. It handles the Core.* housekeeping locally and drops
// everything else (nothing is forwarded to the world in 9.1). tc is captured for the Core.Ping reply.
func gmcpHandler(g *gmcpState, tc *telnet.Conn, log *slog.Logger) func(pkg string, json []byte) {
	return func(pkg string, payload []byte) {
		if !validGMCPPackage(pkg) {
			log.Debug("gmcp inbound dropped: invalid package name", "len", len(pkg))
			return
		}
		switch pkg {
		case "Core.Hello":
			var h struct {
				Client  string `json:"client"`
				Version string `json:"version"`
			}
			_ = json.Unmarshal(payload, &h)
			// The client field is attacker-controlled text; sanitize (graphic-only, capped) before it is
			// ever stored/logged.
			client := textsan.CleanName(h.Client, 32)
			g.setClient(client)
			log.Debug("gmcp Core.Hello", "client", client)
		case "Core.Supports.Set":
			g.setSupports(parseSupports(payload))
		case "Core.Supports.Add":
			g.addSupports(parseSupports(payload))
		case "Core.Supports.Remove":
			g.removeSupports(parseSupports(payload))
		case "Core.Ping":
			// Latency echo: reply with our own Core.Ping (a no-op if the client somehow isn't enabled).
			_ = tc.WriteGMCP("Core.Ping", nil)
		default:
			log.Debug("gmcp inbound package unhandled; dropped", "pkg", pkg)
		}
	}
}
