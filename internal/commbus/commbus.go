// Package commbus is the player-comms transport for Phase 8 (docs/PHASE8-PLAN.md): the cross-shard
// fan-out layer that carries player-authored chat (channels), directed tells, and presence between
// the WORLD shards (the message SOURCE) and the GATES (the message SINK). It is a SIBLING of, and
// deliberately separate from, internal/contentbus: that bus carries (kind,ref,pack) content
// invalidations; THIS bus carries player-scoped comms with an engine-set author, a per-author
// sequence, and an idempotency key. They may share the NATS server but NOT the subject space, the
// payloads, or the code (PHASE8-PLAN §7; the Phase-8 / Phase-10 boundary).
//
// # The trust / ordering boundary (the riskiest seam — read this)
//
// Comms is the first player-authored, cross-shard, fan-out surface. No single-writer serializes a
// cross-shard channel, so correctness rests on two invariants enforced HERE, in the transport:
//
//  1. The publish ACL (P8-A2 — the impersonation gate). Subjects under telos.comms.chan.* and
//     telos.comms.tell.* are published to by WORLD shards ONLY; GATES are SUBSCRIBE-ONLY on them. A
//     receiver trusts the payload's author field PRECISELY BECAUSE the only writers are source
//     worlds that set it from the live *Entity. If a gate could publish, the impersonation gate is
//     gone. This is enforced as a HANDLE ASYMMETRY: OpenWorld returns a handle that can Publish +
//     Subscribe; OpenGate returns a handle whose Publish on a chan/tell subject is REFUSED
//     (ErrPublishForbidden) — the gate role structurally lacks the chan/tell publish capability.
//
//  2. Per-author ordering (P8-A3). Every message carries a monotonic per-author Seq; a single
//     subject is one ordered stream (the broker — and the MemBus — impose one publish order per
//     subject), so every subscriber to that subject sees the same order. We promise per-author /
//     per-subject order, never a global cross-author clock (none exists, none is needed).
//
// # Optional, never fatal (mirrors contentbus.openContentBus)
//
// The bus is OPTIONAL: NATS unreachable => a DISABLED no-op handle (Publish/Subscribe are safe
// no-ops, never a crash), exactly as hot reload degrades when NATS is down. A busless gate/world is
// byte-identical to a pre-Phase-8 process; the empty-boot invariant is untouched.
//
// # Mem fallback (hermetic tests)
//
// MemBus mirrors the NATS observable semantics in-process (a subject→subscriber map with per-sub
// ORDERED delivery and wildcard support) so the cross-shard tests run N shards + N gates in ONE
// process against ONE MemBus with no live broker — exactly as contentbus.MemBus does. memjs.go is
// the JetStream STAND-IN (an append log + a delivered-cursor) the durable-tell/mail slices (8.5+)
// will build on; only the stand-in lands here.
package commbus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Subject taxonomy (P8-D2). A subject HIERARCHY under the telos.comms. root so it never collides
// with contentbus's content.invalidate. The durable JetStream streams (dtell/mail) are slice 8.5+;
// 8.1 lays the taxonomy and the two transient roots (chan/tell) plus presence.
//
// SubjectRoot is the namespace prefix for every comms subject. ChanPrefix/TellPrefix are the two
// PLAYER-AUTHORED roots the publish ACL guards (world-publish, gate-subscribe-only). PresenceSubject
// is the per-shard heartbeat fan-in (slice 8.4) — engine mechanism, not player-authored, so it is
// NOT under the chan/tell ACL (a shard publishing its own residents' presence is not impersonation).
const (
	SubjectRoot     = "telos.comms."
	ChanPrefix      = SubjectRoot + "chan." // telos.comms.chan.<channelRef>
	TellPrefix      = SubjectRoot + "tell." // telos.comms.tell.<targetPlayerId>
	PresenceSubject = SubjectRoot + "presence"
	// ConfigPrefix is the per-player COMMS-CONFIG root (Phase 8.6): telos.comms.config.<playerId>. The
	// SOURCE world publishes a player's effective receiver-side comms config — the {enabled ∩ hearable}
	// channel hear-set, the ignore list, the AFK flag — to their personal config subject; the player's
	// GATE subscribes to its OWN config subject and re-subscribes its concrete channel subjects + caches
	// the ignore funnel from it. Like presence (and unlike chan/tell) it is engine mechanism, NOT
	// player-authored, so it is deliberately NOT under the chan/tell publish ACL (a world publishing a
	// player's own derived config is not impersonation). A gate subscribes ONLY its own concrete
	// config.<self> subject — never a config.* wildcard — so no config can leak across players.
	ConfigPrefix = SubjectRoot + "config." // telos.comms.config.<playerId>
)

