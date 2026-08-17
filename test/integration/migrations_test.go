package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/anpl1623/sharpline/internal/platform/migrate"
)

// TestMigrationsApplyFromScratchAndEveryDownReversesItsUp is the phase-2a gate,
// turned into a test.
//
// Phase 2a verified reversibility by hand: apply everything, roll everything back,
// apply it again, and compare two pg_dump outputs. A check performed once by a
// human is a check that has already stopped working — the next migration is written
// by someone who does not know the ritual. This runs it in CI.
//
// It is stronger than the manual gate in two ways:
//
//  1. The rollback is STEPWISE. `down-to 0` proves the set can be undone in bulk;
//     rolling back one migration at a time proves each individual Down runs, which
//     is the operation an operator actually performs at 3am. A Down that only works
//     as part of a bulk rollback — because it depends on a later migration's object
//     already being gone — passes the bulk test and fails this one.
//
//  2. Identity is asserted twice, by two independent readers: a catalogue-derived
//     fingerprint that also covers the TimescaleDB metadata pg_dump cannot see, and
//     pg_dump itself.
func TestMigrationsApplyFromScratchAndEveryDownReversesItsUp(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db := freshDatabase(t)
	log := testLogger(t)
	embedded := embeddedMigrationCount(t)

	// ---- up, from a genuinely empty database -------------------------------
	up1, err := runMigrate(ctx, db.dsn, log, migrate.Invocation{Command: migrate.CommandUp})
	if err != nil {
		t.Fatalf("first up: %v", err)
	}
	if got := len(up1.Applied); got != embedded {
		t.Fatalf("first up applied %d migrations, want all %d embedded", got, embedded)
	}
	if up1.VersionBefore != 0 {
		t.Fatalf("first up started at version %d; the database was supposed to be empty", up1.VersionBefore)
	}
	highest := up1.VersionAfter

	conn := rawConn(t, db.dsn)

	fingerprintAfterFirstUp, err := schemaFingerprint(ctx, conn)
	if err != nil {
		t.Fatalf("fingerprint after first up: %v", err)
	}
	dumpAfterFirstUp, err := pgDumpSchema(ctx, db)
	if err != nil {
		t.Fatalf("pg_dump after first up: %v", err)
	}

	// A migrated database must actually contain the schema. Without this the rest
	// of the test would happily prove that "empty" reverses to "empty".
	if tables := countPublicTables(t, ctx, conn); tables < 20 {
		t.Fatalf("only %d tables in public after up; phase 2a's schema has 21", tables)
	}

	// ---- a second up is a no-op -------------------------------------------
	up2, err := runMigrate(ctx, db.dsn, log, migrate.Invocation{Command: migrate.CommandUp})
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if len(up2.Applied) != 0 {
		t.Errorf("second up applied %d migrations; an at-head `up` must apply none", len(up2.Applied))
	}

	// ---- validate reports at head, and applies nothing ---------------------
	validate, err := runMigrate(ctx, db.dsn, log, migrate.Invocation{Command: migrate.CommandValidate})
	if err != nil {
		t.Fatalf("validate at head: %v", err)
	}
	if validate.PendingBefore != 0 {
		t.Errorf("validate reports %d pending at head, want 0", validate.PendingBefore)
	}
	if validate.VersionAfter != highest {
		t.Errorf("validate moved the schema version from %d to %d; validate must apply nothing",
			highest, validate.VersionAfter)
	}

	// ---- down, one migration at a time, to zero ----------------------------
	rolledBack := 0
	for {
		summary, err := runMigrate(ctx, db.dsn, log, migrate.Invocation{Command: migrate.CommandDown})
		if err != nil {
			// `down` with nothing applied is a deliberate FAILURE, not a silent
			// no-op — an explicit request to change the schema that changed
			// nothing belongs on stderr with a non-zero status. That is the loop's
			// terminating condition.
			if errors.Is(err, migrate.ErrNothingToRollBack) {
				break
			}
			t.Fatalf("down after %d rollbacks: %v", rolledBack, err)
		}
		if len(summary.Applied) != 1 {
			t.Fatalf("one `down` rolled back %d migrations, want exactly 1", len(summary.Applied))
		}
		if dir := summary.Applied[0].Direction; dir != "down" {
			t.Fatalf("`down` reported direction %q", dir)
		}
		rolledBack++
		if rolledBack > embedded {
			t.Fatalf("`down` has now run %d times against %d migrations and still reports work; the loop is not terminating", rolledBack, embedded)
		}
	}
	if rolledBack != embedded {
		t.Errorf("rolled back %d migrations one at a time, want %d — a Down that only works in a bulk rollback fails here",
			rolledBack, embedded)
	}

	// ---- the rollback is complete -----------------------------------------
	//
	// goose_db_version is expected to survive: it is goose's own bookkeeping, not
	// a migration's object, and no migration creates it. Everything else must be
	// gone.
	if leftovers := publicRelationsExcept(t, ctx, conn, "goose_db_version"); len(leftovers) != 0 {
		t.Errorf("after rolling every migration back, these relations are still in public: %v\n"+
			"A Down did not drop what its Up created.", leftovers)
	}
	if leftovers := publicFunctions(t, ctx, conn); len(leftovers) != 0 {
		t.Errorf("after rolling every migration back, these functions are still in public: %v\n"+
			"A Down dropped its tables but left its trigger functions behind.", leftovers)
	}
	if version := schemaVersion(t, ctx, conn); version != 0 {
		t.Errorf("schema version after a full rollback is %d, want 0", version)
	}
	// Both hypertables are migration-created, so neither may survive.
	if n := scalarInt(t, ctx, conn, `SELECT count(*) FROM timescaledb_information.hypertables`); n != 0 {
		t.Errorf("%d hypertables survived the rollback; migrations 00003/00004 own both of them", n)
	}
	if n := scalarInt(t, ctx, conn, `SELECT count(*) FROM timescaledb_information.jobs WHERE hypertable_name IS NOT NULL`); n != 0 {
		t.Errorf("%d TimescaleDB policy jobs survived the rollback; a leftover policy makes the next `up` create a duplicate", n)
	}

	// ---- up again, and assert the schema is identical -----------------------
	up3, err := runMigrate(ctx, db.dsn, log, migrate.Invocation{Command: migrate.CommandUp})
	if err != nil {
		t.Fatalf("up after full rollback: %v", err)
	}
	if got := len(up3.Applied); got != embedded {
		t.Fatalf("up after rollback applied %d migrations, want %d", got, embedded)
	}
	if up3.VersionAfter != highest {
		t.Errorf("up after rollback reached version %d, first up reached %d", up3.VersionAfter, highest)
	}

	fingerprintAfterSecondUp, err := schemaFingerprint(ctx, conn)
	if err != nil {
		t.Fatalf("fingerprint after second up: %v", err)
	}
	dumpAfterSecondUp, err := pgDumpSchema(ctx, db)
	if err != nil {
		t.Fatalf("pg_dump after second up: %v", err)
	}

	assertSameText(t, "the catalogue fingerprint", fingerprintAfterFirstUp, fingerprintAfterSecondUp)
	assertSameText(t, "pg_dump --schema-only", dumpAfterFirstUp, dumpAfterSecondUp)
}

