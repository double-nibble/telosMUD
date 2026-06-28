# TELOSMUD fork of gopher-lua

This is a **minimal in-tree fork** of [`github.com/yuin/gopher-lua`](https://github.com/yuin/gopher-lua)
**v1.1.1**, vendored under `third_party/gopher-lua` and wired into the main module
through a `replace` directive in the repo-root `go.mod`:

```
replace github.com/yuin/gopher-lua => ./third_party/gopher-lua
```

The module path is **unchanged** (`github.com/yuin/gopher-lua`), so engine code imports
it as `lua "github.com/yuin/gopher-lua"` with no awareness of the fork.

> **DO NOT run `gofumpt` / `golangci-lint fmt` / a mass formatter across this directory.**
> Upstream gopher-lua is `gofmt`-formatted, not `gofumpt`-formatted; a reformat rewrites
> ~every file and **bloats the minimal delta**, making the next upstream re-apply (below) a
> merge nightmare. This module is excluded from the main module's lint/CI by design. Keep the
> fork **upstream-formatted** and touch ONLY the tagged sites. If you must edit a fork file,
> hand-edit just the tagged hunk and leave the rest byte-for-byte upstream.

## Why we fork (docs/PHASE7-PLAN.md P7-D6 / T3)

The Lua sandbox needs a **deterministic per-call instruction-count abort** so a tight
pure-CPU loop (`while true do end`) cannot stall the single-writer zone goroutine, and so
the abort is **test-reproducible** (wall-clock alone is not — it varies run to run and
cannot stop a loop that re-burns the deadline each call).

Upstream gopher-lua **v1.1.1 has no `SetHook` / `MaskCount` / debug-hook of any kind**
(confirmed by the security probe), so the count cannot be added from outside the package.
The fork adds it **inline in `mainLoopWithContext`**, beside the existing `ctx.Done()`
deadline select — the wall-clock layer is upstream, the instruction-count layer is the fork.

## The delta (kept deliberately small)

Two files, a handful of lines:

1. **`value.go`** — added two fields to `LState` (`instrBudget`, `instrCount`) and three
   exported methods:
   - `SetInstructionBudget(n int)` — arm (n>0) / disarm (n<=0) the per-call cap.
   - `ResetInstructionCount()` — zero the tally; called at the per-call chokepoint that
     also arms the fresh context deadline.
   - `InstructionCount() int` — read the tally (tests / metrics).
2. **`vm.go`** — in `mainLoopWithContext`, before the `ctx.Done()` select, a guarded
   counter:
   ```go
   if L.instrBudget > 0 {
       L.instrCount++
       if L.instrCount > L.instrBudget {
           L.RaiseError("instruction budget exceeded")
           return
       }
   }
   ```
   When `instrBudget == 0` (the default for any `LState` that never opts in) this is a
   single predictable-branch no-op, so a VM that never arms the budget behaves **exactly
   like upstream** — both functionally and ~at parity on perf.
3. **`_vm.go`** — the same block mirrored into the `go-inline` **source template**, so a
   future regeneration (`go generate` / the upstream `_tools` go-inline step) reproduces
   the patch in `vm.go` instead of dropping it. `vm.go` is the file the Go build actually
   compiles (the `_`-prefixed files are ignored by the toolchain); `_vm.go` is kept in sync
   for regenerability only.

The abort uses `RaiseError` — the **same mechanism the context deadline already uses** — so
it surfaces as an ordinary Lua error caught by the engine's outer `pcall` (no new error
channel, no special-casing on the engine side beyond recognizing the message string).

4. **`state.go`** — added one read-only accessor `(*LState).RegistryCap() int` (slice 7.5),
   returning `cap(ls.reg.array)` — the value-stack registry capacity, a cheap monotonic proxy
   for the VM's growable memory footprint, for the detection-only per-zone memory metric (T5).
   Read-only; allocates nothing; exposes only a count. (The registry is otherwise unexported.)

### Removed from the vendored copy

To keep the fork lean and dependency-free, the upstream **`cmd/glua` REPL** and **`_tools`
go-inline generator** directories were deleted (the REPL was the only consumer of
`github.com/chzyer/readline`). Consequently the fork's `go.mod` drops the `readline` and
test-only requires. The library packages (`.`, `parse`, `ast`, `pm`) are untouched apart
from the delta above.

## Re-applying the fork on an upstream bump

1. Note the new upstream version (e.g. `v1.2.0`).
2. Replace the contents of `third_party/gopher-lua` with the new upstream source
   (`cp -R $(go env GOMODCACHE)/github.com/yuin/gopher-lua@vX.Y.Z/. third_party/gopher-lua/`),
   then re-delete `cmd/` and `_tools/` and re-trim `go.mod` as above.
3. Re-apply the delta:
   - Re-add the `instrBudget`/`instrCount` fields + the three methods to `LState` in
     `value.go` (search for `ctxCancelFn` to find the struct).
   - Re-add the guarded counter block to `mainLoopWithContext` in **both** `vm.go` and
     `_vm.go` (search for `case <-L.ctx.Done():`).
   All fork lines are tagged with a `TELOSMUD FORK` comment — grep for it to audit:
   ```
   grep -rn "TELOSMUD FORK" third_party/gopher-lua
   ```
4. `cd third_party/gopher-lua && GOWORK=off go build ./...` then the repo's
   `go build ./... && go test ./...`.

## Pinning

The fork is pinned by being **in-tree** — there is no remote to drift. The base version
(**v1.1.1**) is recorded here and in the root `go.mod` comment. The original upstream
`go.sum` line for v1.1.1 is preserved in the repo `go.sum` history for provenance.