// ChanSubject / TellSubject build the per-ref / per-target subject from a VALIDATED token. The
// caller (the source world) must pass a known channel ref / a resolved player id — never free-form
// client input concatenated into the subject space (P8-A8, subject injection). 8.1 only provides the
// builders; the channel-ref validation lives with channel_defs in slice 8.3.
func ChanSubject(channelRef string) string { return ChanPrefix + channelRef }

// TellSubject builds the per-target tell subject — see ChanSubject for the validation contract.
func TellSubject(playerID string) string { return TellPrefix + playerID }

// ConfigSubject builds the per-player comms-config subject (Phase 8.6). Same validated-token contract
// as TellSubject: the caller passes a resolved player id, never free-form client text.
func ConfigSubject(playerID string) string { return ConfigPrefix + playerID }

// isACLGuarded reports whether subj is a player-authored subject the publish ACL protects (chan/tell).
// These are the subjects a GATE may subscribe to but must NEVER publish to (P8-A2). Presence and any
// future engine-mechanism subject are deliberately NOT guarded.
func isACLGuarded(subj string) bool {
	return strings.HasPrefix(subj, ChanPrefix) || strings.HasPrefix(subj, TellPrefix)
}

// errors returned by the bus.
var (
	// ErrBusClosed is returned by Publish/Subscribe on a closed bus.
	ErrBusClosed = errors.New("commbus: bus is closed")

	// ErrPublishForbidden is the ACL refusal (P8-A2, the impersonation gate): a GATE-role handle
	// tried to Publish on a chan/tell subject. The gate is subscribe-only on those subjects; only a
	// WORLD-role handle (which sets the author from the live *Entity) may publish them. This is the
	// single most security-sensitive error in the package — a returned ErrPublishForbidden is the
	// impersonation gate doing its job.
	ErrPublishForbidden = errors.New("commbus: publish forbidden on this subject for this role (gates are subscribe-only on chan/tell)")
)

// Role is the publish capability of a bus handle — the structural half of the ACL (P8-A2). A handle
// is opened as exactly one role and carries it for life; the role is set by which constructor wired
// it (OpenWorld vs OpenGate / NewWorldBus vs NewGateBus), NEVER by a field a caller can flip.
type Role int

const (
	// RoleWorld is the message SOURCE: it MAY publish on chan/tell subjects (it holds the
	// authoritative author identity and sets the engine-set author field).
	RoleWorld Role = iota
	// RoleGate is the message SINK: it MAY subscribe to chan/tell subjects but MUST NOT publish on
	// them (the impersonation gate). A RoleGate Publish on a guarded subject returns
	// ErrPublishForbidden.
	RoleGate
)

func (r Role) String() string {
	switch r {
	case RoleWorld:
		return "world"
	case RoleGate:
		return "gate"
	default:
		return "unknown"
	}
}

