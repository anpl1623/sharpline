package integration

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// chunkInterval is what migration 00003 declares for `prices`: 12 hours.
const pricesChunkInterval = 12 * time.Hour

// auditLogChunkInterval is what migration 00007 declares for `audit_log`.
const auditLogChunkInterval = 7 * 24 * time.Hour

// TestPricesIsARealHypertableWithTheDeclaredChunkInterval asserts the partitioning
// exists in the engine, not merely in the migration file.
//
// `CREATE TABLE prices (...)` followed by `SELECT create_hypertable(...)` is two
// statements, and only the first one is checked by anything else in the repo: sqlc
// parses the migration and sees an ordinary table, the fingerprint would still
// match if the create_hypertable call were deleted from both the Up and the Down,
// and every query in internal/platform/postgres/queries works fine against a plain
// table — just without chunk exclusion, which is the property the whole design of
// those queries depends on. So the interval is read out of TimescaleDB's own
// catalogue.
func TestPricesIsARealHypertableWithTheDeclaredChunkInterval(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	conn := rawConn(t, sharedDatabase(t).dsn)

	for _, tc := range []struct {
		hypertable string
		column     string
		interval   time.Duration
		compressed bool
	}{
		{"prices", "observed_at", pricesChunkInterval, true},
		{"audit_log", "occurred_at", auditLogChunkInterval, false},
	} {
		t.Run(tc.hypertable, func(t *testing.T) {
			var column string
			var seconds float64
			err := conn.QueryRow(ctx, `
SELECT column_name, extract(epoch FROM time_interval)
  FROM timescaledb_information.dimensions
 WHERE hypertable_schema = 'public' AND hypertable_name = $1`, tc.hypertable).Scan(&column, &seconds)
			if err != nil {
				if err == pgx.ErrNoRows {
					t.Fatalf("%s has no entry in timescaledb_information.dimensions: it is a plain table, not a hypertable", tc.hypertable)
				}
				t.Fatalf("read the %s dimension: %v", tc.hypertable, err)
			}

			if column != tc.column {
				t.Errorf("%s is partitioned on %q, want %q", tc.hypertable, column, tc.column)
			}
			if got := time.Duration(seconds) * time.Second; got != tc.interval {
				t.Errorf("%s chunk interval is %s, want %s", tc.hypertable, got, tc.interval)
			}

			// One time dimension, not two. A second dimension (space
			// partitioning) would change the chunk count arithmetic every other
			// test in this file relies on.
			if n := scalarInt(t, ctx, conn, `
SELECT num_dimensions FROM timescaledb_information.hypertables
 WHERE hypertable_schema = 'public' AND hypertable_name = $1`, tc.hypertable); n != 1 {
				t.Errorf("%s has %d dimensions, want 1", tc.hypertable, n)
			}

			if got := scalarBool(t, ctx, conn, `
SELECT compression_enabled FROM timescaledb_information.hypertables
 WHERE hypertable_schema = 'public' AND hypertable_name = $1`, tc.hypertable); got != tc.compressed {
				t.Errorf("%s compression_enabled = %v, want %v", tc.hypertable, got, tc.compressed)
			}
		})
	}
}

