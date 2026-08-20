// The Timescale writer against a real TimescaleDB, a real migrated schema and
// the real domain constructors.
//
// # Why these are integration tests and not unit tests
//
// CLAUDE.md §10 forbids a mocked database, and everything worth asserting here
// is a property of the engine rather than of this package's Go code: that a
// hypertable accepts ON CONFLICT at all, that the natural-key index absorbs a
// redelivery, that the append-only triggers refuse an UPDATE with SQLSTATE
// 23001, that `RETURNING (xmax = 0)` distinguishes an insert from an update,
// that a partial unique index rolls the whole transaction back. A fake would
// reproduce the API and none of the behaviour.
//
// harness_test.go owns the container, the migration run and the payload builder.
// Read its header first — in particular the NO MOCK DATA note: every row
// asserted on here was produced by the writer from a payload the calling test
// built, and nothing is seeded.
//
// # These tests are deliberately NOT parallel
//
// They were, and the whole file failed at once with
// `deadlock detected (SQLSTATE 40P01)` out of the price insert. The cause is not
// in this package: a dozen transactions concurrently inserting the FIRST row of
// a `prices` chunk that does not exist yet make TimescaleDB create that chunk
// concurrently, and the catalogue locks that takes can cycle. Every transaction
// used the same observation instant, so every one of them wanted the same 12-hour
// chunk.
//
// Serialising them removes contention this package's own design does not have —
// internal/platform/kafka's Consumer delivers records to a handler sequentially,
// one goroutine, one record at a time. But the production case is NOT hypothetical
// (several `ingest` replicas write concurrently and a new chunk opens every 12
// hours), so it is written up as a finding rather than hidden by this comment.
// The consequence there is bounded: postgres.InTx does not retry a failed
// transaction, the handler returns the error, and the record is redelivered or
// skipped by the ErrorPolicy the Consumer was built with.
package writer_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/writer"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// -----------------------------------------------------------------------------
// Delivery construction
// -----------------------------------------------------------------------------

// delivery frames a Record the way internal/platform/kafka would deliver it.
//
// The envelope is built here rather than round-tripped through a broker because
// what is under test is the HANDLER, and the envelope's own encoding already has
// its own tests in internal/platform/kafka. The topic is the real
// TopicOddsNormalized constant, so Delivery.MarketID's key-kind check runs for
// real — a test topic would make it a no-op and hide the one place an untyped
// key could be given the wrong type back.
func delivery(t *testing.T, key domain.MarketID, p writer.Record, offset int64) *kafka.Delivery {
	t.Helper()

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return &kafka.Delivery{
		Topic:     kafka.TopicOddsNormalized,
		Partition: 0,
		Offset:    offset,
		Key:       string(key),
		Timestamp: time.Now().UTC(),
		Headers:   map[string]string{},
		Envelope: kafka.Envelope{
			Version:  kafka.EnvelopeVersion,
			Type:     writer.MessageType,
			Producer: "writer-it",
			Data:     data,
		},
	}
}

// tombstone frames a deletion of key on the compacted topic.
func tombstone(key domain.MarketID, offset int64) *kafka.Delivery {
	return &kafka.Delivery{
		Topic:           kafka.TopicOddsNormalized,
		Partition:       0,
		Offset:          offset,
		Key:             string(key),
		Timestamp:       time.Now().UTC(),
		Headers:         map[string]string{},
		Tombstone:       true,
		TombstoneReason: "market suspended by the admin console",
	}
}

// handle runs one record through the writer with a bounded context.
func handle(t *testing.T, w *writer.Writer, d *kafka.Delivery) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return w.HandleMessage(ctx, d)
}

// -----------------------------------------------------------------------------
// Row readers
// -----------------------------------------------------------------------------

type priceRow struct {
	selectionID string
	bookID      string
	decimal     float64
	line        *float64
	observedAt  time.Time
	ingestedAt  time.Time
	createdAt   time.Time
}

// pricesFor reads every row this market's selections have ever produced, in the
// natural key's order.
func pricesFor(t *testing.T, db *postgres.DB, m *market) []priceRow {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := db.Pool().Query(ctx, `
		SELECT selection_id, book_id, decimal_odds, line, observed_at, ingested_at, created_at
		  FROM prices
		 WHERE selection_id = ANY($1)
		 ORDER BY selection_id, book_id, observed_at`,
		[]string{string(m.homeSel), string(m.awaySel)})
	if err != nil {
		t.Fatalf("read prices: %v", err)
	}
	defer rows.Close()

	var out []priceRow
	for rows.Next() {
		var r priceRow
		if err := rows.Scan(&r.selectionID, &r.bookID, &r.decimal, &r.line,
			&r.observedAt, &r.ingestedAt, &r.createdAt); err != nil {
			t.Fatalf("scan price: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read prices: %v", err)
	}
	return out
}

// scalar reads a single value, failing the test on any error including no rows.
func scalar[T any](t *testing.T, db *postgres.DB, sql string, args ...any) T {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var v T
	if err := db.Pool().QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

// exec runs a statement and returns its error, for the append-only guards.
func exec(t *testing.T, db *postgres.DB, sql string, args ...any) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := db.Pool().Exec(ctx, sql, args...)
	return err
}

// -----------------------------------------------------------------------------
// Construction — no container needed
// -----------------------------------------------------------------------------

func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()

	cases := map[string]writer.Options{
		"no db":                  {Logger: discardLogger()},
		"no logger":              {DB: stubDB{}},
		"negative rows per stmt": {DB: stubDB{}, Logger: discardLogger(), MaxRowsPerStatement: -1},
		"negative flush timeout": {DB: stubDB{}, Logger: discardLogger(), FlushTimeout: -time.Second},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if w, err := writer.New(opts); err == nil {
				t.Fatalf("New(%s) = (%v, nil), want an error", name, w)
			}
		})
	}
}

