// Command telos-seed imports the embedded demo content pack into Postgres (the `make seed`
// target). It is the "content is data, not a migration" path (decision D4): the same
// packs/demo/ tree the unit tests load is merged and written into the pack='demo' definition rows
// for the live stack. Idempotent — re-running replaces the pack's rows.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/store"
)

func main() {
	cfg, err := config.Load(config.PathFromEnv())
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	ctx := context.Background()

	pack, found, err := content.LoadPack(content.DemoPack)
	if err != nil {
		slog.Error("load embedded demo pack failed", "err", err)
		os.Exit(1)
	}
	if !found {
		slog.Error("embedded demo pack not found", "pack", content.DemoPack)
		os.Exit(1)
	}

	pool, err := store.Open(ctx, cfg.Postgres.DSN)
	if err != nil {
		slog.Error("connect to postgres failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.ImportPack(ctx, pack); err != nil {
		slog.Error("import demo pack failed", "err", err)
		os.Exit(1)
	}
	slog.Info("seeded content pack", "pack", pack.Pack, "zones", len(pack.Zones))

	// Hot-reload trigger (docs/PHASE4-PLAN.md §5): publish an invalidation for every ref in the
	// re-imported pack so any RUNNING shard reloads it without a restart. OPTIONAL and non-fatal:
	// if NATS is unreachable the seed still succeeded (the rows are written) — running shards just
	// won't hot-reload until their next boot. Mirrors the rest of the optional-dependency posture.
	bus, err := contentbus.Connect(cfg.NATS.URL)
	if err != nil {
		slog.Warn("content bus unreachable; rows seeded but running shards not hot-reloaded", "err", err)
		return
	}
	defer func() { _ = bus.Close() }()
	// telos-seed imports the demo pack without a logical content version (ImportPack, not ImportVersion),
	// so it stamps a wall-clock-nanos version — the dev/seed equivalent of a shard-local reload.
	n, err := contentbus.PublishPack(ctx, bus, pack, uint64(time.Now().UnixNano()))
	if err != nil {
		slog.Warn("publishing content invalidations failed (partial)", "published", n, "err", err)
		return
	}
	slog.Info("published content invalidations", "count", n)
}
