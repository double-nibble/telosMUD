package world

import (
	"context"
	"strings"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/textsan"
)

// channel.go is the SOURCE-WORLD half of Phase-8 channels (docs/PHASE8-PLAN.md slice 8.3, P8-D3):
// channels as CONTENT (channel_defs) + the channel verbs + the publish path. The engine names NO
// channel — `gossip`/`newbie` are demo CONTENT; the engine only knows the channel_def shape and the
// commbus transport. An empty pack ships zero channel_defs, so there are NO channel verbs and the
// empty-boot invariant holds.
//
// # The publish path = the world is the SOURCE (P8-D1)
//
// A channel verb (`gossip hi`) reaches the zone goroutine as ordinary input and dispatches here. The
// world is the message SOURCE: it holds the authoritative author identity (the live *Entity) and the
// content rules, so it — and ONLY it — publishes to telos.comms.chan.<ref>. The gate is the SINK (it
// renders); it never authors a channel line (the commbus RoleGate ACL — the impersonation gate). The
// world does NOT deliver to anyone's socket directly, not even co-located players: everyone (local
// AND cross-shard) receives via the bus, so there is exactly ONE delivery path and no double-render.
//
// # The five security obligations (carried forward from the 8.1 review — every one enforced HERE)
//
//  1. REF-VALIDATE (P8-A8): the channel is resolved from the loaded channel_defs (channelForVerb);
//     a verb with no def is simply not a channel verb (dispatch falls through to "Huh?"). The subject
//     is built from the def's KNOWN ref via commbus.ChanSubject — never from free-form client text
//     (ChanSubject does NO validation). There is no path by which client input names the subject.
//  2. ACCESS (P8-A8): channelDef.canSpeak is evaluated against the speaking *Entity BEFORE publish; a
//     speaker who fails the content predicate is refused with a message and nothing reaches the bus.
//  3. RATE-LIMIT (P8-A1): a per-author token bucket throttles the SENDER ONLY; an over-limit line is
//     dropped with a "you are doing that too much" to the speaker, never degrading other players.
//  4. SANITIZE (P8-A7): the player's text is cleaned (textsan) and rendered as the $t DATA arg of the
//     channel format, so a `$`/`%`/ANSI/IAC in it is LITERAL — a player cannot forge a "[gossip]
//     Admin:" prefix or inject a control sequence. The trusted format template comes from content.
//  5. ENGINE-SET AUTHOR (P8-A2): AuthorID/AuthorName are stamped from the LIVE resolved *Entity, never
//     from any client field; Seq comes from a server-held monotonic per-author counter (Shard.commSeq).
//     The ACL stops a GATE publishing; this stamping is the half the ACL can't enforce — a world
//     publishing a badly-sourced author — so it lives here, on the source path.

// channelDef is the runtime form of a content.ChannelDTO (P8-D3): a content-defined comms channel.
// Immutable after build — shared read-only across zone goroutines via the registry, exactly like a
// *abilityDef/*affectDef — and swapped wholesale by the hot-reload path (defRegistry.reload). The
// publish path reads it (canSpeak/render); the gate never sees it (the world renders into Body).
type channelDef struct {
	ref    string
	name   string
	words  []string // command verbs that emit on this channel (lower-cased, registered exact-only)
	color  string   // color/markup token prepended to the rendered line (content; empty => none)
	format string   // listener-perspective template ("[$channel] $name: $t"); empty => defaultChannelFormat
	// history is the recent-lines buffer size (carried; retrieval deferred — P8-D3). When retrieval
	// lands it MUST gate fetches on canHear (the split hear predicate) AT FETCH TIME — a player who
	// lost hear access must not replay lines from when they had it.
	history   int
	defaultOn bool // is a fresh character subscribed by default (drives the hear-set vs a toggle, 8.6)

	// access is the parsed SPEAK predicate (P8-A8). A zero predicate (no conditions) is the open
	// channel — anyone may speak. canSpeak evaluates it against the live *Entity.
	access channelAccess
	// hearAccess is the parsed LISTEN predicate when content SPLITS it from access (hear_access in the
	// channel_def). nil => hear mirrors the speak predicate (the v1 rule); non-nil (even zero — the
	// "announce" shape) => canHear evaluates THIS predicate instead.
	hearAccess *channelAccess
}

// channelAccess is the parsed access predicate (P8-A8). All present conditions AND together; a zero
// value (no conditions) admits everyone. It is DATA only — a required flag, an attribute floor — read
// against the live entity; the engine names no rule, the content does.
type channelAccess struct {
	requireFlag string // a named entity flag the speaker must carry; "" => no flag requirement
	minAttrName string // an attribute the speaker must have >= minAttrVal; "" => no attr floor
	minAttrVal  float64
}

