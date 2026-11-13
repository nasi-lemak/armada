package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// Database schema changes are numbered migrations, applied in order, each in
// its own transaction, and recorded in schema_migrations.
//
// Rules for changing the schema:
//   - Add a new migration at the end of the list with the next version.
//   - Never edit, renumber or remove a migration that has been released:
//     databases in the field have already run it. TestMigrationsAreFrozen
//     fails if one changes.
//   - Write each migration for SQLite and keep it to plain DDL/DML. SQLite
//     runs DDL inside transactions, so a failed migration leaves nothing
//     behind.
//
// Version 1 is the baseline: the schema as it was before versioning. It is
// written to be idempotent (CREATE ... IF NOT EXISTS, columns added only if
// missing), so databases created by earlier builds, which have no
// schema_migrations table, are brought up to the same state as new ones.

type migration struct {
	Version int
	Name    string
	// SQL runs as one script. Use Apply instead for logic SQL can't express.
	SQL   string
	Apply func(ctx context.Context, tx *sql.Tx) error
}

var migrations = []migration{
	{Version: 1, Name: "baseline", Apply: applyBaseline},
	{Version: 2, Name: "index events by time and sessions by expiry", SQL: `
CREATE INDEX IF NOT EXISTS events_time ON events(time);
CREATE INDEX IF NOT EXISTS sessions_expires ON sessions(expires_at);
`},
	{Version: 3, Name: "maintenance windows", SQL: `
CREATE TABLE IF NOT EXISTS maintenance (
	id TEXT PRIMARY KEY,
	spec TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
`},
	{Version: 4, Name: "device lifecycle", SQL: `
ALTER TABLE devices ADD COLUMN lifecycle TEXT NOT NULL DEFAULT 'active';
ALTER TABLE devices ADD COLUMN lifecycle_info TEXT NOT NULL DEFAULT '';
`},
	{Version: 5, Name: "bulk operations", SQL: `
CREATE TABLE IF NOT EXISTS bulk_operations (
	id TEXT PRIMARY KEY,
	spec TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
ALTER TABLE actions ADD COLUMN batch_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS actions_batch ON actions(batch_id) WHERE batch_id <> '';
`},
	{Version: 6, Name: "report schedules", SQL: `
CREATE TABLE IF NOT EXISTS report_schedules (
	id TEXT PRIMARY KEY,
	spec TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS alerts_started ON alerts(started_at);
CREATE INDEX IF NOT EXISTS device_updates_updated ON device_updates(updated_at);
`},
	// Append new migrations here.
}

// LatestSchemaVersion is the schema version this build expects.
func LatestSchemaVersion() int { return migrations[len(migrations)-1].Version }

// ErrNewerSchema means the database was migrated by a newer build.
var ErrNewerSchema = errors.New("database schema is newer than this build")

// baselineSchemas are the table definitions of version 1, in order.
var baselineSchemas = []string{schema, usersSchema, historySchema, scimSchema, passkeySchema, mfaSchema, configSchema}

// baselineColumns were added to tables over time before versioning; version
// 1 adds whichever a database is missing.
var baselineColumns = []struct{ Table, Name, Def string }{
	{"devices", "hw_key", "TEXT NOT NULL DEFAULT ''"},
	{"devices", "attestation", "TEXT NOT NULL DEFAULT ''"},
	{"devices", "license_revoked", "INTEGER NOT NULL DEFAULT 0"},
	{"devices", "health", "TEXT NOT NULL DEFAULT '[]'"},
	{"devices", "parent_id", "TEXT NOT NULL DEFAULT ''"},
	{"events", "actor", "TEXT NOT NULL DEFAULT ''"},
	{"releases", "version_code", "INTEGER NOT NULL DEFAULT 0"},
	{"sessions", "method", "TEXT NOT NULL DEFAULT 'password'"},
	{"deployments", "created_by", "TEXT NOT NULL DEFAULT ''"},
	{"deployments", "approved_by", "TEXT NOT NULL DEFAULT ''"},
	{"deployments", "approved_at", "INTEGER NOT NULL DEFAULT 0"},
	{"sessions", "created_at", "INTEGER NOT NULL DEFAULT 0"},
}

func applyBaseline(ctx context.Context, tx *sql.Tx) error {
	for _, s := range baselineSchemas {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	for _, c := range baselineColumns {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, c.Table, c.Name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE `+c.Table+` ADD COLUMN `+c.Name+` `+c.Def); err != nil {
				return err
			}
		}
	}
	return nil
}

// MigrationInfo describes what Open did to the schema.
type MigrationInfo struct {
	From, To int
	Applied  []string // "2: index events by time…"
	Backup   string   // copy taken before migrating, if any
}

const migrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at INTEGER NOT NULL
)`

func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	if _, err := db.ExecContext(ctx, migrationsTable); err != nil {
		return 0, err
	}
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

// hasData reports whether a database has tables besides schema_migrations
// (an existing install, versioned or not).
func hasData(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_migrations') AND name NOT LIKE 'sqlite_%'`).Scan(&n)
	return n > 0, err
}

// migrate brings a database to the latest version. Before changing an
// existing database it copies it next to itself (path is "" for in-memory
// databases, which get no copy).
func migrate(ctx context.Context, db *sql.DB, path string) (MigrationInfo, error) {
	info := MigrationInfo{To: LatestSchemaVersion()}
	cur, err := schemaVersion(ctx, db)
	if err != nil {
		return info, err
	}
	info.From = cur
	if cur > info.To {
		return info, fmt.Errorf("%w: the database is at version %d, this build knows up to %d; run a newer armada-server (or restore a backup taken before the upgrade)", ErrNewerSchema, cur, info.To)
	}
	if cur == info.To {
		return info, nil
	}
	if existing, err := hasData(ctx, db); err != nil {
		return info, err
	} else if existing && path != "" {
		info.Backup = fmt.Sprintf("%s.before-v%d-%s", path, info.To, time.Now().UTC().Format("20060102T150405Z"))
		if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, info.Backup); err != nil {
			return info, fmt.Errorf("backing up the database before migrating: %w", err)
		}
		_ = os.Chmod(info.Backup, 0o600)
	}
	for _, m := range migrations {
		if m.Version <= cur {
			continue
		}
		if err := runMigration(ctx, db, m); err != nil {
			msg := fmt.Sprintf("migration %d (%s) failed; the database is unchanged at version %d", m.Version, m.Name, cur)
			if info.Backup != "" {
				msg += "; a copy from before the upgrade is at " + info.Backup
			}
			return info, fmt.Errorf("%s: %w", msg, err)
		}
		cur = m.Version
		info.Applied = append(info.Applied, fmt.Sprintf("%d: %s", m.Version, m.Name))
	}
	return info, nil
}

func runMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if m.Apply != nil {
		err = m.Apply(ctx, tx)
	} else {
		_, err = tx.ExecContext(ctx, m.SQL)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?,?,?)`, m.Version, m.Name, ts(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

// SchemaStatus reports a database file's schema version and the migrations
// this build would apply, without changing anything.
func SchemaStatus(path string) (current int, pending []string, err error) {
	if _, err := os.Stat(path); err != nil {
		return 0, nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, nil, err
	}
	defer db.Close()
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&exists); err != nil {
		return 0, nil, err
	}
	if exists > 0 {
		var v sql.NullInt64
		if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
			return 0, nil, err
		}
		current = int(v.Int64)
	}
	for _, m := range migrations {
		if m.Version > current {
			pending = append(pending, fmt.Sprintf("%d: %s", m.Version, m.Name))
		}
	}
	return current, pending, nil
}