// TestMigrateRefusesToRunAgainstASchemaAheadOfTheBinary covers the rule that has
// no equivalent in goose: an APPLIED version this binary does not embed.
//
// goose rejects an out-of-order source. It does not notice the opposite — a row in
// goose_db_version with no matching file — which is exactly what deploying an older
// image over a newer schema looks like. Without the check, `up` reports "0
// applied", exits 0, and `api` starts against a schema whose shape its code does
// not know. A silent success is worse than any crash, so this asserts the refusal.
//
// The fixture is one row in goose's own bookkeeping table, inserted by this test
// and asserted on by this test. It carries no domain data.
func TestMigrateRefusesToRunAgainstASchemaAheadOfTheBinary(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	db := freshMigratedDatabase(t)
	conn := rawConn(t, db.dsn)
	log := testLogger(t)

	// A version far above anything that could ever be embedded.
	const futureVersion = 99999
	if _, err := conn.Exec(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES ($1, TRUE, now())`,
		futureVersion); err != nil {
		t.Fatalf("record a future applied version: %v", err)
	}

	for _, cmd := range []migrate.Command{migrate.CommandUp, migrate.CommandValidate} {
		t.Run(string(cmd), func(t *testing.T) {
			_, err := runMigrate(ctx, db.dsn, log, migrate.Invocation{Command: cmd})
			if err == nil {
				t.Fatalf("%s succeeded against a database whose applied version %d this binary does not embed; "+
					"the deploy-an-older-image case must fail loudly, not report 0 applied and exit 0", cmd, futureVersion)
			}
			if !errors.Is(err, migrate.ErrSchemaAhead) {
				t.Fatalf("%s failed with %v, want ErrSchemaAhead", cmd, err)
			}
		})
	}

	// Cleaning up is part of the test: with the row removed, the runner works
	// again, which proves the refusal is a live check on the database's state and
	// not a latched flag.
	if _, err := conn.Exec(ctx, `DELETE FROM goose_db_version WHERE version_id = $1`, futureVersion); err != nil {
		t.Fatalf("remove the future version row: %v", err)
	}
	if _, err := runMigrate(ctx, db.dsn, log, migrate.Invocation{Command: migrate.CommandValidate}); err != nil {
		t.Fatalf("validate after removing the future version row: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Catalogue helpers
// -----------------------------------------------------------------------------

func countPublicTables(t *testing.T, ctx context.Context, conn *pgx.Conn) int {
	t.Helper()
	return scalarInt(t, ctx, conn, `
SELECT count(*)
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public'
   AND c.relkind IN ('r', 'p')
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid AND d.deptype = 'e')`)
}

// publicRelationsExcept lists the tables and views left in public, minus the names
// the caller expects to survive.
func publicRelationsExcept(t *testing.T, ctx context.Context, conn *pgx.Conn, keep ...string) []string {
	t.Helper()
	return stringColumn(t, ctx, conn, `
SELECT c.relname
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public'
   AND c.relkind IN ('r', 'p', 'v', 'm')
   AND NOT (c.relname = ANY($1::TEXT[]))
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid AND d.deptype = 'e')
 ORDER BY 1`, keep)
}

// publicFunctions lists this project's own functions in public. Extension-owned
// ones (TimescaleDB installs about a hundred) are excluded, so a non-empty result
// is always a migration's leftover.
func publicFunctions(t *testing.T, ctx context.Context, conn *pgx.Conn) []string {
	t.Helper()
	return stringColumn(t, ctx, conn, `
SELECT p.proname
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = 'public'
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
 ORDER BY 1`)
}

// schemaVersion reads goose's applied high-water mark the same way
// `make migrate-dry-run` does, filtering the sentinel row 0.
func schemaVersion(t *testing.T, ctx context.Context, conn *pgx.Conn) int64 {
	t.Helper()
	var version int64
	err := conn.QueryRow(ctx, `
SELECT coalesce(max(version_id), 0)
  FROM goose_db_version
 WHERE is_applied AND version_id > 0`).Scan(&version)
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	return version
}

func scalarInt(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

func scalarInt64(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

func scalarString(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := conn.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return s
}

func scalarBool(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string, args ...any) bool {
	t.Helper()
	var b bool
	if err := conn.QueryRow(ctx, sql, args...).Scan(&b); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return b
}

func stringColumn(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string, args ...any) []string {
	t.Helper()
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan %q: %v", sql, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %q: %v", sql, err)
	}
	return out
}
