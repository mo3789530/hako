package dsql

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const (
	createMigrationTable = `CREATE TABLE IF NOT EXISTS hako_schema_migrations (
		version BIGINT PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL
	)`
	maxDDLRetries = 5
)

type migration struct {
	version int64
	name    string
	sql     string
}

// Migrate applies embedded migrations in order. Each migration file must
// contain exactly one idempotent DDL statement. Aurora DSQL does not permit
// combining DDL with DML in one transaction, so version recording is a
// separate statement after DDL succeeds.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("Aurora DSQL pool is required")
	}
	if err := execDDL(ctx, pool, createMigrationTable); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}

	migrations, err := loadMigrations(migrationFiles)
	if err != nil {
		return err
	}
	known := make(map[int64]struct{}, len(migrations))
	for _, migration := range migrations {
		known[migration.version] = struct{}{}
	}
	for version := range applied {
		if _, ok := known[version]; !ok {
			return fmt.Errorf("database has unknown migration version %d; refusing to run older migration set", version)
		}
	}
	missingBeforeApplied := false
	for _, migration := range migrations {
		if _, ok := applied[migration.version]; ok {
			if missingBeforeApplied {
				return fmt.Errorf("migration ledger is out of order at version %d", migration.version)
			}
			continue
		}
		missingBeforeApplied = true
		if err := execDDL(ctx, pool, migration.sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", migration.name, err)
		}
		_, err := pool.Exec(ctx,
			`INSERT INTO hako_schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`,
			migration.version, migration.name, time.Now().UTC(),
		)
		if err != nil {
			return fmt.Errorf("record migration %s: %w", migration.name, err)
		}
	}
	return nil
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[int64]struct{}, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM hako_schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	versions := make(map[int64]struct{})
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		versions[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return versions, nil
}

func loadMigrations(files fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	var migrations []migration
	for _, entry := range entries {
		if entry.IsDir() || path.Ext(entry.Name()) != ".sql" {
			continue
		}
		versionText, _, ok := strings.Cut(entry.Name(), "_")
		if !ok || len(versionText) != 6 {
			return nil, fmt.Errorf("invalid migration filename %q: expected NNNNNN_name.sql", entry.Name())
		}
		version, err := strconv.ParseInt(versionText, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse migration version from %q: %w", entry.Name(), err)
		}
		contents, err := fs.ReadFile(files, path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		sql := strings.TrimSpace(string(contents))
		sql = strings.TrimSuffix(sql, ";")
		if sql == "" || strings.Contains(sql, ";") {
			return nil, fmt.Errorf("migration %s must contain exactly one SQL statement", entry.Name())
		}
		migrations = append(migrations, migration{version: version, name: entry.Name(), sql: sql})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})
	for i := 1; i < len(migrations); i++ {
		if migrations[i-1].version == migrations[i].version {
			return nil, fmt.Errorf("duplicate migration version %d", migrations[i].version)
		}
	}
	return migrations, nil
}

func execDDL(ctx context.Context, pool *pgxpool.Pool, statement string) error {
	var err error
	for attempt := 0; attempt < maxDDLRetries; attempt++ {
		_, err = pool.Exec(ctx, statement)
		if err == nil || !isSerializationConflict(err) || attempt == maxDDLRetries-1 {
			return err
		}

		delay := time.Duration(1<<attempt) * 50 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func isSerializationConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40001"
}
