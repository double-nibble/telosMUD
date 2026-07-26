package content

import "fmt"

// chargen_lint.go — boot/reload validation of chargen flow step config (#518). A mis-authored step surfaces
// only when a PLAYER hits it (a generic "misconfigured" refusal at submit, or — for an array whose size
// doesn't match its attributes — a stuck gate prompt), so this names the offending flow/step at LOAD instead,
// the same operator-signal role LintFinite plays for attribute literals. Non-fatal at boot (logged); the
// reload gate can reject on it.

// ChargenViolation is one mis-authored chargen step.
type ChargenViolation struct {
	Pack  string
	Ref   string // the chargen flow ref
	Step  string // the step id
	Field string // the offending field ("array" | "roll_dice")
	Msg   string // a human-readable reason
}

// LintChargen validates each chargen flow's steps: an array_assign must have exactly as many array values as
// attributes to assign (a mismatch can't be completed), and a roll step's RollDice must parse.
func LintChargen(packs []Pack) []ChargenViolation {
	var out []ChargenViolation
	for _, p := range packs {
		for _, flow := range p.Chargens {
			for _, st := range flow.Steps {
				switch st.Kind {
				case "array_assign":
					if len(st.Attributes) != len(st.Array) {
						out = append(out, ChargenViolation{
							Pack: p.Pack, Ref: flow.Ref, Step: st.ID, Field: "array",
							Msg: fmt.Sprintf("array has %d values but %d attributes to assign; they must match",
								len(st.Array), len(st.Attributes)),
						})
					}
				case "roll":
					if _, _, _, err := parseRollSpec(st.RollDice); err != nil {
						out = append(out, ChargenViolation{
							Pack: p.Pack, Ref: flow.Ref, Step: st.ID, Field: "roll_dice", Msg: err.Error(),
						})
					}
				}
			}
		}
	}
	return out
}