// TestInsertingPricesAcrossChunkBoundariesCreatesChunks proves the partitioning is
// live rather than declared: rows written on either side of a 12-hour boundary land
// in different chunks, and the chunks come into existence because the rows did.
//
// The window is this test's own (uniqueTimeWindow), 30 days wide, so the chunks
// counted here can only contain rows this test wrote. That is what makes the count
// an assertion instead of a coincidence, and what lets the test run in parallel
// against a shared server.
func TestInsertingPricesAcrossChunkBoundariesCreatesChunks(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db := sharedDatabase(t)
	conn := rawConn(t, db.dsn)

	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)

	// The window starts on a UTC midnight, and TimescaleDB aligns 12-hour chunks
	// to the epoch, so midnight is itself a boundary. Offsets of 0, 13, 26 and 39
	// hours therefore fall in four consecutive, distinct chunks: [0,12), [12,24),
	// [24,36) and [36,48).
	base := uniqueTimeWindow()
	offsets := []time.Duration{0, 13 * time.Hour, 26 * time.Hour, 39 * time.Hour}
	windowEnd := base.Add(30 * 24 * time.Hour)

	if n := countChunks(t, ctx, conn, "prices", base, windowEnd); n != 0 {
		t.Fatalf("this test's window already holds %d chunks before it wrote anything; uniqueTimeWindow is handing out overlapping windows", n)
	}

	type observation struct {
		at   time.Time
		odds float64
	}
	want := make([]observation, 0, len(offsets))
	for i, off := range offsets {
		obs := observation{
			at: base.Add(off),
			// A value with a full float64 mantissa, so the round trip below is a
			// real bit-exactness check and not a comparison of two short decimals.
			odds: 1.9000000000000004 + float64(i)/64,
		}
		want = append(want, obs)

		mustExec(t, ctx, conn, `
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES ($1, $2, $3, NULL, $4, $5)`,
			mkt.HomeSelection, cat.BookID, obs.odds, obs.at, obs.at.Add(120*time.Millisecond))
	}

	// Four rows straddling three boundaries -> four chunks.
	if got := countChunks(t, ctx, conn, "prices", base, windowEnd); got != len(offsets) {
		t.Errorf("wrote %d prices in four distinct 12-hour buckets and got %d chunks, want %d.\n"+
			"Either the chunk interval is not %s or `prices` is not partitioned at all.",
			len(offsets), got, len(offsets), pricesChunkInterval)
	}

	// Every chunk's declared range is exactly one interval wide, which is the
	// property that makes chunk exclusion on a bounded read worth having.
	for _, span := range chunkSpans(t, ctx, conn, "prices", base, windowEnd) {
		if span != pricesChunkInterval {
			t.Errorf("a chunk in this test's window spans %s, want %s", span, pricesChunkInterval)
		}
	}

	// And the rows read back, across the chunk boundaries, in order, exactly.
	rows, err := conn.Query(ctx, `
SELECT decimal_odds, observed_at
  FROM prices
 WHERE selection_id = $1 AND book_id = $2 AND observed_at >= $3 AND observed_at < $4
 ORDER BY observed_at`, mkt.HomeSelection, cat.BookID, base, windowEnd)
	if err != nil {
		t.Fatalf("read prices back: %v", err)
	}
	defer rows.Close()

	var got []observation
	for rows.Next() {
		var o observation
		if err := rows.Scan(&o.odds, &o.at); err != nil {
			t.Fatalf("scan price: %v", err)
		}
		got = append(got, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate prices: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("read %d prices back, wrote %d", len(got), len(want))
	}
	for i := range want {
		if math.Float64bits(got[i].odds) != math.Float64bits(want[i].odds) {
			t.Errorf("price %d: decimal_odds round-tripped as %v (bits %#x), wrote %v (bits %#x); "+
				"DOUBLE PRECISION must be bit-exact — a rounded price is a wrong price",
				i, got[i].odds, math.Float64bits(got[i].odds), want[i].odds, math.Float64bits(want[i].odds))
		}
		if !got[i].at.Equal(want[i].at) {
			t.Errorf("price %d: observed_at round-tripped as %s, wrote %s", i, got[i].at, want[i].at)
		}
	}
}