// stubDB satisfies writer.DB for the construction tests only. It is never given
// a record to handle — a test that exercised the write path against it would be
// the mocked database CLAUDE.md §10 forbids.
type stubDB struct{}

func (stubDB) InTx(context.Context, postgres.TxFunc) error {
	panic("stubDB is for construction tests only; it must never handle a record")
}

func TestRunRefusesWithoutAConsumer(t *testing.T) {
	t.Parallel()

	w, _ := newWriter(t, stubDB{})
	if err := w.Run(context.Background(), nil); !errors.Is(err, writer.ErrNotRunnable) {
		t.Fatalf("Run(nil consumer) = %v, want ErrNotRunnable", err)
	}
}

// TestInjectedMetricsTakePrecedence covers the multi-Writer case Options.Metrics
// exists for: one registration, several Writers.
func TestInjectedMetricsTakePrecedence(t *testing.T) {
	t.Parallel()

	shared, err := writer.NewMetrics(nil)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	for i := 0; i < 2; i++ {
		w, err := writer.New(writer.Options{DB: stubDB{}, Logger: discardLogger(), Metrics: shared})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if w.Metrics() != shared {
			t.Error("Writer.Metrics() is not the injected set")
		}
	}
}

// -----------------------------------------------------------------------------
// The happy path
// -----------------------------------------------------------------------------

