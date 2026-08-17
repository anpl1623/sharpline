package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// This file builds two independent descriptions of a live schema, so that
// "identical" is an assertion with a diff attached rather than a claim.
//
//	schemaFingerprint — read out of the system catalogues as ordered text. It
//	                    knows nothing about OIDs or physical order, names every
//	                    object by NAME, and includes the TimescaleDB metadata a
//	                    plain dump does not cover (chunk interval, compression
//	                    settings, policy jobs). When it differs, the failure
//	                    message points at the exact object.
//
//	pgDumpSchema      — `pg_dump --schema-only`, run INSIDE the engine container.
//	                    This is the check phase 2a performed by hand; it is here so
//	                    it runs in CI and cannot regress. It corroborates the
//	                    fingerprint from a completely different code path — if a
//	                    catalogue query I forgot to write hides a difference, the
//	                    dump still sees it.
//
// Both are needed. The fingerprint is diagnosable; the dump is exhaustive.

// fingerprintQueries each return a single text column. Every one is ordered, and
// every one is keyed by NAME rather than OID, because object OIDs differ between
// two runs of the same DDL and would make an identical schema look different.
//
// Extension-owned objects are excluded via pg_depend deptype = 'e'. Without that,
// TimescaleDB's ~100 functions and pg_stat_statements' two views appear in the
// fingerprint — harmless for a comparison, but they bury the 200-odd lines that
// belong to this project's own migrations, and a CREATE EXTENSION whose version
// moved would then read as a migration defect.
var fingerprintQueries = []struct {
	label string
	sql   string
}{
	{
		// Columns, in physical order. attnum is zero-padded so ordering the
		// formatted text keeps the column order rather than sorting the names
		// alphabetically — a schema whose columns are in a different order is a
		// different schema.
		label: "columns",
		sql: `
SELECT format('%s %s.%s %s %s default=%s',
              c.relkind, c.relname, lpad(a.attnum::TEXT, 3, '0'), a.attname,
              format_type(a.atttypid, a.atttypmod)
                || CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE ' NULL' END,
              coalesce(pg_get_expr(ad.adbin, ad.adrelid), '-'))
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
  LEFT JOIN pg_attrdef ad ON ad.adrelid = c.oid AND ad.adnum = a.attnum
 WHERE n.nspname = 'public'
   AND c.relkind IN ('r', 'p', 'v', 'm')
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid AND d.deptype = 'e')
 ORDER BY 1`,
	},
	{
		label: "constraints",
		sql: `
SELECT format('%s %s %s', c.relname, con.conname, pg_get_constraintdef(con.oid))
  FROM pg_constraint con
  JOIN pg_class c ON c.oid = con.conrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public'
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = con.oid AND d.deptype = 'e')
 ORDER BY 1`,
	},
	{
		label: "indexes",
		sql: `
SELECT indexdef
  FROM pg_indexes
 WHERE schemaname = 'public'
 ORDER BY 1`,
	},
	{
		// Non-internal triggers only. The append-only guards, the updated_at
		// stampers and the two deferred constraint triggers all land here; the
		// per-chunk machinery TimescaleDB installs lives in
		// _timescaledb_internal and is out of scope by schema.
		label: "triggers",
		sql: `
SELECT pg_get_triggerdef(t.oid)
  FROM pg_trigger t
  JOIN pg_class c ON c.oid = t.tgrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public'
   AND NOT t.tgisinternal
 ORDER BY 1`,
	},
	{
		label: "functions",
		sql: `
SELECT pg_get_functiondef(p.oid)
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = 'public'
   AND p.prokind = 'f'
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
 ORDER BY 1`,
	},
	{
		label: "views",
		sql: `
SELECT format('%s => %s', c.relname, pg_get_viewdef(c.oid, TRUE))
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public'
   AND c.relkind IN ('v', 'm')
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid AND d.deptype = 'e')
 ORDER BY 1`,
	},
	{
		// Comments are part of the schema. Every migration documents its tables
		// and columns heavily, and a Down that drops a table drops its comments
		// with it — so a Down that recreates a table but forgets its COMMENT ON
		// is a real, silent divergence that only this section catches.
		label: "comments",
		sql: `
SELECT format('relation %s: %s', c.relname, obj_description(c.oid, 'pg_class'))
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public'
   AND c.relkind IN ('r', 'p', 'v', 'm')
   AND obj_description(c.oid, 'pg_class') IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid AND d.deptype = 'e')
UNION ALL
SELECT format('column %s.%s: %s', c.relname, a.attname, col_description(c.oid, a.attnum))
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
 WHERE n.nspname = 'public'
   AND col_description(c.oid, a.attnum) IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid AND d.deptype = 'e')
UNION ALL
SELECT format('function %s(%s): %s', p.proname, pg_get_function_identity_arguments(p.oid),
              obj_description(p.oid, 'pg_proc'))
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname = 'public'
   AND obj_description(p.oid, 'pg_proc') IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
UNION ALL
SELECT format('constraint %s.%s: %s', c.relname, con.conname, obj_description(con.oid, 'pg_constraint'))
  FROM pg_constraint con
  JOIN pg_class c ON c.oid = con.conrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public'
   AND obj_description(con.oid, 'pg_constraint') IS NOT NULL
 ORDER BY 1`,
	},
	{
		// TimescaleDB metadata. Invisible to pg_dump --schema-only, which is the
		// other half of why the fingerprint exists: the chunk interval and the
		// compression policy are the whole point of migrations 00003 and 00004,
		// and a Down that forgot to remove a policy would leave the second `up`
		// with a duplicate.
		label: "timescale/dimensions",
		sql: `
SELECT format('%s dimension %s type=%s interval=%s',
              hypertable_name, column_name, column_type, coalesce(time_interval::TEXT, '-'))
  FROM timescaledb_information.dimensions
 ORDER BY 1`,
	},
	{
		label: "timescale/hypertables",
		sql: `
SELECT format('%s compression_enabled=%s num_dimensions=%s',
              hypertable_name, compression_enabled, num_dimensions)
  FROM timescaledb_information.hypertables
 WHERE hypertable_schema = 'public'
 ORDER BY 1`,
	},
	{
		label: "timescale/compression_settings",
		sql: `
SELECT format('%s %s segmentby=%s orderby=%s asc=%s nullsfirst=%s',
              hypertable_name, attname,
              coalesce(segmentby_column_index::TEXT, '-'),
              coalesce(orderby_column_index::TEXT, '-'),
              coalesce(orderby_asc::TEXT, '-'),
              coalesce(orderby_nullsfirst::TEXT, '-'))
  FROM timescaledb_information.compression_settings
 ORDER BY 1`,
	},
	{
		// Two TimescaleDB surrogate keys are deliberately NOT in the fingerprint,
		// and both were found by this test failing:
		//
		//	job_id         — assigned from a sequence that dropping a policy does
		//	                 not reset, so the second `up` gets 1001 where the first
		//	                 got 1000.
		//	hypertable_id  — same, inside the policy's own `config` JSON: measured
		//	                 1 on the first up and 3 after a full rollback and
		//	                 reapply, because both hypertables were recreated.
		//
		// Neither is schema. What must match is which policies exist, on what, how
		// often, and with what settings — so config is compared with the surrogate
		// stripped (`config - 'hypertable_id'`) rather than whole. Stripping it is
		// safe because the hypertable this policy belongs to is already asserted by
		// name in the same line.
		label: "timescale/policy_jobs",
		sql: `
SELECT format('%s on %s every %s config=%s',
              proc_name, hypertable_name, schedule_interval,
              coalesce((config - 'hypertable_id')::TEXT, '-'))
  FROM timescaledb_information.jobs
 WHERE hypertable_name IS NOT NULL
 ORDER BY 1`,
	},
	{
		label: "extensions",
		sql: `
SELECT format('extension %s', extname)
  FROM pg_extension
 ORDER BY 1`,
	},
}

