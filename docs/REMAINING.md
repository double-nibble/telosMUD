# Remaining work — now tracked in GitHub

**The open backlog moved to GitHub Issues, Projects, and Milestones (2026-07-02).** This file is no
longer the source of truth — it would only drift. Everything that was here is now:

- **Issues [#20–#72](https://github.com/double-nibble/telosMUD/issues)** — one per work item, with
  `[LARGE]` items broken into sub-issues under their parent (e.g. the builder/wizard trust tier, the
  demo-packs sprint). Each issue carries its owning component and a link back to the track it came from.
- **Pinned meta issue [#73](https://github.com/double-nibble/telosMUD/issues/73)** — the **shared-seam
  map + hard ordering constraints + suggested attack order**. Read this first: it's the dependency
  compass that keeps us from building against a stub and reworking a seam.
- **[Projects](https://github.com/double-nibble/telosMUD/projects)** — one board per track (Track 0–10)
  plus a "Standalone, independent & blocked" board. Each issue lives on its track's board.
- **Milestones** — one per **burn-down round**. The current round
  ([Round 6](https://github.com/double-nibble/telosMUD/milestone/1)) holds the issues queued for the
  next session; future rounds are created as work is pulled into them. An issue with no milestone is
  un-queued backlog.

## Finding things

- **By track:** open the track's Project board, or filter issues by the `track-0`…`track-10` label.
- **By kind:** the `large` label (design-heavy, wants a planning pass) and `blocked` label
  (waiting on another slice — don't start cold).
- **What's next:** the open **Round** milestone.

## How the burn-down works now

Pick an issue from the current Round milestone, branch → PR → all required checks green → merge (main
is ruleset-protected; no direct pushes). Each change still gets the owning component-engineer + a
cross-cutting reviewer before merge. When a round's issues are all closed, the milestone closes and the
next round opens. Completed work is recorded in [COMPLETED.md](COMPLETED.md), which also holds the
history of earlier burn-down rounds and the launch-hardening rounds.
