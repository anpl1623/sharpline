package pgstore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi"
	"github.com/anpl1623/sharpline/internal/httpapi/pgstore"
	"github.com/anpl1623/sharpline/internal/platform/migrate"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// The API's data plane, against a REAL TimescaleDB.
//
// # Why this tier exists even though sqlc already type-checks the queries
//
// sqlc parses SQL with its own embedded PostgreSQL parser and NEVER CONTACTS A
// SERVER. It happily generates code for a statement the real engine rejects, and
// it has no opinion whatsoever about whether a query returns the right rows.
// Everything in internal/platform/postgres/queries/api.sql that a type checker
// cannot see is proved here:
//
//   - the keyset predicate `(scheduled_start, id) > ($1, $2::TEXT)` actually
//     paginates: no row served twice, no row skipped;
//   - the epoch-floor bucketing produces the OHLC a chart needs, and `has_line`
//     really does distinguish a null line from a zero one;
//   - `account_balances` returns no row for an untouched account and the store
//     materialises the zero, so a new user reads as 0 rather than as an error;
//   - the append-only user_limits triggers accept the supersede-then-insert
//     sequence [pgstore.Store.Set] performs, in one transaction;
//   - the audit insert satisfies migration 00007's CHECK constraints on
//     trace_id/span_id shape.
//
// Each of those is a statement about the DATABASE, and only a database can
// answer it.
//
// # No mock data
//
// Every row here is written BY this test FOR this test and asserted on by the
// test that wrote it. Nothing is seeded, nothing stands in for ingested data,
// and no canned result appears anywhere.
//
// # It fails rather than skips
//
// A silently skipped integration test reports green while proving nothing, and
// the CI job meant to enforce the prime directive becomes decorative.

// postgresImage is the compose stack's `postgres` image, pinned by digest —
// the same image at the same digest test/integration uses. A stock postgres has
// no TimescaleDB, and the `prices` hypertable does not exist without it.
const postgresImage = "timescale/timescaledb:latest-pg17@sha256:981e3016a2810fec47515e3828ad70ae97b84f4c9ef63d032180b54f61566fd6"

const (
	pgUser     = "sharpline"
	pgPassword = "test-only-throwaway"
	pgDatabase = "sharpline_apistore"

	startDeadline = 4 * time.Minute
)

var testDSN string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), startDeadline)
	defer cancel()

	container, dsn, err := startPostgres(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres: %v\n", err)
		os.Exit(1)
	}
	testDSN = dsn

	if err := applyMigrations(ctx, dsn); err != nil {
		_ = container.Terminate(context.Background())
		fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	_ = container.Terminate(context.Background())
	os.Exit(code)
}

func startPostgres(ctx context.Context) (testcontainers.Container, string, error) {
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":             pgUser,
				"POSTGRES_PASSWORD":         pgPassword,
				"POSTGRES_DB":               pgDatabase,
				"TIMESCALEDB_TELEMETRY":     "off",
				"NO_TS_TUNE":                "true",
				"POSTGRES_HOST_AUTH_METHOD": "scram-sha-256",
			},
			WaitingFor: wait.ForAll(
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithDeadline(startDeadline),
		},
		Started: true,
	}
	c, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		return nil, "", err
	}
	host, err := c.Host(ctx)
	if err != nil {
		return nil, "", err
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, "", err
	}
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, url.QueryEscape(pgPassword), host, port.Port(), pgDatabase)
	return c, dsn, nil
}