// TestCompressingAPriceChunkPreservesEveryValueAndThePolicyIsRegistered covers
// both halves of migration 00004's compression work.
//
// The policy is asserted from the catalogue, because a policy that exists only in
// the migration file compresses nothing. The compression itself is forced rather
// than waited for — the policy runs every six hours and a test cannot wait for it —
// and then every value is read back and compared bit-for-bit.
//
// That comparison is the point. Compression rewrites the chunk into a columnar
// layout with the segmentby/orderby columns from 00004; if a float64 lost a bit or
// a timestamptz lost microseconds on the way through, every CLV number ever
// computed from line history would be quietly wrong, and nothing else in the system
// would notice.
func TestCompressingAPriceChunkPreservesEveryValueAndThePolicyIsRegistered(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	db := sharedDatabase(t)
	conn := rawConn(t, db.dsn)

	t.Run("policy is registered", func(t *testing.T) {
		var procName, schedule, compressAfter string
		err := conn.QueryRow(ctx, `
SELECT proc_name, schedule_interval::TEXT, config ->> 'compress_after'
  FROM timescaledb_information.jobs
 WHERE hypertable_schema = 'public'
   AND hypertable_name = 'prices'
   AND proc_name = 'policy_compression'`).Scan(&procName, &schedule, &compressAfter)
		if err != nil {
			if err == pgx.ErrNoRows {
				t.Fatal("no compression policy is registered on `prices`; migration 00004's add_compression_policy did not take effect")
			}
			t.Fatalf("read the compression policy: %v", err)
		}
		if compressAfter != "7 days" {
			t.Errorf("compression policy compresses after %q, migration 00004 declares 7 days", compressAfter)
		}
		if schedule != "06:00:00" {
			t.Errorf("compression policy runs every %q, migration 00004 declares 6 hours", schedule)
		}

		// The compression settings themselves — which columns segment and which
		// order — are what make the columnar layout useful rather than merely
		// smaller. An empty result means compression is enabled with defaults,
		// which is not what 00004 asked for.
		if n := scalarInt(t, ctx, conn, `
SELECT count(*) FROM timescaledb_information.compression_settings
 WHERE hypertable_name = 'prices'`); n == 0 {
			t.Error("`prices` has compression enabled but no compression_settings; segmentby/orderby were not configured")
		}
	})

	// ---- force-compress a chunk this test owns -----------------------------
	cat := newCatalogue(t, ctx, conn)
	mkt := newMoneylineMarket(t, ctx, conn, cat)

	base := uniqueTimeWindow()
	windowEnd := base.Add(30 * 24 * time.Hour)

	type row struct {
		selection string
		odds      float64
		line      *float64
		observed  time.Time
		ingested  time.Time
	}
	lineValue := -3.5
	var want []row
	for i := range 12 {
		// All inside ONE 12-hour bucket, so exactly one chunk is compressed and
		// the assertion below can name it.
		at := base.Add(time.Duration(i) * 37 * time.Minute)
		r := row{
			selection: string(mkt.HomeSelection),
			odds:      2.0000000000000004 + float64(i)/128,
			observed:  at,
			ingested:  at.Add(time.Duration(i) * 13 * time.Millisecond),
		}
		if i%2 == 0 {
			// Half the rows carry a NULL line, half a value: compression handles
			// nulls through a separate bitmap, so a test that wrote only non-null
			// values would not exercise it.
			v := lineValue
			r.line = &v
		}
		want = append(want, r)

		mustExec(t, ctx, conn, `
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
			mkt.HomeSelection, cat.BookID, r.odds, r.line, r.observed, r.ingested)
	}

	chunks := chunkNames(t, ctx, conn, "prices", base, windowEnd)
	if len(chunks) != 1 {
		t.Fatalf("expected the 12 rows to land in exactly 1 chunk, got %d: %v", len(chunks), chunks)
	}
	chunk := chunks[0]

	before := readPrices(t, ctx, conn, mkt.HomeSelection, base, windowEnd)
	if len(before) != len(want) {
		t.Fatalf("read %d rows before compression, wrote %d", len(before), len(want))
	}

	// The policy job could in principle have compressed this chunk first — the
	// window is far in the past, so it is older than compress_after. Tolerating
	// that is correct: what the test asserts is the state after compression, not
	// who caused it.
	if !chunkIsCompressed(t, ctx, conn, chunk) {
		if _, err := conn.Exec(ctx, `SELECT compress_chunk($1::REGCLASS)`, chunk); err != nil {
			t.Fatalf("compress_chunk(%s): %v", chunk, err)
		}
	}
	if !chunkIsCompressed(t, ctx, conn, chunk) {
		t.Fatalf("%s reports is_compressed = false after compress_chunk returned successfully", chunk)
	}

	after := readPrices(t, ctx, conn, mkt.HomeSelection, base, windowEnd)
	assertPricesIdentical(t, "after compression", before, after)

	// Decompressing must be lossless too — it is the read path a backfill or a
	// schema change takes.
	if _, err := conn.Exec(ctx, `SELECT decompress_chunk($1::REGCLASS)`, chunk); err != nil {
		t.Fatalf("decompress_chunk(%s): %v", chunk, err)
	}
	if chunkIsCompressed(t, ctx, conn, chunk) {
		t.Fatalf("%s still reports is_compressed = true after decompress_chunk", chunk)
	}
	assertPricesIdentical(t, "after decompression", before,
		readPrices(t, ctx, conn, mkt.HomeSelection, base, windowEnd))
}

// -----------------------------------------------------------------------------
// Chunk helpers
// -----------------------------------------------------------------------------

func countChunks(t *testing.T, ctx context.Context, conn *pgx.Conn, hypertable string, from, to time.Time) int {
	t.Helper()
	return scalarInt(t, ctx, conn, `
SELECT count(*)
  FROM timescaledb_information.chunks
 WHERE hypertable_schema = 'public' AND hypertable_name = $1
   AND range_start >= $2 AND range_end <= $3`, hypertable, from, to)
}

func chunkNames(t *testing.T, ctx context.Context, conn *pgx.Conn, hypertable string, from, to time.Time) []string {
	t.Helper()
	return stringColumn(t, ctx, conn, `
SELECT format('%I.%I', chunk_schema, chunk_name)
  FROM timescaledb_information.chunks
 WHERE hypertable_schema = 'public' AND hypertable_name = $1
   AND range_start >= $2 AND range_end <= $3
 ORDER BY range_start`, hypertable, from, to)
}

func chunkSpans(t *testing.T, ctx context.Context, conn *pgx.Conn, hypertable string, from, to time.Time) []time.Duration {
	t.Helper()
	rows, err := conn.Query(ctx, `
SELECT extract(epoch FROM (range_end - range_start))
  FROM timescaledb_information.chunks
 WHERE hypertable_schema = 'public' AND hypertable_name = $1
   AND range_start >= $2 AND range_end <= $3
 ORDER BY range_start`, hypertable, from, to)
	if err != nil {
		t.Fatalf("read chunk spans: %v", err)
	}
	defer rows.Close()

	var out []time.Duration
	for rows.Next() {
		var seconds float64
		if err := rows.Scan(&seconds); err != nil {
			t.Fatalf("scan chunk span: %v", err)
		}
		out = append(out, time.Duration(seconds)*time.Second)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate chunk spans: %v", err)
	}
	return out
}

func chunkIsCompressed(t *testing.T, ctx context.Context, conn *pgx.Conn, chunk string) bool {
	t.Helper()
	return scalarBool(t, ctx, conn, `
SELECT is_compressed
  FROM timescaledb_information.chunks
 WHERE format('%I.%I', chunk_schema, chunk_name) = $1`, chunk)
}

// priceRow is one row of `prices` as read back, kept comparable so that
// "identical" means identical rather than approximately equal.
type priceRow struct {
	Odds     float64
	Line     *float64
	Observed time.Time
	Ingested time.Time
}

func readPrices(t *testing.T, ctx context.Context, conn *pgx.Conn, selection any, from, to time.Time) []priceRow {
	t.Helper()

	rows, err := conn.Query(ctx, `
SELECT decimal_odds, line, observed_at, ingested_at
  FROM prices
 WHERE selection_id = $1 AND observed_at >= $2 AND observed_at < $3
 ORDER BY observed_at`, selection, from, to)
	if err != nil {
		t.Fatalf("read prices: %v", err)
	}
	defer rows.Close()

	var out []priceRow
	for rows.Next() {
		var r priceRow
		if err := rows.Scan(&r.Odds, &r.Line, &r.Observed, &r.Ingested); err != nil {
			t.Fatalf("scan price: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate prices: %v", err)
	}
	return out
}

func assertPricesIdentical(t *testing.T, when string, want, got []priceRow) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s: %d rows, want %d", when, len(got), len(want))
	}
	for i := range want {
		if math.Float64bits(got[i].Odds) != math.Float64bits(want[i].Odds) {
			t.Errorf("%s: row %d decimal_odds is %v (bits %#x), want %v (bits %#x)",
				when, i, got[i].Odds, math.Float64bits(got[i].Odds),
				want[i].Odds, math.Float64bits(want[i].Odds))
		}
		if !sameNullableFloat(want[i].Line, got[i].Line) {
			t.Errorf("%s: row %d line is %s, want %s", when, i, showFloat(got[i].Line), showFloat(want[i].Line))
		}
		if !got[i].Observed.Equal(want[i].Observed) {
			t.Errorf("%s: row %d observed_at is %s, want %s", when, i, got[i].Observed, want[i].Observed)
		}
		if !got[i].Ingested.Equal(want[i].Ingested) {
			t.Errorf("%s: row %d ingested_at is %s, want %s", when, i, got[i].Ingested, want[i].Ingested)
		}
	}
}

func sameNullableFloat(a, b *float64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return math.Float64bits(*a) == math.Float64bits(*b)
	}
}

func showFloat(v *float64) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("%v", *v)
}
