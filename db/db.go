// Package db owns the embedded migration set and the goose runner. Migrations are the thin
// relational skeleton (docs/PERSISTENCE.md §9): definition + state tables. They are rare and
// structural — adding game flavor is a content write, never a migration.
//
// Two ways to run them, per decision D3:
//
//   - `make migrate` (the goose CLI configured against db/migrations) for dev/CI;
//   - opt-in auto-migrate on world boot, guarded by TELOS_DB_AUTOMIGRATE (default off). goose
//     takes a Postgres advisory lock, so N shards booting against one database serialize
//     their migration check instead of racing it (risk §7.6).
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver (goose needs *sql.DB)
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// AutoMigrateEnv is the env var that opts into auto-migrate-on-boot. Default OFF.
const AutoMigrateEnv = "TELOS_DB_AUTOMIGRATE"

// AutoMigrateEnabled reports whether auto-migrate-on-boot is opted in.
func AutoMigrateEnabled() bool {
	switch os.Getenv(AutoMigrateEnv) {
	case "1", "true", "TRUE", "yes", "on":
		return true
	}
	return false
}

// Migrate runs all up migrations against dsn, advisory-locked so concurrent callers (multi-
// shard boot) serialize. It opens its own short-lived *sql.DB via the pgx stdlib shim — goose
// works on database/sql, while the rest of the app uses pgxpool directly (store.Pool).
func Migrate(ctx context.Context, dsn string) error {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("db: open for migrate: %w", err)
	}
	defer sqldb.Close()
	return MigrateDB(ctx, sqldb)
}

// fsSub roots the embedded FS at migrations/ so the Provider sees the .sql files directly.
func fsSub() (fs.FS, error) {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("db: sub migrations fs: %w", err)
	}
	return sub, nil
}

// newProvider builds a goose Provider over the embedded migrations with a Postgres SESSION
// LOCKER engaged: it takes a Postgres advisory lock for the duration of the run, so N shards
// booting against one database serialize their migration check rather than racing it (decision
// D3 / risk §7.6).
func newProvider(sqldb *sql.DB) (*goose.Provider, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("db: build session locker: %w", err)
	}
	sub, err := fsSub()
	if err != nil {
		return nil, err
	}
	return goose.NewProvider(goose.DialectPostgres, sqldb, sub,
		goose.WithSessionLocker(locker))
}

// MigrateDB runs up migrations against an already-open *sql.DB (used by the `make migrate`
// CLI in cmd/telos-migrate and by Migrate above), advisory-locked.
func MigrateDB(ctx context.Context, sqldb *sql.DB) error {
	p, err := newProvider(sqldb)
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("db: migrate up: %w", err)
	}
	return nil
}

// Status returns the migration status (the `make migrate-status` target prints it).
func Status(ctx context.Context, sqldb *sql.DB) error {
	p, err := newProvider(sqldb)
	if err != nil {
		return err
	}
	st, err := p.Status(ctx)
	if err != nil {
		return err
	}
	for _, s := range st {
		fmt.Printf("%-8s %s\n", s.State, s.Source.Path)
	}
	return nil
}
