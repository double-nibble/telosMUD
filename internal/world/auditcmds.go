package world

import (
	"context"
	"sort"
	"strconv"
	"strings"
)

// auditcmds.go holds the #350 read surface: the `audit` command over the durable audit trail. It has two
// forms with two access levels, deliberately gated INSIDE the handler rather than by a whole-command
// MinRank (which would hide even the self-view from a mortal):
//
//   - `audit` (no arg): the caller's OWN permanent-change history, available to ANY tier. Scoped in the
//     store to subject_id = the caller's STABLE character pid (not the mutable name) — a player can only
//     ever see their own trail, and a future rename/purge can't replay a prior owner's history to a name.
//   - `audit <name>`: another character's trail, STAFF-ONLY. A mortal who passes a name is refused (the
//     rank check below), so the named lookup never reaches the store for a non-staff caller.
//
// Account tier_changed rows (subject_id = the ACCOUNT uuid, subject_name NULL) appear in neither the
// pid-scoped self-view nor the name-scoped character read. The STAFF `audit <name>` view now ALSO fetches
// the account's tier history (ListAccountTierAudit resolves name -> characters.account_id -> the account's
// tier rows) and merges it, newest-first, so an account's tier changes are reachable through a character
// (#399 item 4). The mortal self-view stays character-scoped by design (tier history is a staff concern).
//
// The store call is blocking pool I/O, so — like mailList — the handler rate-limits + spawns a short-lived
// goroutine that queries off the zone goroutine and writes straight to the session out channel. A nil
// sink (a storeless / bare-engine shard) degrades to "Audit is unavailable." for every form.

// auditListLimit caps how many trail rows one `audit` invocation returns (newest-first). A modest cap
// keeps the render readable over telnet and bounds the query — the full history is a DB concern, not a
// single command's job. A package var so a test can shrink it.
var auditListLimit = 20

// auditCommands returns the #350 audit command set, appended to the base table (registerCommands). It is
// registered low-priority (with the comms/mail commands) so it never shadows or abbreviates a movement/
// look/say verb. NOT MinRank-gated on the whole command: the no-arg self-view is a mortal-facing feature,
// and cmdAudit gates the named-subject form to staff internally.
func auditCommands() []*Command {
	return []*Command{
		{Name: "audit", Run: cmdAudit},
	}
}

// cmdAudit dispatches the two audit forms. Bare `audit` renders the caller's own trail (any tier);
// `audit <name>` renders another character's trail (staff only — a mortal is refused before the store is
// touched). A storeless shard reports "Audit is unavailable." Never releases ownership, so dispatch
// prompts on return.
func cmdAudit(c *Context) error {
	z, s := c.z, c.s
	if z.auditSink() == nil {
		c.Send("Audit is unavailable.")
		return nil
	}
	arg := strings.TrimSpace(c.Rest())

	// Resolve the target + the access decision ON the zone goroutine (single-writer reads of s.tier /
	// s.character / s.entity.pid), BEFORE any goroutine or store call — so a refused mortal never spends a
	// token or hits PG. The read fn is bound here so the two forms differ only in HOW they scope the query.
	label := s.character
	var query func(ctx context.Context, sink AuditSink) ([]AuditEntry, error)
	if arg == "" {
		// SELF-VIEW: the caller's own trail, scoped by the STABLE character pid (not the name). Name is a
		// mutable, potentially-reusable key — an append-only trail outlives a future rename/purge, so a
		// name-scoped self-view could replay a prior owner's history; the pid can't be reused. A session
		// with no character identity yet (pid nil during the async-create window) has no trail to show.
		pid := s.entity.pid
		if pid == nil {
			c.Send("You have no audit history yet.")
			return nil
		}
		subjectID := string(*pid)
		query = func(ctx context.Context, sink AuditSink) ([]AuditEntry, error) {
			return sink.ListAuditForSubject(ctx, subjectID, auditListLimit)
		}
	} else {
		// A NAMED lookup is staff-only. Resolve the caller's tier to a rank via the content ladder; a
		// mortal (rank 0) is refused here — the store is never queried for another subject on their behalf.
		if z.trustLadder().rank(s.tier) < rankStaff {
			c.Send("You may only view your own audit history.")
			return nil
		}
		label = arg
		query = func(ctx context.Context, sink AuditSink) ([]AuditEntry, error) {
			// The character's own trail PLUS the owning account's tier history (#399 item 4), merged
			// newest-first and capped — so `audit <name>` surfaces an account tier change even though its
			// row is account-subject (invisible to the by-name character read).
			charRows, err := sink.ListAuditForCharacterName(ctx, label, auditListLimit)
			if err != nil {
				return nil, err
			}
			tierRows, err := sink.ListAccountTierAudit(ctx, label, auditListLimit)
			if err != nil {
				return nil, err
			}
			return mergeAuditNewestFirst(charRows, tierRows, auditListLimit), nil
		}
	}

	// RATE-LIMIT the async-PG read on the same per-author comms bucket mail/who use (it spawns a goroutine
	// + a Postgres query — the cheapest unbounded async-I/O path otherwise). Enforced on the zone goroutine
	// BEFORE the goroutine spawns, so a throttled invocation never touches the store.
	if !z.commRateOK(s.character) {
		c.Send("You are checking the audit log too fast.")
		return nil
	}

	sink := z.auditSink()
	out := s.out
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), auditIOTimeout)
		defer cancel()
		entries, err := query(ctx, sink)
		if err != nil {
			writeFrameTo(out, textFrame("Audit is unavailable."))
			return
		}
		writeFrameTo(out, textFrame(renderAuditTrail(label, entries)))
	}()
	return nil
}

