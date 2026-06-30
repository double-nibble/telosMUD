package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
)

// chargen.go — the website's character-generation page (Phase 14.8b). It renders the content-driven flow
// (a race/class picker + a point-buy allocator) and posts the submission to the account Service, which
// validates it server-side and creates the character with its first-spawn marker. The form is built entirely
// from the content flow, so adding a race/class — or switching the whole method to a standard-array or
// roll-stats flow — is a content change with no code change here.

// chargenStepView is one rendered step (a tagged union over Kind, like the DTO).
type chargenStepView struct {
	ID, Kind, Prompt string
	Options          []content.ChargenBundleOption // bundle_choice: the selectable bundles of this step's kind
	Attributes       []string                      // point_buy: the attributes to allocate
	Points, Base     int
	Min, Max         int
}

// buildSteps turns the content flow into the render model, filtering each bundle_choice step's options to its
// kind.
func buildSteps(flow content.ChargenDTO, options []content.ChargenBundleOption) []chargenStepView {
	steps := make([]chargenStepView, 0, len(flow.Steps))
	for _, st := range flow.Steps {
		v := chargenStepView{ID: st.ID, Kind: st.Kind, Prompt: st.Prompt}
		switch st.Kind {
		case "bundle_choice":
			for _, o := range options {
				if o.Kind == st.BundleKind {
					v.Options = append(v.Options, o)
				}
			}
		case "point_buy":
			v.Attributes = st.Attributes
			v.Points, v.Base, v.Min, v.Max = st.Points, st.Base, st.Min, st.Max
		}
		steps = append(steps, v)
	}
	return steps
}

func (s *Server) handleChargenForm(w http.ResponseWriter, r *http.Request) {
	account := s.sessionAccount(r)
	if account == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if s.chargen == nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	flow, options, ok := s.chargen.ChargenFlow()
	if !ok {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.render(w, "chargen", map[string]any{"Steps": buildSteps(flow, options), "Name": "", "Error": ""})
}

func (s *Server) handleChargenCreate(w http.ResponseWriter, r *http.Request) {
	account := s.sessionAccount(r)
	if account == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if s.chargen == nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	flow, options, _ := s.chargen.ChargenFlow()
	name := r.FormValue("name")
	picks, allocs := parseChargenSubmission(flow, r)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, reason, err := s.chargen.BuildCharacter(ctx, account, name, picks, allocs)
	if err != nil {
		s.fail(w, "chargen create", err)
		return
	}
	if reason != "" {
		// Re-render the form with the validation message + the submitted name preserved.
		w.WriteHeader(http.StatusOK)
		s.render(w, "chargen", map[string]any{"Steps": buildSteps(flow, options), "Name": name, "Error": reason})
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// parseChargenSubmission reads the posted form into the picks (bundle_choice step id -> chosen ref) and allocs
// (point_buy step id -> attribute -> value) the validator expects. A point_buy field is named "<stepID>_<attr>".
func parseChargenSubmission(flow content.ChargenDTO, r *http.Request) (picks map[string]string, allocs map[string]map[string]int) {
	picks = map[string]string{}
	allocs = map[string]map[string]int{}
	for _, st := range flow.Steps {
		switch st.Kind {
		case "bundle_choice":
			if v := r.FormValue(st.ID); v != "" {
				picks[st.ID] = v
			}
		case "point_buy":
			m := map[string]int{}
			for _, a := range st.Attributes {
				if v, err := strconv.Atoi(r.FormValue(st.ID + "_" + a)); err == nil {
					m[a] = v
				}
			}
			allocs[st.ID] = m
		}
	}
	return picks, allocs
}