func applyMigrations(ctx context.Context, dsn string) error {
	runner, err := migrate.New(migrate.Options{DSN: dsn, Logger: discard()})
	if err != nil {
		return err
	}
	defer func() { _ = runner.Close() }()
	_, err = runner.Run(ctx, migrate.Invocation{Command: migrate.CommandUp})
	return err
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newStore opens a pool and builds the adapter. Every test gets its own pool so
// a test that exhausts one cannot affect another.
func newStore(t *testing.T) (*pgstore.Store, *postgres.DB) {
	t.Helper()

	db, err := postgres.Connect(t.Context(), postgres.Options{
		DSN:     testDSN,
		Service: "api-store-test",
		Logger:  discard(),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	store, err := pgstore.New(pgstore.Options{
		DB:         db,
		CoolingOff: time.Hour,
		NewID:      idGen(t),
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store, db
}

// idCounter is shared by every generator in the package and read and written
// from tests that call t.Parallel(), so the increment has to be atomic. A bare
// idCounter++ is a read-modify-write that the race detector flags -- and it did,
// on CI rather than locally, because GO_TEST_P serialises package BINARIES while
// t.Parallel() still runs these tests concurrently inside one binary.
var idCounter atomic.Int64

func idGen(t *testing.T) func() (string, error) {
	prefix := sanitize(t.Name())
	return func() (string, error) {
		return fmt.Sprintf("lim_%s_%d", prefix, idCounter.Add(1)), nil
	}
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// -----------------------------------------------------------------------------
// Catalogue and keyset pagination
// -----------------------------------------------------------------------------

// TestKeysetPaginationWalksTheWholeSetExactlyOnce is the property the whole
// cursor design exists for, asserted against the real planner and the real
// row-value comparison.
//
// The fixture deliberately gives FOUR events the SAME scheduled_start, so the
// `id` tie-break in `(scheduled_start, id) > (...)` is what carries the cursor.
// Ordering by scheduled_start alone would be non-deterministic across those four
// and the walk would drop or repeat one.
func TestKeysetPaginationWalksTheWholeSetExactlyOnce(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	f := seedCatalogue(t, ctx, db)

	const total = 9
	start := f.window
	for i := range total {
		// Three groups of three sharing an instant.
		insertEvent(t, ctx, db, f, fmt.Sprintf("%s_e%02d", f.prefix, i),
			start.Add(time.Duration(i/3)*time.Hour), "scheduled")
	}

	seen := map[domain.EventID]int{}
	var after *httpapi.EventKey
	for page := 0; page < total+2; page++ {
		got, err := store.EventPage(ctx, httpapi.EventPageQuery{
			LeagueID:       f.leagueID,
			StartingBefore: start.Add(24 * time.Hour),
			After:          after,
			Limit:          2,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, e := range got.Events {
			seen[e.ID]++
		}
		if !got.HasMore {
			break
		}
		last := got.Events[len(got.Events)-1]
		after = &httpapi.EventKey{ScheduledStart: last.ScheduledStart, ID: last.ID}
	}

	if len(seen) != total {
		t.Errorf("walked %d distinct events, want %d: the keyset predicate skipped rows", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("event %s returned %d times: the keyset predicate duplicated a row", id, n)
		}
	}
}

// TestBoardExcludesSettledEvents: the status literals in the query must match
// the partial index's predicate, and a settled event must not reach the board.
func TestBoardExcludesSettledEvents(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	f := seedCatalogue(t, ctx, db)

	live := insertEvent(t, ctx, db, f, f.prefix+"_live", f.window, "live")
	insertEvent(t, ctx, db, f, f.prefix+"_settled", f.window.Add(time.Minute), "settled")

	got, err := store.EventPage(ctx, httpapi.EventPageQuery{
		LeagueID:       f.leagueID,
		StartingBefore: f.window.Add(24 * time.Hour),
		Limit:          50,
	})
	if err != nil {
		t.Fatalf("event page: %v", err)
	}
	if len(got.Events) != 1 || got.Events[0].ID != live {
		t.Fatalf("board returned %d events, want only the live one", len(got.Events))
	}
}

// TestSearchMatchesEitherCompetitorAndEscapesWildcards.
//
// The OR-of-two-partial-indexes form must find a competitor on either side and
// return an event whose two competitors both match exactly ONCE. And a query of
// `%` must match nothing rather than becoming a leading-wildcard scan.
func TestSearchMatchesEitherCompetitorAndEscapesWildcards(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	f := seedCatalogue(t, ctx, db)

	home := insertNamedEvent(t, ctx, db, f, f.prefix+"_h", f.window, "scheduled",
		f.prefix+"Celtics", f.prefix+"Lakers")
	away := insertNamedEvent(t, ctx, db, f, f.prefix+"_a", f.window.Add(time.Minute), "scheduled",
		f.prefix+"Heat", f.prefix+"Celtics")
	// Both sides match the same prefix: UNION-style de-duplication must give one
	// row, and the OR form gets that for free.
	both := insertNamedEvent(t, ctx, db, f, f.prefix+"_b", f.window.Add(2*time.Minute), "scheduled",
		f.prefix+"Celtics A", f.prefix+"Celtics B")

	got, err := store.SearchEvents(ctx, httpapi.SearchQuery{Prefix: f.prefix + "Celtics", Limit: 50})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	found := map[domain.EventID]int{}
	for _, e := range got.Events {
		found[e.ID]++
	}
	for _, want := range []domain.EventID{home, away, both} {
		if found[want] != 1 {
			t.Errorf("event %s appeared %d times, want exactly 1", want, found[want])
		}
	}

	// A bare wildcard must be a literal. If escapeLike were missing, this would
	// return every event in the table.
	wild, err := store.SearchEvents(ctx, httpapi.SearchQuery{Prefix: "%", Limit: 50})
	if err != nil {
		t.Fatalf("wildcard search: %v", err)
	}
	if len(wild.Events) != 0 {
		t.Errorf("a search for %%%% returned %d events: LIKE metacharacters are not escaped", len(wild.Events))
	}
}

// -----------------------------------------------------------------------------
// Prices
// -----------------------------------------------------------------------------

// TestBucketedHistoryProducesOHLCAndDistinguishesANullLine.
//
// The epoch-floor bucketing is arithmetic sqlc cannot check and the planner will
// happily run wrongly. This asserts the numbers.
func TestBucketedHistoryProducesOHLCAndDistinguishesANullLine(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	f := seedCatalogue(t, ctx, db)

	ev := insertEvent(t, ctx, db, f, f.prefix+"_hist", f.window, "live")
	mk := insertMarket(t, ctx, db, f, ev, "moneyline", nil)
	sel := insertSelection(t, ctx, db, mk, f.prefix+"_sel", "home")

	// Four quotes inside one 1-minute bucket, in a deliberately jumbled order so
	// "open" and "close" are decided by observed_at and not by insertion order.
	base := f.window.Truncate(time.Minute)
	for _, q := range []struct {
		offset time.Duration
		odds   float64
	}{
		{30 * time.Second, 1.95},
		{10 * time.Second, 1.90}, // open
		{50 * time.Second, 1.85}, // close, and the low
		{20 * time.Second, 1.98}, // high
	} {
		insertPrice(t, ctx, db, sel, f.bookID, q.odds, nil, base.Add(q.offset))
	}

	points, err := store.History(ctx, httpapi.HistoryQuery{
		SelectionID: sel,
		BookID:      f.bookID,
		From:        base.Add(-time.Minute),
		To:          base.Add(2 * time.Minute),
		Bucket:      time.Minute,
		MaxPoints:   100,
	})
	if err != nil {
		t.Fatalf("bucketed history: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("got %d buckets, want 1; the epoch-floor arithmetic is wrong", len(points))
	}

	p := points[0]
	if float64(p.Open) != 1.90 {
		t.Errorf("open = %v, want 1.90 (the earliest quote in the bucket)", p.Open)
	}
	if float64(p.Close) != 1.85 {
		t.Errorf("close = %v, want 1.85 (the latest quote in the bucket)", p.Close)
	}
	if float64(p.High) != 1.98 {
		t.Errorf("high = %v, want 1.98", p.High)
	}
	if float64(p.Low) != 1.85 {
		t.Errorf("low = %v, want 1.85", p.Low)
	}
	if p.Samples != 4 {
		t.Errorf("samples = %d, want 4", p.Samples)
	}
	// A moneyline carries no line. `has_line` is what stops the coalesced zero
	// being reported as a line of 0.0 — which on a spread market would be a
	// pick'em, a real and different price.
	if p.Line != nil {
		t.Errorf("line = %v on a market with no line; the has_line flag is not being honoured", *p.Line)
	}

	// Now a market that DOES carry a line, to prove the flag distinguishes them.
	line := -3.5
	mk2 := insertMarket(t, ctx, db, f, ev, "spread", &line)
	sel2 := insertSelection(t, ctx, db, mk2, f.prefix+"_sel2", "home")
	insertPrice(t, ctx, db, sel2, f.bookID, 1.91, &line, base.Add(15*time.Second))

	points2, err := store.History(ctx, httpapi.HistoryQuery{
		SelectionID: sel2, BookID: f.bookID,
		From: base.Add(-time.Minute), To: base.Add(2 * time.Minute),
		Bucket: time.Minute, MaxPoints: 100,
	})
	if err != nil {
		t.Fatalf("bucketed history (spread): %v", err)
	}
	if len(points2) != 1 || points2[0].Line == nil || *points2[0].Line != line {
		t.Fatalf("spread bucket did not carry its line: %+v", points2)
	}
}

// TestCurrentQuotesReturnsTheNewestPerBookAndHonoursTheHorizon.
func TestCurrentQuotesReturnsTheNewestPerBookAndHonoursTheHorizon(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	f := seedCatalogue(t, ctx, db)

	ev := insertEvent(t, ctx, db, f, f.prefix+"_cur", f.window, "live")
	mk := insertMarket(t, ctx, db, f, ev, "moneyline", nil)
	sel := insertSelection(t, ctx, db, mk, f.prefix+"_cursel", "home")

	insertPrice(t, ctx, db, sel, f.bookID, 1.80, nil, f.window.Add(-2*time.Hour))
	insertPrice(t, ctx, db, sel, f.bookID, 1.90, nil, f.window.Add(-time.Minute))
	insertPrice(t, ctx, db, sel, f.bookID, 1.95, nil, f.window)

	got, err := store.CurrentQuotes(ctx, []domain.SelectionID{sel}, f.window.Add(-time.Hour))
	if err != nil {
		t.Fatalf("current quotes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d quotes, want 1 per (selection, book)", len(got))
	}
	if float64(got[0].Odds) != 1.95 {
		t.Errorf("odds = %v, want the newest quote 1.95", got[0].Odds)
	}

	// The stale quote is outside the horizon and must not come back as a
	// "current" line — a quote older than the horizon is history, not a price.
	stale, err := store.CurrentQuotes(ctx, []domain.SelectionID{sel}, f.window.Add(time.Minute))
	if err != nil {
		t.Fatalf("current quotes (future horizon): %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("got %d quotes past the freshness horizon, want 0", len(stale))
	}
}

// -----------------------------------------------------------------------------
// Balances
// -----------------------------------------------------------------------------

// TestUntouchedAccountReadsAsZeroRatherThanMissing.
//
// account_balances GROUPs BY account, so a user with no ledger entries has NO
// ROW — correctly. The store materialises the zero so a handler never has to
// decide what an absent account means, and "never funded" stays distinguishable
// from "funded and spent to zero" through entry_count.
func TestUntouchedAccountReadsAsZeroRatherThanMissing(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	user := insertUser(t, ctx, db)

	balances, err := store.Balances(ctx, user)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	if len(balances) != 2 {
		t.Fatalf("got %d balances, want cash and escrow", len(balances))
	}
	for _, b := range balances {
		if !b.Amount.IsZero() || b.Entries != 0 {
			t.Errorf("%v = %d minor over %d entries, want a zero balance over zero entries",
				b.Kind, b.Amount.MinorUnits(), b.Entries)
		}
	}
}

// -----------------------------------------------------------------------------
// Self-imposed limits
// -----------------------------------------------------------------------------

// TestTighteningBindsImmediatelyAndLooseningServesTheCoolingOff is the control
// itself. A limit a user can lift the instant they want to is not a limit.
func TestTighteningBindsImmediatelyAndLooseningServesTheCoolingOff(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	user := insertUser(t, ctx, db)

	now := time.Now().UTC().Truncate(time.Microsecond)
	ac := httpapi.AuditContext{RequestID: "req-limits", At: now}

	// Introducing a limit where there was none is a tightening.
	first := money(t, 100_000)
	got, err := store.Set(ctx, httpapi.SetLimit{
		UserID: user, Kind: auth.LimitKindLoss, Period: auth.LimitPeriodDay,
		Amount: &first, Audit: ac,
	})
	if err != nil {
		t.Fatalf("set first limit: %v", err)
	}
	if !got.EffectiveFrom.Equal(got.RequestedAt) {
		t.Errorf("introducing a limit was delayed: effective_from %v != requested_at %v",
			got.EffectiveFrom, got.RequestedAt)
	}

	// Lowering it is a tightening: immediate.
	lower := money(t, 50_000)
	ac2 := httpapi.AuditContext{RequestID: "req-limits-2", At: now.Add(time.Second)}
	got, err = store.Set(ctx, httpapi.SetLimit{
		UserID: user, Kind: auth.LimitKindLoss, Period: auth.LimitPeriodDay,
		Amount: &lower, Audit: ac2,
	})
	if err != nil {
		t.Fatalf("tighten: %v", err)
	}
	if !got.EffectiveFrom.Equal(got.RequestedAt) {
		t.Errorf("a tightening was delayed to %v", got.EffectiveFrom)
	}

	// Raising it is a LOOSENING: it must wait.
	higher := money(t, 500_000)
	ac3 := httpapi.AuditContext{RequestID: "req-limits-3", At: now.Add(2 * time.Second)}
	got, err = store.Set(ctx, httpapi.SetLimit{
		UserID: user, Kind: auth.LimitKindLoss, Period: auth.LimitPeriodDay,
		Amount: &higher, Audit: ac3,
	})
	if err != nil {
		t.Fatalf("loosen: %v", err)
	}
	if !got.EffectiveFrom.After(got.RequestedAt) {
		t.Errorf("a loosening took effect immediately (requested %v, effective %v): "+
			"a limit a user can lift at will is not a limit", got.RequestedAt, got.EffectiveFrom)
	}

	// Exactly one current row per (user, kind, period) — the partial unique
	// index guarantees it, and Current must reflect that.
	current, err := store.Current(ctx, user)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	n := 0
	for _, l := range current {
		if l.Kind == auth.LimitKindLoss && l.Period == auth.LimitPeriodDay {
			n++
			if l.Amount == nil || l.Amount.MinorUnits() != 500_000 {
				t.Errorf("current amount = %v, want 500000", l.Amount)
			}
		}
	}
	if n != 1 {
		t.Errorf("found %d current loss/day limits, want exactly 1", n)
	}
}

// TestSessionLimitStoresADurationAndNoAmount exercises the three biconditionals
// in migration 00005 and the value-plus-flag encoding that reads them back.
func TestSessionLimitStoresADurationAndNoAmount(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	user := insertUser(t, ctx, db)

	d := 90 * time.Minute
	got, err := store.Set(ctx, httpapi.SetLimit{
		UserID: user, Kind: auth.LimitKindSession, Period: auth.LimitPeriodSession,
		Duration: &d,
		Audit:    httpapi.AuditContext{RequestID: "req-session", At: time.Now().UTC()},
	})
	if err != nil {
		t.Fatalf("set session limit: %v", err)
	}
	if got.Amount != nil {
		t.Errorf("a session limit carries an amount: %v", got.Amount)
	}
	if got.Duration == nil || *got.Duration != d {
		t.Errorf("duration = %v, want %v", got.Duration, d)
	}

	// And read back: this is the path where a NULL amount_minor would otherwise
	// be scanned into a non-pointer integer and fail.
	current, err := store.Current(ctx, user)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	for _, l := range current {
		if l.Kind != auth.LimitKindSession {
			continue
		}
		if l.Amount != nil {
			t.Errorf("session limit read back with an amount: %v", l.Amount)
		}
		if l.Duration == nil || *l.Duration != d {
			t.Errorf("session limit read back with duration %v, want %v", l.Duration, d)
		}
	}
}

// TestLimitHistoryIsAppendOnly: the superseded row survives and is not edited.
func TestLimitHistoryIsAppendOnly(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	user := insertUser(t, ctx, db)

	now := time.Now().UTC()
	for i, minor := range []int64{100_000, 50_000, 20_000} {
		amount := money(t, minor)
		if _, err := store.Set(ctx, httpapi.SetLimit{
			UserID: user, Kind: auth.LimitKindStake, Period: auth.LimitPeriodWeek,
			Amount: &amount,
			Audit:  httpapi.AuditContext{RequestID: "req-hist", At: now.Add(time.Duration(i) * time.Second)},
		}); err != nil {
			t.Fatalf("set %d: %v", minor, err)
		}
	}

	var rows int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM user_limits WHERE user_id = $1 AND kind = 'stake' AND period = 'week'`,
		user).Scan(&rows); err != nil {
		t.Fatalf("count history: %v", err)
	}
	// Three requests, three rows: two superseded and one current. A settings row
	// that was edited in place would show one.
	if rows != 3 {
		t.Errorf("user_limits holds %d rows for three changes, want 3: the history is not append-only", rows)
	}
}

// -----------------------------------------------------------------------------
// Audit
// -----------------------------------------------------------------------------

// TestAuditEntrySatisfiesTheTraceIDCheck.
//
// Migration 00007 constrains trace_id and span_id to lowercase hex of the right
// width, so a malformed id is refused rather than stored as an unjoinable
// string. This proves the store writes a shape the database accepts, and that
// an absent trace is written as NULL rather than as an all-zero id.
func TestAuditEntrySatisfiesTheTraceIDCheck(t *testing.T) {
	t.Parallel()

	store, db := newStore(t)
	ctx := t.Context()
	user := insertUser(t, ctx, db)

	entry := httpapi.AuditEntry{
		Context: httpapi.AuditContext{
			RequestID: "req-audit-1",
			TraceID:   "4bf92f3577b34da6a3ce929d0e0e4736",
			SpanID:    "00f067aa0ba902b7",
			At:        time.Now().UTC(),
		},
		ActorKind:  "user",
		ActorID:    user.String(),
		Action:     "totp.enrol_begin",
		EntityType: "user_totp",
		EntityID:   user.String(),
		Outcome:    "success",
		After:      map[string]any{"enrolment": "started"},
	}
	if err := store.Record(ctx, entry); err != nil {
		t.Fatalf("record audit entry: %v", err)
	}

	// And with no trace at all.
	entry.Context.TraceID = ""
	entry.Context.SpanID = ""
	entry.Context.RequestID = "req-audit-2"
	if err := store.Record(ctx, entry); err != nil {
		t.Fatalf("record audit entry without a trace: %v", err)
	}

	var n int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE actor_id = $1 AND action = 'totp.enrol_begin'`,
		user.String()).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 2 {
		t.Errorf("audit_log holds %d rows, want 2", n)
	}

	var traceNull bool
	if err := db.Pool().QueryRow(ctx,
		`SELECT trace_id IS NULL FROM audit_log WHERE request_id = 'req-audit-2'`).Scan(&traceNull); err != nil {
		t.Fatalf("read trace_id: %v", err)
	}
	if !traceNull {
		t.Error("an absent trace was stored as a value rather than NULL; an all-zero id looks like data and joins to nothing")
	}
}

// -----------------------------------------------------------------------------
// Absence
// -----------------------------------------------------------------------------

// TestMissingRowsReportErrNotFoundAndNotAZeroValue.
//
// Collapsing "no such event" into a zero value is how a 404 becomes a 200 with
// an empty object; collapsing a connection failure into "not found" is how a
// database outage takes an hour to diagnose.
func TestMissingRowsReportErrNotFoundAndNotAZeroValue(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := t.Context()

	if _, _, _, err := store.EventWithBreadcrumb(ctx, "evt_does_not_exist"); err == nil {
		t.Error("EventWithBreadcrumb returned no error for a missing event")
	} else if !isNotFound(err) {
		t.Errorf("EventWithBreadcrumb error = %v, want httpapi.ErrNotFound", err)
	}
	if _, err := store.Market(ctx, "mkt_does_not_exist"); !isNotFound(err) {
		t.Errorf("Market error = %v, want httpapi.ErrNotFound", err)
	}
	if _, err := store.Selection(ctx, "sel_does_not_exist"); !isNotFound(err) {
		t.Errorf("Selection error = %v, want httpapi.ErrNotFound", err)
	}
	if _, err := store.LeagueBySlug(ctx, "no-such-league"); !isNotFound(err) {
		t.Errorf("LeagueBySlug error = %v, want httpapi.ErrNotFound", err)
	}
	if _, err := store.Profile(ctx, "usr_does_not_exist"); !isNotFound(err) {
		t.Errorf("Profile error = %v, want httpapi.ErrNotFound", err)
	}
}

func isNotFound(err error) bool { return errors.Is(err, httpapi.ErrNotFound) }