// mergeAuditNewestFirst concatenates two audit slices (the character trail + the account tier trail),
// sorts the union newest-first, and caps it at limit (#399 item 4). Each input is already ≤limit, so the
// union is ≤2*limit; capping after the merge keeps the newest `limit` across BOTH sources rather than
// silently favoring one. A stable sort preserves each source's own tie order within a same-`at` instant.
//
// REACHABILITY CAVEAT: because the merged view is a newest-first page of `limit` rows, a rare account tier
// change is reachable through a character only while it stays within that newest window — if the character
// accrues ≥limit audit events NEWER than the tier change, the tier row is paged out. Acceptable for a
// staff read (tier changes are rare and this is a paged view, not an exhaustive account report); a
// dedicated account-history read would be the answer if absolute reachability is ever needed.
func mergeAuditNewestFirst(a, b []AuditEntry, limit int) []AuditEntry {
	merged := make([]AuditEntry, 0, len(a)+len(b))
	merged = append(merged, a...)
	merged = append(merged, b...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].At.After(merged[j].At) })
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// renderAuditTrail formats a character's audit trail newest-first: a header + one line per event (time,
// kind, actor, and a short payload summary). An empty trail is a friendly notice. The entries arrive
// newest-first from the store; this re-sorts defensively so the render order is independent of the
// backend's tie-break.
func renderAuditTrail(name string, entries []AuditEntry) string {
	if len(entries) == 0 {
		return "No audit history for " + name + "."
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].At.After(entries[j].At) })
	var b strings.Builder
	b.WriteString("Audit history for ")
	b.WriteString(name)
	b.WriteString(":")
	for _, e := range entries {
		b.WriteByte('\n')
		b.WriteString("  ")
		b.WriteString(e.At.UTC().Format("2006-01-02 15:04 UTC"))
		b.WriteString("  ")
		b.WriteString(e.EventKind)
		if actor := auditActorLabel(e); actor != "" {
			b.WriteString("  by ")
			b.WriteString(actor)
		}
		if summary := auditPayloadSummary(e); summary != "" {
			b.WriteString("  (")
			b.WriteString(summary)
			b.WriteByte(')')
		}
	}
	return b.String()
}

// auditActorLabel renders the "by <actor>" clause: the acting character's name for a death by another
// player (from the payload), else the actor type ("system"), else "" when there is no actor. It never
// surfaces a raw UUID — an id is not human-meaningful in a staff read.
func auditActorLabel(e AuditEntry) string {
	// A player kill carries the killer_name in the payload; prefer it over the opaque actor_id.
	if e.EventKind == AuditKindDied {
		if kn, _ := e.Payload["killer_name"].(string); kn != "" {
			return kn
		}
	}
	switch e.ActorType {
	case AuditActorSystem:
		return "system"
	case "":
		return ""
	default:
		return e.ActorType
	}
}

// auditPayloadSummary renders a short, kind-specific one-liner from the payload (the "v" version field is
// omitted — it is a schema detail, not staff-facing). Unknown kinds fall through to a generic key=val
// dump so a future event kind is still legible before its own case is added.
func auditPayloadSummary(e AuditEntry) string {
	switch e.EventKind {
	case AuditKindDied:
		if room, _ := e.Payload["room_ref"].(string); room != "" {
			return "in " + room
		}
		return ""
	case AuditKindAttributeBase:
		attr, _ := e.Payload["attr"].(string)
		return attr + " " + auditNum(e.Payload["old"]) + " -> " + auditNum(e.Payload["new"])
	case AuditKindTrackAdvanced:
		track, _ := e.Payload["track"].(string)
		return track + " step " + auditNum(e.Payload["step"])
	case AuditKindTierChanged:
		return auditTierLabel(e.Payload["old_tier"]) + " -> " + auditTierLabel(e.Payload["new_tier"])
	case AuditKindCharacterCreated:
		if room, _ := e.Payload["room"].(string); room != "" {
			return "in " + room
		}
		return ""
	default:
		return genericPayloadSummary(e.Payload)
	}
}

// auditNum renders a numeric payload value compactly. JSON round-trips numbers as float64, so an integral
// value prints without a trailing ".0"; a non-numeric value falls back to its default string form.
func auditNum(v any) string {
	switch n := v.(type) {
	case float64:
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'g', -1, 64)
	case int:
		return strconv.Itoa(n)
	case nil:
		return "?"
	default:
		return ""
	}
}

// auditTierLabel renders a tier payload value: the tier string, or "(none)" for a null (the initial
// grant's old_tier).
func auditTierLabel(v any) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return "(none)"
}

// genericPayloadSummary dumps unknown payloads as sorted key=val pairs (minus the schema "v"), so a newly
// added event kind is still readable before renderAuditTrail gains a dedicated case.
func genericPayloadSummary(payload map[string]any) string {
	keys := make([]string, 0, len(payload))
	for k := range payload {
		if k == "v" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+auditValString(payload[k]))
	}
	return strings.Join(parts, " ")
}

// auditValString renders an arbitrary payload value for the generic dump.
func auditValString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return auditNum(x)
	case nil:
		return "null"
	default:
		return ""
	}
}