// schemaFingerprint renders the live schema as ordered, section-labelled text.
func schemaFingerprint(ctx context.Context, conn *pgx.Conn) (string, error) {
	var b strings.Builder
	for _, q := range fingerprintQueries {
		fmt.Fprintf(&b, "===== %s =====\n", q.label)

		rows, err := conn.Query(ctx, q.sql)
		if err != nil {
			return "", fmt.Errorf("fingerprint section %s: %w", q.label, err)
		}
		lines := 0
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				return "", fmt.Errorf("fingerprint section %s: scan: %w", q.label, err)
			}
			// Collapse the whitespace inside a definition. pg_get_constraintdef
			// and pg_get_viewdef pretty-print with embedded newlines whose exact
			// indentation is a formatting decision of the server, not a property
			// of the schema; keeping them would make the diff unreadable without
			// making it more sensitive.
			b.WriteString(strings.Join(strings.Fields(line), " "))
			b.WriteByte('\n')
			lines++
		}
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("fingerprint section %s: %w", q.label, err)
		}
		if lines == 0 {
			b.WriteString("(empty)\n")
		}
	}
	return b.String(), nil
}

// pgDumpSchema runs pg_dump inside the engine container and returns the schema
// dump with the two nondeterministic lines removed.
//
// THE FILTER IS NOT A CONVENIENCE. pg_dump on this engine emits a `\restrict
// <random-token>` / `\unrestrict <random-token>` pair around the dump — a
// psql-side guard against a hostile dump being sourced into an interactive
// session. The token is freshly generated per invocation, so two dumps of a
// byte-identical schema differ on exactly those two lines. Measured: with the
// pair removed, two independently migrated containers produce dumps that are
// byte-for-byte equal (3033 lines each), and so does an up -> down -> up cycle on
// one container. Without the filter, every comparison fails for the wrong reason.
//
// stderr is discarded rather than multiplexed into the output: pg_dump warns about
// circular foreign keys on TimescaleDB's own `continuous_agg` catalogue table on
// every run, and interleaving that into the dump makes the comparison depend on
// stream scheduling.
func pgDumpSchema(ctx context.Context, db *database) (string, error) {
	const cmd = `pg_dump --schema-only --no-owner --no-privileges 2>/dev/null`
	out, err := execInContainer(ctx, db, "sh", "-c", cmd)
	if err != nil {
		return "", err
	}

	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, `\restrict `) || strings.HasPrefix(line, `\unrestrict `) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), nil
}

// assertSameText compares two renderings of a schema and, on a difference, prints
// the differing lines rather than two thousand identical ones.
func assertSameText(t *testing.T, what, before, after string) {
	t.Helper()
	if before == after {
		return
	}

	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")

	inBefore := map[string]int{}
	for _, l := range beforeLines {
		inBefore[l]++
	}
	inAfter := map[string]int{}
	for _, l := range afterLines {
		inAfter[l]++
	}

	var only []string
	for l, n := range inBefore {
		if inAfter[l] < n {
			only = append(only, "  only before down/up: "+l)
		}
	}
	for l, n := range inAfter {
		if inBefore[l] < n {
			only = append(only, "  only after  down/up: "+l)
		}
	}
	if len(only) > 24 {
		only = append(only[:24], fmt.Sprintf("  ... and %d more differing lines", len(only)-24))
	}

	t.Errorf("%s is NOT identical across up -> down -> up (%d lines before, %d after).\n"+
		"A migration's Down does not fully reverse its Up, which makes the rollback path a lie.\n%s",
		what, len(beforeLines), len(afterLines), strings.Join(only, "\n"))
}
