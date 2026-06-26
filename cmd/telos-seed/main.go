// Command telos-seed imports the embedded demo content pack into Postgres (the `make seed`
// target). It is the "content is data, not a migration" path (decision D4): the same
// packs/demo.yaml the unit tests load is written into the pack='demo' definition rows for the
// live stack. Idempotent — re-running replaces the pack's rows.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/double-nibble/telosmud/internal/config"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/store"
)

func main() {
	cfg, err := config.Load(config.PathFromEnv())
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	ctx := context.Background()

	data, err := content.DemoPackBytes()
	if err != nil {
		slog.Error("read embedded demo pack failed", "err", err)
		os.Exit(1)
	}
	pack, err := content.ParsePack(data)
	if err != nil {
		slog.Error("parse demo pack failed", "err", err)
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
}