// defaultChannelFormat is the listener-perspective template used when a channel_def supplies no
// `format`. It renders "[<channel name>] <author>: <text>" with $-substitution: $channel = the
// channel display name, $name = the ENGINE-SET author, $t = the player's text (sanitized DATA).
const defaultChannelFormat = "[$channel] $name: $t"

// buildChannelDef maps a content.ChannelDTO onto the runtime channelDef (defineGlobals / hot reload).
// It lower-cases the verb words (dispatch lower-cases the typed verb before lookup), parses the access
// predicate, and defaults an empty format to defaultChannelFormat. Build-time only; immutable result.
func buildChannelDef(c content.ChannelDTO) *channelDef {
	words := make([]string, 0, len(c.Words))
	for _, w := range c.Words {
		lw := strings.ToLower(strings.TrimSpace(w))
		if lw != "" {
			words = append(words, lw)
		}
	}
	format := c.Format
	if format == "" {
		format = defaultChannelFormat
	}
	def := &channelDef{
		ref:       c.Ref,
		name:      c.Name,
		words:     words,
		color:     c.Color,
		format:    format,
		history:   c.History,
		defaultOn: c.DefaultOn,
	}
	def.access = parseChannelAccess(c.Access)
	if c.HearAccess != nil {
		ha := parseChannelAccess(*c.HearAccess)
		def.hearAccess = &ha
	}
	return def
}

// parseChannelAccess maps a content access predicate onto its runtime form (shared by the speak and
// the optional split hear predicate).
func parseChannelAccess(a content.ChannelAccessDTO) channelAccess {
	var out channelAccess
	out.requireFlag = a.RequireFlag
	if a.MinAttr != nil && a.MinAttr.Attr != "" {
		out.minAttrName = a.MinAttr.Attr
		out.minAttrVal = a.MinAttr.Min
	}
	return out
}

// meets evaluates one parsed predicate against a live entity: all present conditions AND together, a
// zero predicate admits everyone, nil entity refused (defensive). Zone-goroutine only.
func (a *channelAccess) meets(e *Entity) bool {
	if e == nil {
		return false
	}
	if a.requireFlag != "" && !hasFlag(e, a.requireFlag) {
		return false
	}
	if a.minAttrName != "" && attr(e, a.minAttrName) < a.minAttrVal {
		return false
	}
	return true
}

// canSpeak evaluates the channel's access predicate against the speaking entity (P8-A8). It is the
// authoritative speak gate — the source world owns the live *Entity, so this is the one place the
// content rule is checked, never the client. A nil entity (defensive) is refused. An open channel (no
// conditions) admits anyone. Pure read of the immutable def + the live entity; zone-goroutine only.
func (d *channelDef) canSpeak(e *Entity) bool {
	return d.access.meets(e)
}

// canHear evaluates the channel's LISTEN predicate against the LISTENING entity (Phase 8.6, the
// receiver HEAR-filter). When content supplies a split hear_access, THAT predicate rules (a zero one
// admits everyone — the "announce" channel: restricted speak, open hear); otherwise hear mirrors the
// speak predicate (the v1 rule — a restricted channel restricts both directions). The world evaluates
// it against the live *Entity (zone-goroutine) when computing the player's effective hear-set
// (effectiveHearSet); the gate never runs it (it has no content) — it just subscribes the refs the
// world put in the hear-set. A nil entity is refused (defensive, in meets).
func (d *channelDef) canHear(e *Entity) bool {
	return d.hearPredicate().meets(e)
}

// hearPredicate returns the EFFECTIVE listen predicate: the split hear_access when content supplies
// one, else the speak predicate (the v1 mirror rule). The single dispatch point — canHear and the
// anyChannelGatesHearing republish guard both use it, so they cannot disagree on which predicate
// gates hearing.
func (d *channelDef) hearPredicate() *channelAccess {
	if d.hearAccess != nil {
		return d.hearAccess
	}
	return &d.access
}

// renderLine produces the FULLY-rendered channel line the gate will write verbatim (P8-A7). The world
// renders here — not the gate — because the format/color are CONTENT the world holds; the gate is a
// dumb sink (it has no content, and this keeps the engine=mechanism / content=flavor split: the gate
// never names a channel). text is the player's message, already SANITIZED by the caller. It is
// substituted as the $t DATA arg by a single-pass scanner that treats a `$`/`%` in $t (and in $name —
// the author display name) literally, NEVER re-scanning a substituted value for tokens — so a player
// cannot forge a "[gossip] Admin:" prefix. The color token (content) wraps the whole line.
func (d *channelDef) renderLine(authorName, text string) string {
	body := renderChannelFormat(d.format, d.name, authorName, text)
	if d.color != "" {
		return d.color + body
	}
	return body
}

