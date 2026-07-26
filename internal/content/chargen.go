package content

import (
	"fmt"
	"math/rand"
	"strconv"
)

// chargen.go — the pure, content-driven validation of a chargen submission (Phase 14.8b). It walks a
// ChargenDTO's steps and turns a player's raw choices into the applied RESULT (chosen bundle refs + chosen
// attribute base values) the world later applies on first spawn. No I/O, no engine — just the content rules,
// so it is reusable by the website (now) and any future client (telnet chargen). Adding a step KIND here is
// the one code change a new generation method needs; everything else is data.

// ChargenBundleOption is a selectable bundle the website renders for a bundle_choice step: its ref, its kind
// (the step filters by it), and a display label.
type ChargenBundleOption struct {
	Ref   string
	Kind  string
	Label string
}

// ValidateChargen checks a submission against flow and returns the applied result. picks maps a bundle_choice
// step id -> the chosen bundle ref; allocs maps a point_buy step id -> attribute -> chosen value; bundleKind
// maps a bundle ref -> its kind (the legality check, so a forged ref of the wrong kind is rejected). On a
// rule violation it returns a non-empty user-facing reason (bundles/attrs nil); a valid submission returns the
// chosen bundles + attribute base values with an empty reason.
// rng rolls a `roll` step's scores server-side (never a client value). It must be non-nil when the flow
// contains a roll step; callers that know their flow has none may pass nil.
func ValidateChargen(flow ChargenDTO, picks map[string]string, allocs map[string]map[string]int, bundleKind map[string]string, rng *rand.Rand) (bundles []string, attrs map[string]float64, reason string) {
	attrs = map[string]float64{}
	for _, st := range flow.Steps {
		switch st.Kind {
		case "bundle_choice":
			ref := picks[st.ID]
			if ref == "" {
				return nil, nil, fmt.Sprintf("Please choose a %s.", labelOr(st.BundleKind, "option"))
			}
			if bundleKind[ref] != st.BundleKind {
				return nil, nil, fmt.Sprintf("%q is not a valid %s.", ref, labelOr(st.BundleKind, "choice"))
			}
			bundles = append(bundles, ref)
		case "point_buy":
			spent := 0
			for _, a := range st.Attributes {
				v, ok := allocs[st.ID][a]
				if !ok {
					v = st.Base // an unsubmitted attribute sits at the base
				}
				if v < st.Min || v > st.Max {
					return nil, nil, fmt.Sprintf("%s must be between %d and %d.", a, st.Min, st.Max)
				}
				cost, ok := st.Cost[strconv.Itoa(v)]
				if !ok {
					return nil, nil, fmt.Sprintf("%d is not an allowed value for %s.", v, a)
				}
				spent += cost
				attrs[a] = float64(v)
			}
			if spent > st.Points {
				return nil, nil, fmt.Sprintf("That allocation costs %d of %d points.", spent, st.Points)
			}
		case "array_assign":
			// The player assigns the FIXED Array multiset across Attributes (via allocs, exactly like
			// point_buy). Accept iff the submitted values are a PERMUTATION of Array — each array value used
			// once. A remaining-count multiset both rejects an off-array value and enforces no-reuse.
			if len(st.Attributes) != len(st.Array) {
				return nil, nil, "This character sheet is misconfigured (array size)."
			}
			remaining := map[int]int{}
			for _, v := range st.Array {
				remaining[v]++
			}
			for _, a := range st.Attributes {
				v, ok := allocs[st.ID][a]
				if !ok {
					return nil, nil, fmt.Sprintf("Assign a value to %s.", a)
				}
				if remaining[v] <= 0 {
					return nil, nil, fmt.Sprintf("%d is not an available value for %s.", v, a)
				}
				remaining[v]--
				attrs[a] = float64(v)
			}
		case "roll":
			// Each attribute gets an INDEPENDENT server-rolled score (4d6-drop-lowest by default). The roll
			// is authoritative — rng is the server's, run here at submit — so a roll step takes no client
			// input and cannot be forged. A nil rng on a roll step is a caller bug, reported not panicked.
			if rng == nil {
				return nil, nil, "This world cannot roll ability scores right now."
			}
			for _, a := range st.Attributes {
				score, err := rollAbilityScore(rng, st.RollDice)
				if err != nil {
					return nil, nil, "This character sheet is misconfigured (roll dice)."
				}
				attrs[a] = float64(score)
			}
		default:
			return nil, nil, "Unsupported chargen step: " + st.Kind
		}
	}
	return bundles, attrs, ""
}

func labelOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
