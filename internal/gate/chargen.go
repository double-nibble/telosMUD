package gate

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/double-nibble/telosmud/internal/telnet"
)

// chargen.go — Phase 15.4 prompt-driven character select + creation. After the device login authes, the player
// picks an existing character or creates one by walking the CONTENT chargen flow as telnet prompts. The content
// drives the prompts; the account validates the submission + applies the build on first spawn (the engine is
// unchanged from the old web flow — only the renderer is the terminal now).

const chargenCallTimeout = 5 * time.Minute // per network call to account during select/create

// selectOrCreateCharacter runs character select/create after a successful login. It returns the chosen (or
// freshly created) character name, or ok=false if the player disconnects/aborts.
func (s *Server) selectOrCreateCharacter(ctx context.Context, tc *telnet.Conn, log *slog.Logger, account string, chars []CharacterInfo) (string, bool) {
	if len(chars) == 0 {
		_ = tc.Write("\r\nYou have no characters yet. Let's create one.\r\n")
		return s.runChargen(ctx, tc, log, account)
	}
	for {
		_ = tc.Write("\r\nChoose a character:\r\n")
		for i, c := range chars {
			_ = tc.Write(fmt.Sprintf("  %d) %s\r\n", i+1, c.Name))
		}
		createOpt := len(chars) + 1
		_ = tc.Write(fmt.Sprintf("  %d) Create a new character\r\n> ", createOpt))
		line, err := tc.ReadLine()
		if err != nil {
			return "", false
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > createOpt {
			_ = tc.Write("\r\nPick a number from the list.\r\n")
			continue
		}
		if n == createOpt {
			if name, ok := s.runChargen(ctx, tc, log, account); ok {
				return name, true
			}
			continue // creation aborted: back to the menu
		}
		return chars[n-1].Name, true
	}
}

// runChargen walks the content chargen flow as prompts and creates the character. The whole flow re-runs on a
// create rejection (a name taken / over-budget allocation / at the cap) so the player can correct any choice.
func (s *Server) runChargen(ctx context.Context, tc *telnet.Conn, log *slog.Logger, account string) (string, bool) {
	fctx, cancel := context.WithTimeout(ctx, chargenCallTimeout)
	configured, steps, options, err := s.account.GetChargenFlow(fctx)
	cancel()
	if err != nil || !configured {
		_ = tc.Write("\r\nCharacter creation isn't available right now.\r\n")
		return "", false
	}
	for {
		picks := map[string]string{}
		allocs := map[string]map[string]int{}
		if !walkSteps(tc, steps, options, picks, allocs) {
			return "", false // disconnected mid-flow
		}
		name, ok := promptName(tc)
		if !ok {
			return "", false
		}
		cctx, c := context.WithTimeout(ctx, chargenCallTimeout)
		_, reason, err := s.account.CreateChargenCharacter(cctx, account, name, picks, allocs)
		c()
		if err != nil {
			log.Warn("CreateChargenCharacter failed", "err", err)
			_ = tc.Write("\r\nCharacter creation is unavailable right now.\r\n")
			return "", false
		}
		if reason != "" {
			_ = tc.Write("\r\n" + reason + " Let's try again.\r\n")
			continue // re-run the whole flow so any choice can be corrected
		}
		_ = tc.Write("\r\nCharacter created!\r\n")
		return name, true
	}
}

// walkSteps prompts each chargen step, filling picks (bundle_choice) + allocs (point_buy). Returns false on a
// disconnect.
func walkSteps(tc *telnet.Conn, steps []ChargenStep, options []ChargenBundleOption, picks map[string]string, allocs map[string]map[string]int) bool {
	for _, step := range steps {
		switch step.Kind {
		case "bundle_choice":
			ref, ok := promptBundleChoice(tc, step, options)
			if !ok {
				return false
			}
			picks[step.ID] = ref
		case "point_buy":
			vals, ok := promptPointBuy(tc, step)
			if !ok {
				return false
			}
			allocs[step.ID] = vals
		}
	}
	return true
}

func promptBundleChoice(tc *telnet.Conn, step ChargenStep, options []ChargenBundleOption) (string, bool) {
	var opts []ChargenBundleOption
	for _, o := range options {
		if o.Kind == step.BundleKind {
			opts = append(opts, o)
		}
	}
	if len(opts) == 0 {
		return "", true // nothing to choose (the validator will flag a required-but-empty step)
	}
	for {
		if step.Prompt != "" {
			_ = tc.Write("\r\n" + step.Prompt + "\r\n")
		}
		for i, o := range opts {
			_ = tc.Write(fmt.Sprintf("  %d) %s\r\n", i+1, o.Label))
		}
		_ = tc.Write("> ")
		line, err := tc.ReadLine()
		if err != nil {
			return "", false
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > len(opts) {
			_ = tc.Write("\r\nPick a number from the list.\r\n")
			continue
		}
		return opts[n-1].Ref, true
	}
}

func promptPointBuy(tc *telnet.Conn, step ChargenStep) (map[string]int, bool) {
	vals := map[string]int{}
	if step.Prompt != "" {
		_ = tc.Write("\r\n" + step.Prompt + "\r\n")
	}
	_ = tc.Write(fmt.Sprintf("(%d points to spend; each value %d-%d; press Enter for the default)\r\n", step.Points, step.Min, step.Max))
	for _, attr := range step.Attributes {
		for {
			_ = tc.Write(fmt.Sprintf("  %s [%d]: ", attr, step.Base))
			line, err := tc.ReadLine()
			if err != nil {
				return nil, false
			}
			t := strings.TrimSpace(line)
			if t == "" {
				vals[attr] = step.Base
				break
			}
			n, err := strconv.Atoi(t)
			if err != nil || n < step.Min || n > step.Max {
				_ = tc.Write(fmt.Sprintf("    Enter a number between %d and %d.\r\n", step.Min, step.Max))
				continue
			}
			vals[attr] = n
			break
		}
	}
	return vals, true
}

func promptName(tc *telnet.Conn) (string, bool) {
	for {
		_ = tc.Write("\r\nName your character: ")
		line, err := tc.ReadLine()
		if err != nil {
			return "", false
		}
		name := strings.TrimSpace(line)
		if reason, ok := validateName(name); !ok {
			_ = tc.Write("\r\nThat name won't do: " + reason + "\r\n")
			continue
		}
		return name, true
	}
}