// renderChannelFormat substitutes the channel template's $-tokens in a SINGLE pass. Recognized tokens:
// $channel (the channel display name — trusted content), $name (the engine-set author display name),
// $t (the player's sanitized text), $$ (a literal '$'). EVERY substituted value is copied verbatim and
// NEVER re-scanned, so a `$`/`%` inside $name or $t is data, never a token or a format directive (the
// act() render precedent, act.go — there is no fmt.Sprintf path and no way for player content to
// become a template). An unknown token is emitted literally so a stray '$' surfaces in review.
func renderChannelFormat(tmpl, channelName, authorName, text string) string {
	var b strings.Builder
	b.Grow(len(tmpl) + len(channelName) + len(authorName) + len(text) + 8)
	for i := 0; i < len(tmpl); i++ {
		c := tmpl[i]
		if c != '$' || i+1 >= len(tmpl) {
			b.WriteByte(c)
			continue
		}
		// Match the longest known token name first ($channel before any single letter).
		rest := tmpl[i+1:]
		switch {
		case strings.HasPrefix(rest, "channel"):
			b.WriteString(channelName)
			i += len("channel")
		case rest[0] == 'n' && strings.HasPrefix(rest, "name"):
			b.WriteString(authorName)
			i += len("name")
		case rest[0] == 't':
			b.WriteString(text)
			i++
		case rest[0] == '$':
			b.WriteByte('$')
			i++
		default:
			// Unknown token: emit the '$' literally and let the next byte be scanned normally.
			b.WriteByte('$')
		}
	}
	return b.String()
}

// cmdChannel is the channel-verb handler (the SOURCE publish path). It runs on the zone goroutine via
// dispatch, with def the channel resolved from the typed verb (channelForVerb — the ref is already
// validated against the loaded channel_defs, P8-A8). It enforces, IN ORDER, the five security
// obligations above and then publishes the engine-set, fully-rendered line to telos.comms.chan.<ref>.
//
// It never delivers to a socket directly: every recipient (co-located AND cross-shard) receives via
// the bus, so there is exactly one delivery path. With comms unavailable (NATS down => a disabled bus)
// Publish is a clean no-op and the speaker is told comms are offline — never a crash (the never-fatal
// rule). No prompt is sent here; dispatch prompts on return.
func (z *Zone) cmdChannel(s *session, def *channelDef, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		s.send(textFrame("Say what on " + def.name + "?"))
		return
	}
	actor := s.entity

	// (2) ACCESS (P8-A8): refuse a speaker who fails the content predicate. Evaluated against the LIVE
	// entity — never the client.
	if !def.canSpeak(actor) {
		s.send(textFrame("You can't speak on " + def.name + "."))
		return
	}

	// (3) RATE-LIMIT (P8-A1): a per-author token bucket. Over-limit throttles the SENDER ONLY — the
	// line is dropped before it ever reaches the bus, so other players' delivery is untouched.
	if !z.commRateOK(s.character) {
		s.send(textFrame("You are doing that too much; slow down."))
		return
	}

	bus := z.commsBus()
	if bus == nil {
		// Defensive: a zone with no shard (a bare unit test). Treat as comms-unavailable.
		s.send(textFrame("Channels are temporarily offline."))
		return
	}

	// (4) SANITIZE (P8-A7): clean the player's text so control bytes / ANSI / IAC are stripped before it
	// becomes the $t DATA arg. CleanLine is the same world-ingress sanitizer the input path uses; the
	// gRPC ingress already cleaned it, this is defense-in-depth at the comms boundary specifically.
	safe := textsan.CleanLine(text)

	// (5) ENGINE-SET AUTHOR (P8-A2): the author is the LIVE resolved entity — its display name — NEVER a
	// client field. Seq is a server-held monotonic per-author counter. The fully-rendered line (format +
	// color + sanitized $t) is the Body the gate writes verbatim.
	authorID := s.character
	authorName := actor.Name()
	seq := z.commNextSeq(authorID)
	line := def.renderLine(authorName, safe)

	msg := commbus.Message{
		AuthorID:       authorID,
		AuthorName:     authorName,
		Seq:            seq,
		IdempotencyKey: commbus.NewIdempotencyKey(authorID, seq),
		Body:           line,
		Text:           safe, // RAW sanitized player text (#49): the gate's GMCP mirror carries it so a rich client can render its own per-channel line
	}

	// (1) REF-VALIDATE'd subject: built from the def's KNOWN ref (commbus.ChanSubject does no validation;
	// the ref came from the loaded channel_defs, never from client text — P8-A8).
	subj := commbus.ChanSubject(def.ref)
	if err := bus.Publish(context.Background(), subj, msg); err != nil {
		// A publish failure (e.g. a closed/disabled bus) is never fatal: tell the speaker comms are
		// offline and continue. The RoleGate ACL can't bite here — the world handle is RoleWorld.
		z.log.Debug("channel publish failed", "channel", def.ref, "player", authorID, "err", err)
		s.send(textFrame("Channels are temporarily offline."))
		return
	}
	z.log.Debug("channel published", "channel", def.ref, "author", authorID, "seq", seq)
}
