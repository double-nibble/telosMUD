package world

import "strings"

// credits.go — the `credits` verb (#519): a universal, read-only listing of the loaded content packs'
// license/attribution metadata. A content pack declares `license`/`attribution` on its manifest; the
// loader accumulates one PackCredit per crediting pack (loader.go) and the world stamps the slice onto the
// per-shard bundle at build (build.go). This verb renders it so a license's REQUIRED attribution (e.g. the
// 5e SRD's CC-BY notice) is machine-visible IN-GAME, not only in a repo NOTICE file. Pure metadata display
// — no gameplay effect and no gate (credits are public info), so it is universal like `clear`.

// creditsCommands returns the `credits` verb (aliases `license`/`copyright`). Registered low-priority so it
// never shadows or abbreviates a movement/look/say verb.
func creditsCommands() []*Command {
	return []*Command{
		{Name: "credits", Aliases: []string{"license", "copyright"}, Run: cmdCredits},
	}
}

// cmdCredits lists each crediting pack's license id and attribution notice. A world whose packs declare no
// credits reports a clean notice rather than an empty listing.
func cmdCredits(c *Context) error {
	credits := c.z.defBundle().packCredits
	if len(credits) == 0 {
		c.Send("This world ships no content credits.")
		return nil
	}
	var b strings.Builder
	b.WriteString(colorize("Content credits", "FG_CYAN"))
	b.WriteString("\n")
	for _, cr := range credits {
		b.WriteString("\n")
		b.WriteString(colorize(cr.Pack, "FG_YELLOW"))
		if cr.License != "" {
			b.WriteString(" — ")
			b.WriteString(cr.License)
		}
		if cr.Attribution != "" {
			b.WriteString("\n    ")
			b.WriteString(cr.Attribution)
		}
	}
	c.Send(b.String())
	return nil
}
