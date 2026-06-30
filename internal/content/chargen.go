package content

import (
	"fmt"
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
func ValidateChargen(flow ChargenDTO, picks map[string]string, allocs map[string]map[string]int, bundleKind map[string]string) (bundles []string, attrs map[string]float64, reason string) {
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
