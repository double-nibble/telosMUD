package telnet

import (
	"bytes"
	"log/slog"
)

// gmcp.go is the telnet-codec half of GMCP (option 201, docs/GMCP.md Phase 9): the negotiation OFFER,
// the inbound IAC SB 201 <pkg> SP <json> IAC SE parser, and the outbound encoder. The dispatch in
// handleIAC (telnet.go) routes DO/DONT 201 to gmcpOn and SB 201 to readGMCPSubneg here; everything else
// stays refused/skipped. Package/payload SEMANTICS (Core.Hello, Core.Supports, the support filter) are
// the gate's job — this layer only frames bytes.

// OfferGMCP sends IAC WILL 201, the server-initiated GMCP offer. A capable client replies IAC DO 201
// (handled in handleIAC → gmcpOn). The gate calls this once at connection setup. Best-effort like the
// other negotiation writes — a dead socket surfaces on the next real write.
func (c *Conn) OfferGMCP() error {
	return c.writeRaw([]byte{iac, will, optGMCP})
}

// SetGMCPHandler installs the inbound GMCP sink (called with the package name + raw JSON bytes for each
// IAC SB 201 message). It MUST be set before the read loop starts (see Conn.onGmcp). A nil handler
// drops inbound GMCP silently.
func (c *Conn) SetGMCPHandler(fn func(pkg string, json []byte)) { c.onGmcp = fn }

// GMCPEnabled reports whether the client accepted the GMCP offer (IAC DO 201). The gate gates outbound
// emission on this; WriteGMCP also checks it, so callers need not.
func (c *Conn) GMCPEnabled() bool { return c.gmcpOn.Load() }

// readGMCPSubneg consumes an IAC SB 201 subnegotiation body up to IAC SE and hands the payload to the
// inbound sink. The option byte (201) was already consumed by handleIAC. An IAC inside the body is the
// terminator only when followed by SE; IAC IAC is an escaped literal 0xFF. The payload is capped at
// maxGMCPInBytes — an over-cap message is drained to IAC SE and DROPPED (fail-closed), never delivered,
// so a hostile client cannot grow the buffer without bound or smuggle a giant message. Never panics on
// malformed framing; an early EOF returns the read error (the connection is ending anyway).
func (c *Conn) readGMCPSubneg() error {
	var buf []byte
	over := false
	for {
		b, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		if b == iac {
			b2, err := c.r.ReadByte()
			if err != nil {
				return err
			}
			switch b2 {
			case se:
				return c.dispatchGMCP(buf, over)
			case iac:
				if !over {
					buf = append(buf, iac) // escaped 0xFF → one literal byte
				}
			default:
				// IAC <other> inside a subnegotiation is malformed per RFC 1143; skip the pair and
				// keep scanning for the real IAC SE rather than corrupting the payload.
			}
			continue
		}
		if len(buf) >= maxGMCPInBytes {
			over = true // keep draining to find IAC SE, but stop accumulating
			continue
		}
		buf = append(buf, b)
	}
}

// dispatchGMCP splits the assembled payload into "<pkg> SP <json>" and calls the inbound sink. A payload
// with no space is a data-less message (pkg only). An empty/over-cap payload is dropped. The JSON is
// COPIED so the sink owns it (buf is local but the callee may retain it). The package name is passed
// verbatim; the gate validates it (a known package or a clean drop).
func (c *Conn) dispatchGMCP(buf []byte, over bool) error {
	if over {
		slog.Debug("gmcp subneg over cap; dropped", "component", "telnet", "cap", maxGMCPInBytes)
		return nil
	}
	if c.onGmcp == nil || len(buf) == 0 {
		return nil
	}
	pkg, jsonPart := buf, []byte(nil)
	if i := bytes.IndexByte(buf, ' '); i >= 0 {
		pkg, jsonPart = buf[:i], buf[i+1:]
	}
	if len(pkg) == 0 {
		return nil // a leading-space payload has no package name — malformed, drop
	}
	jsonCopy := append([]byte(nil), jsonPart...)
	c.onGmcp(string(pkg), jsonCopy)
	return nil
}

// WriteGMCP frames a GMCP message as IAC SB 201 <pkg> SP <json> IAC SE and writes it, escaping any 0xFF
// in the payload as IAC IAC so a byte in the JSON can never be misread as a telnet command. It is a
// no-op when the client has not enabled GMCP (so the engine/gate can emit unconditionally). A data-less
// message (nil/empty json) omits the space + payload. Write is mutex-guarded, so this is safe to call
// from the writer goroutine concurrently with the reader's negotiation answers.
func (c *Conn) WriteGMCP(pkg string, json []byte) error {
	if !c.gmcpOn.Load() {
		return nil
	}
	payload := make([]byte, 0, len(pkg)+1+len(json))
	payload = append(payload, pkg...)
	if len(json) > 0 {
		payload = append(payload, ' ')
		payload = append(payload, json...)
	}
	if bytes.IndexByte(payload, iac) >= 0 {
		payload = bytes.ReplaceAll(payload, []byte{iac}, []byte{iac, iac})
	}
	frame := make([]byte, 0, len(payload)+5)
	frame = append(frame, iac, sb, optGMCP)
	frame = append(frame, payload...)
	frame = append(frame, iac, se)
	return c.writeRaw(frame)
}