// TestWriterWritesTheSpineAndTheQuotes is the base case every other test varies.
func TestWriterWritesTheSpineAndTheQuotes(t *testing.T) {
	db := openPool(t)
	w, reg := newWriter(t, db)
	m := newMarket(t)

	if err := handle(t, w, delivery(t, m.marketID, m.payload(), 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	rows := pricesFor(t, db, m)
	if len(rows) != 4 {
		t.Fatalf("prices has %d rows, want 4 (2 selections × 2 books)", len(rows))
	}

	// observed_at is the PROVIDER's instant and the hypertable's partitioning
	// column; ingested_at is ours. Conflating them would make the staleness SLO
	// report perfect health for stale data.
	for _, r := range rows {
		if !r.observedAt.Equal(m.quoteObsAt) {
			t.Errorf("row %s/%s observed_at = %s, want the provider instant %s",
				r.selectionID, r.bookID, r.observedAt, m.quoteObsAt)
		}
		if !r.ingestedAt.Equal(m.ingestedAt) {
			t.Errorf("row %s/%s ingested_at = %s, want the payload's own value %s",
				r.selectionID, r.bookID, r.ingestedAt, m.ingestedAt)
		}
		if r.createdAt.Before(r.ingestedAt) {
			t.Errorf("row %s/%s created_at %s precedes ingested_at %s",
				r.selectionID, r.bookID, r.createdAt, r.ingestedAt)
		}
		if r.line == nil {
			t.Errorf("row %s/%s has a NULL line; a spread quote carries the line it was made at",
				r.selectionID, r.bookID)
		}
	}

	// The away side's line is the home side's, inverted. That distinction is
	// what migrations/00003 says CLV depends on, so it must survive the wire and
	// the column.
	homeLine, _ := m.homeLine.Value()
	awayLine, _ := m.awayLine.Value()
	for _, r := range rows {
		want := homeLine
		if r.selectionID == string(m.awaySel) {
			want = awayLine
		}
		if r.line == nil || *r.line != want {
			t.Errorf("row %s/%s line = %v, want %v", r.selectionID, r.bookID, r.line, want)
		}
	}

	// The spine, in the hierarchy CLAUDE.md §4 defines. Nothing else in the
	// system creates it — migrations/00002 leaves these tables deliberately
	// unseeded — so its presence is the writer's own work.
	if got := scalar[int64](t, db, `SELECT count(*) FROM sports WHERE id = $1`, string(m.sportID)); got != 1 {
		t.Errorf("sports rows for this market = %d, want 1", got)
	}
	if got := scalar[int64](t, db, `SELECT count(*) FROM leagues WHERE id = $1`, string(m.leagueID)); got != 1 {
		t.Errorf("leagues rows = %d, want 1", got)
	}
	if got := scalar[int64](t, db, `SELECT count(*) FROM events WHERE id = $1`, string(m.eventID)); got != 1 {
		t.Errorf("events rows = %d, want 1", got)
	}
	if got := scalar[int64](t, db, `SELECT count(*) FROM markets WHERE id = $1`, string(m.marketID)); got != 1 {
		t.Errorf("markets rows = %d, want 1", got)
	}
	if got := scalar[int64](t, db,
		`SELECT count(*) FROM selections WHERE market_id = $1`, string(m.marketID)); got != 2 {
		t.Errorf("selections rows = %d, want 2", got)
	}
	if got := scalar[int64](t, db,
		`SELECT count(*) FROM books WHERE id = ANY($1)`,
		[]string{string(m.bookA), string(m.bookB)}); got != 2 {
		t.Errorf("books rows = %d, want 2", got)
	}

	// events.observed_at holds the PROVIDER instant; updated_at is row-write
	// metadata stamped by the schema-wide trigger. The phase-2 handoff is
	// explicit that the two must never be conflated.
	if got := scalar[time.Time](t, db,
		`SELECT observed_at FROM events WHERE id = $1`, string(m.eventID)); !got.Equal(m.eventObsAt()) {
		t.Errorf("events.observed_at = %s, want the provider instant %s", got, m.eventObsAt())
	}

	if got := counterValue(t, reg, "sharpline_writer_messages_total",
		map[string]string{"outcome": "written"}); got != 1 {
		t.Errorf("messages_total{written} = %v, want 1", got)
	}
	if got := counterValue(t, reg, "sharpline_writer_price_rows_total",
		map[string]string{"outcome": "inserted"}); got != 4 {
		t.Errorf("price_rows_total{inserted} = %v, want 4", got)
	}
	if got := counterValue(t, reg, "sharpline_writer_price_rows_total",
		map[string]string{"outcome": "duplicate"}); got != 0 {
		t.Errorf("price_rows_total{duplicate} = %v on a first write, want 0", got)
	}
	if got := histogramCount(t, reg, "sharpline_writer_flush_duration_seconds",
		map[string]string{"outcome": "ok"}); got != 1 {
		t.Errorf("flush_duration_seconds{ok} count = %d, want 1", got)
	}
	if got, ok := histogramSum(t, reg, "sharpline_writer_batch_rows", map[string]string{}); !ok || got != 4 {
		t.Errorf("batch_rows sum = %v (present=%v), want 4", got, ok)
	}
	for _, table := range []string{"sports", "leagues", "events", "markets", "books", "selections"} {
		if got := counterValue(t, reg, "sharpline_writer_catalogue_upserts_total",
			map[string]string{"table": table, "outcome": "inserted"}); got == 0 {
			t.Errorf("catalogue_upserts_total{table=%q,outcome=\"inserted\"} = 0, want > 0", table)
		}
	}

	// One successful record touches every series this package owns, so the
	// GATHERED set — what a Prometheus scrape would actually pull — must be
	// exactly the declared contract. metrics_test.go asserts the names from the
	// descriptors, which holds before anything has been observed; this asserts
	// that observing a record produces those same names and nothing else.
	scraped := map[string]bool{}
	for _, n := range metricNames(t, reg) {
		scraped[n] = true
	}
	for _, n := range declaredNames(t) {
		if !scraped[n] {
			t.Errorf("%s is declared but was not emitted by a successful write; a series that never "+
				"appears in a scrape is a dashboard panel that says No data", n)
		}
		delete(scraped, n)
	}
	for n := range scraped {
		t.Errorf("a successful write emitted %s, which NewMetrics does not declare", n)
	}
}

// -----------------------------------------------------------------------------
// At-least-once delivery
// -----------------------------------------------------------------------------

// TestRedeliveryDoesNotDuplicateLineHistory is the load-bearing test of this
// package.
//
// The Consumer commits the last successfully handled record per partition, so a
// rebalance redelivers whatever was handled but not yet committed. A duplicated
// row would distort every line-movement and CLV computation built on this table
// — silently, because nothing downstream can tell a real repeat quote from a
// replayed one. The natural key (selection, book, observed_at) plus ON CONFLICT
// DO NOTHING is what makes the second delivery a no-op, and this asserts it
// against the real index rather than against the intention.
func TestRedeliveryDoesNotDuplicateLineHistory(t *testing.T) {
	db := openPool(t)
	w, reg := newWriter(t, db)
	m := newMarket(t)

	d := delivery(t, m.marketID, m.payload(), 1)
	for i := 0; i < 3; i++ {
		if err := handle(t, w, d); err != nil {
			t.Fatalf("HandleMessage delivery %d: %v", i+1, err)
		}
	}

	rows := pricesFor(t, db, m)
	if len(rows) != 4 {
		t.Fatalf("prices has %d rows after three deliveries of one record, want 4", len(rows))
	}

	if got := counterValue(t, reg, "sharpline_writer_price_rows_total",
		map[string]string{"outcome": "inserted"}); got != 4 {
		t.Errorf("price_rows_total{inserted} = %v, want 4", got)
	}
	if got := counterValue(t, reg, "sharpline_writer_price_rows_total",
		map[string]string{"outcome": "duplicate"}); got != 8 {
		t.Errorf("price_rows_total{duplicate} = %v, want 8 (two redeliveries × four quotes)", got)
	}
	// A nil return is a claim of durability, so all three deliveries count as
	// written even though only the first stored anything.
	if got := counterValue(t, reg, "sharpline_writer_messages_total",
		map[string]string{"outcome": "written"}); got != 3 {
		t.Errorf("messages_total{written} = %v, want 3", got)
	}
}

// TestUnchangedCatalogueRowsAreNotRewritten. The steady state is the same market
// re-asserted on every poll; without the distinctness guard that would be an
// UPDATE per row per record, each one firing set_updated_at and writing a new
// row version for no change.
//
// updated_at is the witness precisely because it is written by a trigger rather
// than by this package: if the row were rewritten, the database's own clock
// would move it.
func TestUnchangedCatalogueRowsAreNotRewritten(t *testing.T) {
	db := openPool(t)
	w, reg := newWriter(t, db)
	m := newMarket(t)

	d := delivery(t, m.marketID, m.payload(), 1)
	if err := handle(t, w, d); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	first := scalar[time.Time](t, db, `SELECT updated_at FROM events WHERE id = $1`, string(m.eventID))

	if err := handle(t, w, d); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	second := scalar[time.Time](t, db, `SELECT updated_at FROM events WHERE id = $1`, string(m.eventID))

	if !first.Equal(second) {
		t.Errorf("events.updated_at moved from %s to %s on an identical re-assertion; the "+
			"IS DISTINCT FROM guard is not preventing the rewrite", first, second)
	}
	for _, table := range []string{"sports", "leagues", "events", "markets", "books", "selections"} {
		if got := counterValue(t, reg, "sharpline_writer_catalogue_upserts_total",
			map[string]string{"table": table, "outcome": "unchanged"}); got == 0 {
			t.Errorf("catalogue_upserts_total{table=%q,outcome=\"unchanged\"} = 0 after an identical "+
				"re-assertion, want > 0", table)
		}
	}
}

// TestANewObservationAppends: a moved line is a NEW ROW, not an edit. CLAUDE.md
// §4: "Immutable; a new price is a new row."
func TestANewObservationAppends(t *testing.T) {
	db := openPool(t)
	w, reg := newWriter(t, db)
	m := newMarket(t)

	if err := handle(t, w, delivery(t, m.marketID, m.payload(), 1)); err != nil {
		t.Fatalf("first observation: %v", err)
	}

	// One minute later the favourite shortens.
	m.quoteObsAt = m.quoteObsAt.Add(time.Minute)
	m.marketObsAt = m.marketObsAt.Add(time.Minute)
	m.ingestedAt = m.ingestedAt.Add(time.Minute)
	m.homeDecimal = 1.83
	if err := handle(t, w, delivery(t, m.marketID, m.payload(), 2)); err != nil {
		t.Fatalf("second observation: %v", err)
	}

	rows := pricesFor(t, db, m)
	if len(rows) != 8 {
		t.Fatalf("prices has %d rows after two distinct observations, want 8", len(rows))
	}

	// Both observations of the home price at book A must survive: the earlier
	// one is the history a line-movement chart is drawn from.
	var homeAtA []float64
	for _, r := range rows {
		if r.selectionID == string(m.homeSel) && r.bookID == string(m.bookA) {
			homeAtA = append(homeAtA, r.decimal)
		}
	}
	if len(homeAtA) != 2 {
		t.Fatalf("home price at book A has %d rows, want 2", len(homeAtA))
	}
	if homeAtA[0] != 1.91 || homeAtA[1] != 1.83 {
		t.Errorf("home prices at book A = %v, want [1.91 1.83] in observation order", homeAtA)
	}

	if got := counterValue(t, reg, "sharpline_writer_price_rows_total",
		map[string]string{"outcome": "duplicate"}); got != 0 {
		t.Errorf("price_rows_total{duplicate} = %v across two distinct instants, want 0", got)
	}
}

// -----------------------------------------------------------------------------
// The append-only guards
// -----------------------------------------------------------------------------

// TestPricesRefuseUpdateDeleteAndTruncate proves there is no upsert path to
// find. The triggers are the reason prices.go uses ON CONFLICT DO NOTHING and
// never DO UPDATE, so the guard and the statement are two halves of one design.
func TestPricesRefuseUpdateDeleteAndTruncate(t *testing.T) {
	db := openPool(t)
	w, _ := newWriter(t, db)
	m := newMarket(t)

	if err := handle(t, w, delivery(t, m.marketID, m.payload(), 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	cases := map[string]struct {
		sql  string
		args []any
	}{
		"update": {`UPDATE prices SET decimal_odds = 2.0 WHERE selection_id = $1`,
			[]any{string(m.homeSel)}},
		"delete": {`DELETE FROM prices WHERE selection_id = $1`, []any{string(m.homeSel)}},
		// TRUNCATE is not parameterisable and would hit every test's rows, so it
		// is left to migrations/00003's own verification block. UPDATE and DELETE
		// are the two an application could plausibly reach for.
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := exec(t, db, tc.sql, tc.args...)
			if err == nil {
				t.Fatalf("%s against prices succeeded; the append-only trigger is missing", name)
			}
			if got := postgres.SQLState(err); got != "23001" {
				t.Errorf("%s against prices returned SQLSTATE %q, want 23001 (restrict_violation): %v",
					name, got, err)
			}
		})
	}

	if got := len(pricesFor(t, db, m)); got != 4 {
		t.Errorf("prices has %d rows after the refused mutations, want the original 4", got)
	}
}

// -----------------------------------------------------------------------------
// Out-of-order delivery
// -----------------------------------------------------------------------------

// TestOutOfOrderCatalogueObservationIsDiscarded.
//
// Kafka orders records within a partition, not across them, and a redelivery
// after a rebalance can land after a newer record has already been applied.
// Without the monotonicity guard a replayed record would roll a live event back
// to `scheduled` — a board that says a game has not started while it is being
// played.
func TestOutOfOrderCatalogueObservationIsDiscarded(t *testing.T) {
	db := openPool(t)
	w, _ := newWriter(t, db)
	m := newMarket(t)

	older := m.payload()

	// The newer observation: the game has gone live and the market is halted.
	// The quotes move with it, so the two records carry different natural keys
	// and both sets of rows are real line history.
	m.marketObsAt = m.marketObsAt.Add(5 * time.Minute)
	m.quoteObsAt = m.quoteObsAt.Add(5 * time.Minute)
	m.eventStatus = domain.EventStatusLive
	m.marketStatus = domain.MarketStatusSuspended
	if err := handle(t, w, delivery(t, m.marketID, m.payload(), 2)); err != nil {
		t.Fatalf("newer observation: %v", err)
	}

	// The replay arrives afterwards and must change nothing about the catalogue.
	if err := handle(t, w, delivery(t, m.marketID, older, 1)); err != nil {
		t.Fatalf("replayed older observation: %v", err)
	}

	if got := scalar[string](t, db,
		`SELECT status FROM events WHERE id = $1`, string(m.eventID)); got != domain.EventStatusLive.String() {
		t.Errorf("events.status = %q after a replayed older observation, want %q — the replay rolled "+
			"the event back", got, domain.EventStatusLive)
	}
	if got := scalar[string](t, db,
		`SELECT status FROM markets WHERE id = $1`, string(m.marketID)); got != domain.MarketStatusSuspended.String() {
		t.Errorf("markets.status = %q after a replayed older observation, want %q",
			got, domain.MarketStatusSuspended)
	}

	// The PRICES are a different matter and both observations belong: the older
	// record's quotes carry their own observed_at and are line history, not a
	// stale copy of the current state.
	if got := len(pricesFor(t, db, m)); got != 8 {
		t.Errorf("prices has %d rows, want 8 — a replayed record's quotes are history at their own "+
			"instants, not a rollback", got)
	}
}

// -----------------------------------------------------------------------------
// Tombstones
// -----------------------------------------------------------------------------

// TestTombstoneRetainsLineHistory. A deletion on the compacted topic removes the
// market from the current-line snapshot and says nothing about history — and
// prices_no_delete would refuse to remove a row anyway.
func TestTombstoneRetainsLineHistory(t *testing.T) {
	db := openPool(t)
	w, reg := newWriter(t, db)
	m := newMarket(t)

	if err := handle(t, w, delivery(t, m.marketID, m.payload(), 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if err := handle(t, w, tombstone(m.marketID, 2)); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	if got := len(pricesFor(t, db, m)); got != 4 {
		t.Errorf("prices has %d rows after a tombstone, want the original 4", got)
	}
	// No catalogue mutation is invented either: the tombstone carries no market
	// status, so writing one would be fabricating a transition the provider
	// never reported.
	if got := scalar[string](t, db,
		`SELECT status FROM markets WHERE id = $1`, string(m.marketID)); got != domain.MarketStatusOpen.String() {
		t.Errorf("markets.status = %q after a tombstone, want it untouched (%q)",
			got, domain.MarketStatusOpen)
	}
	if got := counterValue(t, reg, "sharpline_writer_messages_total",
		map[string]string{"outcome": "tombstone"}); got != 1 {
		t.Errorf("messages_total{tombstone} = %v, want 1", got)
	}
}

// -----------------------------------------------------------------------------
// Rejected records
// -----------------------------------------------------------------------------

// TestRejectedRecordsWriteNothing covers every permanent failure, and asserts the
// same two things about each: the error identifies the defect, and the database
// is untouched.
func TestRejectedRecordsWriteNothing(t *testing.T) {
	db := openPool(t)

	cases := map[string]struct {
		build func(t *testing.T, m *market) *kafka.Delivery
		want  error
	}{
		"wrong message type": {
			build: func(t *testing.T, m *market) *kafka.Delivery {
				d := delivery(t, m.marketID, m.payload(), 1)
				d.Envelope.Type = "odds.normalized.v2"
				return d
			},
			want: writer.ErrWrongMessageType,
		},
		"key does not match the payload": {
			// The key is what compaction and partitioning act on, so a mismatch
			// means this market's snapshot lives under another market's key and
			// is deleted by the next write to either.
			build: func(t *testing.T, m *market) *kafka.Delivery {
				return delivery(t, domain.MarketID("market-someone-else-"+m.suffix), m.payload(), 1)
			},
			want: writer.ErrKeyMismatch,
		},
		"quote references an undeclared book": {
			build: func(t *testing.T, m *market) *kafka.Delivery {
				p := m.payload()
				p.Prices[0].BookID = string("book-undeclared-" + m.suffix)
				return delivery(t, m.marketID, p, 1)
			},
			want: writer.ErrIncompletePayload,
		},
		"quote references an undeclared selection": {
			build: func(t *testing.T, m *market) *kafka.Delivery {
				p := m.payload()
				p.Prices[0].SelectionID = string("sel-undeclared-" + m.suffix)
				return delivery(t, m.marketID, p, 1)
			},
			want: writer.ErrIncompletePayload,
		},
		"two quotes share a natural key": {
			// ON CONFLICT DO NOTHING would absorb this silently, and two
			// different prices for the same (selection, book, instant) in ONE
			// snapshot is a normalizer defect: whichever arrived first would win
			// arbitrarily.
			build: func(t *testing.T, m *market) *kafka.Delivery {
				p := m.payload()
				p.Prices = append(p.Prices, p.Prices[0])
				return delivery(t, m.marketID, p, 1)
			},
			want: writer.ErrIncompletePayload,
		},
		"no ingested_at": {
			build: func(t *testing.T, m *market) *kafka.Delivery {
				p := m.payload()
				p.IngestedAt = time.Time{}
				return delivery(t, m.marketID, p, 1)
			},
			want: writer.ErrIncompletePayload,
		},
		"event names a different league": {
			build: func(t *testing.T, m *market) *kafka.Delivery {
				p := m.payload()
				p.Event.LeagueID = string("league-elsewhere-" + m.suffix)
				return delivery(t, m.marketID, p, 1)
			},
			want: writer.ErrIncompletePayload,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w, reg := newWriter(t, db)
			m := newMarket(t)

			err := handle(t, w, tc.build(t, m))
			if !errors.Is(err, tc.want) {
				t.Fatalf("HandleMessage = %v, want an error wrapping %v", err, tc.want)
			}
			if got := len(pricesFor(t, db, m)); got != 0 {
				t.Errorf("a rejected record left %d price rows behind, want 0", got)
			}
			if got := scalar[int64](t, db,
				`SELECT count(*) FROM sports WHERE id = $1`, string(m.sportID)); got != 0 {
				t.Errorf("a rejected record left %d sport rows behind, want 0", got)
			}
			if got := counterValue(t, reg, "sharpline_writer_messages_total",
				map[string]string{"outcome": "invalid"}); got != 1 {
				t.Errorf("messages_total{invalid} = %v, want 1", got)
			}
		})
	}
}

// TestATransactionThatFailsMidwayLeavesNothing.
//
// The whole record is one transaction on purpose: a price row whose selection
// was written by a transaction that later rolled back would be a foreign-key
// violation, and a selection written without its prices would leave a market on
// the board with no quotes. This forces a failure the DATABASE raises rather
// than one the payload validator catches, because those two take different paths
// out of HandleMessage and only this one exercises the rollback.
//
// The trigger is books.slug's UNIQUE index: the record declares two DIFFERENT
// book ids sharing one slug. The writer's own validator rejects duplicate book
// IDs and says nothing about slugs, so this failure can only come from the
// database — which is exactly what the test needs.
//
// It used to be books_reference_unique_idx (two books both flagged as the
// reference book). That trigger is unreachable from a real record: the wire
// contract carries no reference flag at all — see the note in doc.go — so a
// payload cannot declare even ONE reference book, let alone two. Asserting a
// rollback through a state the pipeline cannot produce would be testing the
// harness rather than the writer.
// TestAnUnchangedCatalogueRowIsNotLocked is the phase-9 regression guard on the
// LOCK rather than on the write.
//
// TestUnchangedCatalogueRowsAreNotRewritten already proves the distinctness
// guard stops the UPDATE. It does not prove the row is left alone, and it cannot:
// ON CONFLICT DO UPDATE takes an exclusive row lock on the conflicting row
// BEFORE evaluating that WHERE and holds it to COMMIT, so a statement that writes
// nothing was still blocking every foreign-key check against the row for the life
// of the transaction. That is what deadlocked against the signals stage, whose
// ev_signals insert takes FOR KEY SHARE on the same books, leagues and selections
// rows in a different order.
//
// The test holds exactly the lock a foreign-key check holds — FOR KEY SHARE, from
// a second connection — and requires the writer to commit anyway, under a flush
// timeout short enough that blocking is a failure rather than a delay. Against the
// statement as it stood before the anti-join, this fails by timeout.
func TestAnUnchangedCatalogueRowIsNotLocked(t *testing.T) {
	db := openPool(t)
	w, _ := newWriter(t, db, func(o *writer.Options) { o.FlushTimeout = 3 * time.Second })
	m := newMarket(t)

	// First pass creates the spine. Nothing is contended yet.
	if err := handle(t, w, delivery(t, m.marketID, m.payload(), 1)); err != nil {
		t.Fatalf("first write: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Exactly the row lock a referential-integrity trigger takes when another
	// table's insert references these rows. Every book and the league, because a
	// single row cannot close a cycle on its own.
	var one int
	err = tx.QueryRow(ctx,
		`SELECT 1 FROM books WHERE id = ANY($1::text[]) FOR KEY SHARE`,
		[]string{string(m.bookA), string(m.bookB)}).Scan(&one)
	if err != nil {
		t.Fatalf("lock books: %v", err)
	}
	err = tx.QueryRow(ctx,
		`SELECT 1 FROM leagues WHERE id = $1 FOR KEY SHARE`, string(m.leagueID)).Scan(&one)
	if err != nil {
		t.Fatalf("lock league: %v", err)
	}

	// The same unchanged catalogue, re-asserted while those locks are held.
	if err := handle(t, w, delivery(t, m.marketID, m.payload(), 2)); err != nil {
		t.Fatalf("re-asserting an UNCHANGED catalogue blocked on a FOR KEY SHARE lock "+
			"that a foreign-key check holds: %v", err)
	}
}

// deadlockingDB is the real pool with a deadlock injected in front of it.
//
// It is NOT a mocked database: every statement still runs against TimescaleDB.
// What it fakes is only the ONE condition an integration test cannot arrange
// deterministically — Postgres choosing this transaction as a deadlock victim —
// and it fakes it with the SQLSTATE the server actually sends, so the classifier
// under test (postgres.IsSerializationFailure) is the shipped one.
type deadlockingDB struct {
	db       writer.DB
	deadlock int // how many leading attempts to fail
	attempts int
}

func (d *deadlockingDB) InTx(ctx context.Context, fn postgres.TxFunc) error {
	d.attempts++
	if d.attempts <= d.deadlock {
		return &pgconn.PgError{Code: "40P01", Message: "deadlock detected", Severity: "ERROR"}
	}
	return d.db.InTx(ctx, fn)
}

// TestADeadlockedWriteIsRetriedAndLands is the phase-9 regression guard.
//
// Phase 9 put a second writer on this transaction's foreign-key parents: the
// signals stage inserts ev_signals, whose FKs make Postgres take FOR KEY SHARE on
// the same leagues, books, markets and selections rows that upsertCatalogue takes
// an exclusive lock on — in a different order. The observable symptom, before the
// retry existed, was a line-history writer silently dropping a few percent of its
// price batches on a running stack while every container reported healthy.
//
// The assertion that matters is the ROW COUNT: a deadlock guarantees a full
// rollback, so a retry must produce exactly the rows one clean attempt would,
// never a partial or doubled set.
func TestADeadlockedWriteIsRetriedAndLands(t *testing.T) {
	db := openPool(t)
	gate := &deadlockingDB{db: db, deadlock: 1}
	w, _ := newWriter(t, gate)
	m := newMarket(t)

	if err := handle(t, w, delivery(t, m.marketID, m.payload(), 1)); err != nil {
		t.Fatalf("HandleMessage: %v; a deadlock victim must be re-run, not dropped", err)
	}
	if gate.attempts != 2 {
		t.Fatalf("the pool saw %d transactions, want 2 (one victim, one that landed)", gate.attempts)
	}
	if rows := pricesFor(t, db, m); len(rows) != 4 {
		t.Fatalf("prices has %d rows, want 4 (2 selections × 2 books)", len(rows))
	}
}

// TestSustainedDeadlockingIsReportedRatherThanAbsorbed asserts the loop is
// bounded and that the SQLSTATE survives it.
//
// An unbounded loop would convert sustained contention into unbounded handler
// latency, which internal/platform/kafka's Consumer answers by fencing the group
// member — a worse failure than the one being avoided, and one no metric names.
// The record must fail so its offset stays uncommitted and the operator sees a
// 40P01 rather than a timeout.
func TestSustainedDeadlockingIsReportedRatherThanAbsorbed(t *testing.T) {
	db := openPool(t)
	gate := &deadlockingDB{db: db, deadlock: 99}
	w, _ := newWriter(t, gate)
	m := newMarket(t)

	err := handle(t, w, delivery(t, m.marketID, m.payload(), 1))
	if err == nil {
		t.Fatal("sustained deadlocking was absorbed; the record must fail")
	}
	if got := postgres.SQLState(err); got != "40P01" {
		t.Errorf("SQLSTATE = %q, want 40P01 carried through the retry loop: %v", got, err)
	}
	if gate.attempts < 2 {
		t.Fatalf("the pool saw %d transactions; the write was not retried at all", gate.attempts)
	}
	if rows := pricesFor(t, db, m); len(rows) != 0 {
		t.Fatalf("prices has %d rows after a transaction that never committed", len(rows))
	}
}

func TestATransactionThatFailsMidwayLeavesNothing(t *testing.T) {
	db := openPool(t)
	w, reg := newWriter(t, db)
	m := newMarket(t)

	p := m.payload()
	p.Books[1].Slug = p.Books[0].Slug

	err := handle(t, w, delivery(t, m.marketID, p, 1))
	if err == nil {
		t.Fatal("a payload declaring two book ids under one slug was accepted; books.slug is UNIQUE, " +
			"which is what makes the synthetic book's slug a stable handle")
	}
	if got := postgres.SQLState(err); got != "23505" {
		t.Errorf("SQLSTATE = %q, want 23505 (unique_violation): %v", got, err)
	}

	// Everything the transaction had already written before the failing
	// statement must be gone: the sport is the FIRST upsert in the function, so
	// it is the strongest witness that the rollback reached the beginning.
	for _, q := range []struct {
		table string
		sql   string
		arg   string
	}{
		{"sports", `SELECT count(*) FROM sports WHERE id = $1`, string(m.sportID)},
		{"leagues", `SELECT count(*) FROM leagues WHERE id = $1`, string(m.leagueID)},
		{"events", `SELECT count(*) FROM events WHERE id = $1`, string(m.eventID)},
		{"markets", `SELECT count(*) FROM markets WHERE id = $1`, string(m.marketID)},
	} {
		if got := scalar[int64](t, db, q.sql, q.arg); got != 0 {
			t.Errorf("%s has %d rows after a rolled-back transaction, want 0", q.table, got)
		}
	}
	if got := len(pricesFor(t, db, m)); got != 0 {
		t.Errorf("prices has %d rows after a rolled-back transaction, want 0", got)
	}

	// A database failure is transient-shaped, so it is counted as `failed` and
	// not as `invalid`: the offset stays uncommitted and the record is
	// redelivered, which for an outage is the right outcome.
	if got := counterValue(t, reg, "sharpline_writer_messages_total",
		map[string]string{"outcome": "failed"}); got != 1 {
		t.Errorf("messages_total{failed} = %v, want 1", got)
	}
	if got := histogramCount(t, reg, "sharpline_writer_flush_duration_seconds",
		map[string]string{"outcome": "error"}); got != 1 {
		t.Errorf("flush_duration_seconds{error} count = %d, want 1", got)
	}
}

// -----------------------------------------------------------------------------
// Batching
// -----------------------------------------------------------------------------

// TestChunkingKeepsOneTransaction.
//
// MaxRowsPerStatement is a STATEMENT-size bound, not a batch bound: every chunk
// of one record is written inside the same transaction, so it changes how the
// rows travel and never how many of them commit together. At one row per
// statement the four quotes take four statements and still land or fail as one.
func TestChunkingKeepsOneTransaction(t *testing.T) {
	db := openPool(t)
	m := newMarket(t)
	w, reg := newWriter(t, db, func(o *writer.Options) { o.MaxRowsPerStatement = 1 })

	if err := handle(t, w, delivery(t, m.marketID, m.payload(), 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if got := len(pricesFor(t, db, m)); got != 4 {
		t.Fatalf("prices has %d rows at one row per statement, want 4", got)
	}
	// Still ONE flush, because the transaction boundary is the record.
	if got := histogramCount(t, reg, "sharpline_writer_flush_duration_seconds",
		map[string]string{"outcome": "ok"}); got != 1 {
		t.Errorf("flush_duration_seconds{ok} count = %d, want 1 — chunking must not split the "+
			"transaction", got)
	}
}

// -----------------------------------------------------------------------------
// Lag measurement
// -----------------------------------------------------------------------------

// TestLagsAreMeasuredFromTheCommitInstant.
//
// Both histograms are (commit instant − some earlier instant), and the earlier
// instant is the OLDEST quote in the batch for observation lag: a staleness
// number is a claim about the worst thing in the batch, and reporting the fresh
// quote would flatter the pipeline exactly when it should not.
func TestLagsAreMeasuredFromTheCommitInstant(t *testing.T) {
	db := openPool(t)
	m := newMarket(t)

	// A frozen clock, so the assertion is an exact number rather than a bound.
	// It is never used to stamp a stored value — observed_at is the provider's,
	// ingested_at is ingest's, created_at is the database's.
	commit := m.quoteObsAt.Add(90 * time.Second)
	w, reg := newWriter(t, db, func(o *writer.Options) {
		o.Now = func() time.Time { return commit }
	})

	// The longshot has not moved in five minutes while the favourite re-priced a
	// moment ago. The staleness claim must be about the longshot.
	p := m.payload()
	p.Prices[1].ObservedAt = m.quoteObsAt.Add(-5 * time.Minute)

	if err := handle(t, w, delivery(t, m.marketID, p, 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	wantObservationLag := commit.Sub(p.Prices[1].ObservedAt).Seconds()
	got, ok := histogramSum(t, reg, "sharpline_writer_observation_lag_seconds", map[string]string{})
	if !ok {
		t.Fatal("sharpline_writer_observation_lag_seconds was not observed")
	}
	if got != wantObservationLag {
		t.Errorf("observation_lag_seconds = %v, want %v (the OLDEST quote in the batch)",
			got, wantObservationLag)
	}

	wantBusLag := commit.Sub(m.ingestedAt).Seconds()
	got, ok = histogramSum(t, reg, "sharpline_writer_bus_lag_seconds", map[string]string{})
	if !ok {
		t.Fatal("sharpline_writer_bus_lag_seconds was not observed")
	}
	if got != wantBusLag {
		t.Errorf("bus_lag_seconds = %v, want %v (commit − ingested_at)", got, wantBusLag)
	}
}

// TestClockSkewIsClampedRatherThanNegative. observed_at comes from the PROVIDER's
// clock and ingested_at from ours, so skew legitimately produces a negative
// difference. A negative histogram sample lands in the lowest bucket and is
// indistinguishable from a fast one; the skew itself is detected by
// sharpline_odds_clock_skew_total, which ingest owns.
func TestClockSkewIsClampedRatherThanNegative(t *testing.T) {
	db := openPool(t)
	m := newMarket(t)

	// The provider's clock is an hour ahead of ours, so every quote is "from the
	// future" at the moment it commits.
	commit := m.quoteObsAt.Add(-time.Hour)
	w, reg := newWriter(t, db, func(o *writer.Options) {
		o.Now = func() time.Time { return commit }
	})

	if err := handle(t, w, delivery(t, m.marketID, m.payload(), 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	got, ok := histogramSum(t, reg, "sharpline_writer_observation_lag_seconds", map[string]string{})
	if !ok {
		t.Fatal("sharpline_writer_observation_lag_seconds was not observed")
	}
	if got != 0 {
		t.Errorf("observation_lag_seconds = %v under provider clock skew, want 0 (clamped)", got)
	}
}

// -----------------------------------------------------------------------------
// The consumer loop
// -----------------------------------------------------------------------------

// feed is a writer.Consumer that hands the writer a fixed set of records and
// then blocks until the context is cancelled, exactly as *kafka.Consumer's Run
// does.
//
// What this proves and what it does not: it proves Run reaches the handler with
// every record and returns cleanly on cancellation, against a REAL database. It
// does NOT prove anything about consumer-group rebalancing or offset commits —
// those are properties of the broker and belong in test/integration, where the
// KRaft fixture lives. See harness_test.go.
type feed struct {
	records []*kafka.Delivery
	errs    []error
}

func (f *feed) Run(ctx context.Context, h kafka.Handler) error {
	for _, d := range f.records {
		f.errs = append(f.errs, h.HandleMessage(ctx, d))
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestRunDeliversEveryRecordAndStopsCleanly. Shutdown here is uneventful by
// design: HandleMessage commits before it returns, so a Writer never holds
// unwritten rows and there is nothing to flush on the way out.
func TestRunDeliversEveryRecordAndStopsCleanly(t *testing.T) {
	db := openPool(t)
	w, _ := newWriter(t, db)

	first := newMarket(t)
	second := newMarket(t)

	f := &feed{records: []*kafka.Delivery{
		delivery(t, first.marketID, first.payload(), 1),
		delivery(t, second.marketID, second.payload(), 2),
		tombstone(first.marketID, 3),
	}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, f) }()

	// Every record is handled before Run parks on the context, so once the rows
	// are visible the loop has drained.
	deadline := time.Now().Add(30 * time.Second)
	for len(pricesFor(t, db, second)) < 4 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not deliver both records within 30s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v on cancellation, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s of cancellation")
	}

	for i, err := range f.errs {
		if err != nil {
			t.Errorf("record %d was rejected by the handler: %v", i+1, err)
		}
	}
	if got := len(pricesFor(t, db, first)); got != 4 {
		t.Errorf("the first market has %d price rows, want 4", got)
	}
	if got := len(pricesFor(t, db, second)); got != 4 {
		t.Errorf("the second market has %d price rows, want 4", got)
	}
}