// Message is the comms wire payload (P8-A2 / P8-A3). It is JSON-serialized over the bus to match
// contentbus's wire discipline (one marshal/unmarshal seam, a malformed payload is a handled error,
// not a panic). The SECURITY-LOAD-BEARING fields are the three attribution/ordering fields, set by
// the SOURCE WORLD and NEVER by a client:
//
//   - AuthorID / AuthorName: the ENGINE-SET author (P8-A2). The source world stamps these from the
//     live *Entity at publish time. The receiver renders AuthorName as the speaker. A client input
//     frame carries NO author field; there is no path for a player to set this. The gate (sink)
//     RENDERS this field but never AUTHORS it.
//   - Seq: the author's MONOTONIC per-author sequence (P8-A3). It gives a total order PER AUTHOR on a
//     subject (the receiver can detect a gap / order within one author's stream). It is NOT a global
//     clock — two different authors' Seqs are incomparable.
//   - IdempotencyKey: "<AuthorID>:<Seq>" — carried NOW for the slice-8.5 durable dedup (it becomes
//     the JetStream Nats-Msg-Id + the consumer-side delivered-cursor compare). In 8.1 it is set on
//     every message and round-tripped, but no dedup runs yet (transient chan/tell are at-most-once).
//
// Body is the player-supplied text. It is DATA, never markup: the renderer substitutes it as a $t
// argument (P8-A7, text injection) — 8.1 only carries it; the sanitize-as-$t render lands with the
// channel format in 8.3.
type Message struct {
	Subject        string `json:"subject"`         // the bus subject this was published on (carried for the sink's dispatch)
	AuthorID       string `json:"author_id"`       // engine-set: the source world stamps it from the live *Entity (P8-A2)
	AuthorName     string `json:"author_name"`     // engine-set display name (P8-A2)
	Seq            uint64 `json:"seq"`             // monotonic per-author sequence (P8-A3)
	IdempotencyKey string `json:"idempotency_key"` // "<AuthorID>:<Seq>" — the 8.5 dedup key, carried now
	Body           string `json:"body"`            // the FULLY-RENDERED line the source world produced (format+color+$t); the gate writes it VERBATIM
	Text           string `json:"text,omitempty"`  // the RAW sanitized player message ($t DATA), carried so a rich sink can render its own per-channel line (#49); empty for a config/system message
}

// ConfigPayload is the per-player comms-config the SOURCE world publishes to ConfigSubject(player)
// (Phase 8.6) and the GATE applies. It carries the receiver-side filter state the gate enforces:
//
//   - HearChannels: the player's EFFECTIVE {enabled ∩ hearable} channel REF set — the channels the
//     gate should subscribe to (concrete ChanSubject(ref) each) and render. The world computes it
//     because the hear-access predicate needs the live *Entity + the channel_defs (both world-side);
//     the gate just subscribes the named subjects. A channel the player disabled OR cannot hear is
//     simply absent. The gate drops the chan.* wildcard and subscribes exactly this set (the edge-
//     preferred receiver HEAR-filter), so a restricted channel reaches only sockets whose world put it
//     in their hear-set (the content guardrail, now closed).
//   - Ignore: the receiver's ignore list — the gate caches it and drops EVERY inbound chan/tell/config-
//     less comms frame whose AuthorID is in it, at the single receiver-side funnel (P8-A6).
//
// It is transported as the Body of a Message published on ConfigSubject(player) (JSON-in-JSON), so the
// bus wire format stays uniform; MarshalConfig / UnmarshalConfig keep the encode/decode in one place.
type ConfigPayload struct {
	HearChannels []string `json:"hear_channels,omitempty"` // effective {enabled ∩ hearable} channel refs
	Ignore       []string `json:"ignore,omitempty"`        // receiver ignore list (author ids), funnel input
}

// MarshalConfig encodes a ConfigPayload to the Body string of a config Message. Errors are degenerate
// (the payload is plain slices of strings); a caller treats a marshal error as "skip this publish".
func MarshalConfig(p ConfigPayload) (string, error) {
	b, err := json.Marshal(p)
	return string(b), err
}

// UnmarshalConfig decodes a config Message Body back into a ConfigPayload. A malformed body is a handled
// error (the gate logs + ignores it), never a panic — the contentbus wire discipline.
func UnmarshalConfig(body string) (ConfigPayload, error) {
	var p ConfigPayload
	err := json.Unmarshal([]byte(body), &p)
	return p, err
}

// NewIdempotencyKey builds the canonical "<authorID>:<seq>" key. Kept in one place so the publish
// side and the future (8.5) durable dedup/cursor agree on the exact format.
func NewIdempotencyKey(authorID string, seq uint64) string {
	return authorID + ":" + itoa(seq)
}

