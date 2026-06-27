// Command telos-migrate runs the embedded goose migrations against the configured Postgres
// DSN (the `make migrate` target). It is the dev/CI migration entry point, kept separate from
// the world boot path so production can run migrations as a deploy step and keep boots
// read-only (docs/PHASE4-PLAN.md §7.6). Usage:
//
//	telos-migrate up        # apply all up migrations (default)
//	telos-migrate status    # print migration status
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/double-nibble/telosmud/db"
	"github.com/double-nibble/telosmud/internal/config"
)

func main() {
	cfg, err := config.Load(config.PathFromEnv())
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	ctx := context.Background()

	sqldb, err := sql.Open("pgx", cfg.Postgres.DSN)
	if err != nil {
		slog.Error("open db failed", "err", err)
		os.Exit(1)
	}
	defer func() { _ = sqldb.Close() }()

	switch cmd {
	case "up":
		if err := db.MigrateDB(ctx, sqldb); err != nil {
			slog.Error("migrate up failed", "err", err)
			os.Exit(1)
		}
		slog.Info("migrations applied", "dsn", cfg.Postgres.DSN)
	case "status":
		if err := db.Status(ctx, sqldb); err != nil {
			slog.Error("migrate status failed", "err", err)
			os.Exit(1)
		}
	default:
		slog.Error("unknown command (want: up | status)", "cmd", cmd)
		os.Exit(2)
	}
}