// itoa avoids pulling strconv into the hot path's import set for a single use; identical semantics.
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// marshal/unmarshal keep the wire format in one place (the contentbus discipline) so the NATS bus,
// the MemBus, and the future JetStream path agree, and a malformed payload is a handled error rather
// than a subscriber-goroutine panic.
func (m Message) marshal() ([]byte, error) { return json.Marshal(m) }

func unmarshalMessage(data []byte) (Message, error) {
	var m Message
	err := json.Unmarshal(data, &m)
	return m, err
}

// Bus is the publish/subscribe contract for player comms. Two transports implement it (a NATS-backed
// bus for production, a MemBus for hermetic tests), mirroring the contentbus Bus/MemBus split so the
// whole comms feature is unit-testable with NO broker.
//
// A Bus handle carries a ROLE (P8-A2). The role gates Publish on chan/tell subjects: a RoleGate
// handle's Publish on a guarded subject returns ErrPublishForbidden — the structural impersonation
// gate. Subscribe is role-agnostic (both world and gate may subscribe; the gate is the normal
// subscriber).
type Bus interface {
	// Role reports this handle's publish capability (RoleWorld may publish chan/tell; RoleGate may
	// not). Surfaced so wiring + tests can assert the asymmetry.
	Role() Role

	// Publish broadcasts msg on subj to every subscriber whose subject (or wildcard) matches. It is
	// ACL-GUARDED: if this handle is RoleGate and subj is a chan/tell subject, it returns
	// ErrPublishForbidden and NOTHING reaches the wire (the impersonation gate). Off any zone
	// goroutine (the source world's publish path). A nil/disabled bus is a safe no-op (returns nil).
	Publish(ctx context.Context, subj string, msg Message) error

	// Subscribe registers handler for messages on subj, which MAY be a NATS-style wildcard
	// (telos.comms.chan.* or telos.comms.tell.*). The handler is called once per matching message on
	// a background goroutine the bus owns (never a zone goroutine), DELIVERED SERIALLY PER
	// SUBSCRIPTION so a single subject preserves publish order to that subscriber (P8-A3). Unsubscribe
	// stops delivery. A nil/disabled bus returns a no-op Subscription so callers need no nil-check.
	Subscribe(subj string, handler func(Message)) (Subscription, error)

	// Available reports whether the transport is currently usable for send/receive. A disabled bus
	// (broker down or unconfigured) is false; an in-process bus is always true; a NATS bus reflects its
	// LIVE connection (false during a disconnect/blip, true once reconnected). It is a point-in-time
	// probe — the gate reads it after login to warn a player when chat is offline. Safe from any goroutine.
	//
	// It is for surfacing STATUS to a human, NOT for control flow: do NOT gate Publish on it. Publish is
	// already never-fatal, and `if Available() { Publish() }` is a check-then-act race (the conn can drop
	// between the two). Just Publish — a down bus is a safe no-op.
	Available() bool

	// Close releases the bus. Idempotent.
	Close() error
}

// Subscription is a live subscription handle. Unsubscribe stops further delivery; idempotent and
// safe from any goroutine.
type Subscription interface {
	Unsubscribe() error
}

// wildcardMatches reports whether a published subject matches a subscription pattern. It supports the
// single trailing-token wildcard the taxonomy uses (telos.comms.chan.* matches telos.comms.chan.X
// for any single token X) plus exact match. It deliberately does NOT implement full NATS token
// wildcards (`*` mid-subject, `>` multi-token) — the taxonomy only needs trailing-`*` and exact, and
// keeping the matcher minimal avoids accidentally widening a gate's subscription surface. The real
// NATS bus uses the broker's own matcher; this is the MemBus's mirror of the subset we use.
func wildcardMatches(pattern, subject string) bool {
	if pattern == subject {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := pattern[:len(pattern)-1] // keep the trailing dot: "telos.comms.chan."
		if !strings.HasPrefix(subject, prefix) {
			return false
		}
		// Exactly one more token after the prefix (no further dots) — a single-token wildcard.
		rest := subject[len(prefix):]
		return rest != "" && !strings.Contains(rest, ".")
	}
	return false
}
