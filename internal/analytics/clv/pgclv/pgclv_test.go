package pgclv_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/anpl1623/sharpline/internal/analytics/clv"
	"github.com/anpl1623/sharpline/internal/analytics/clv/pgclv"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/platform/migrate"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// The CLOSING PRICE, against a REAL TimescaleDB.
//
// # Why this is the query that most needs a database
//
// Defining the close is the actual work of the CLV feature, and every part of the
// definition that could go wrong is a property of the STATEMENT rather than of the
// Go around it:
//
//   - `as_of` is INCLUSIVE and `not_before` is EXCLUSIVE, which is what makes a
//     leg's own quote eligible for its own taken snapshot and what keeps a stale
//     quote from six days out from being called a close;
//   - a suspension episode is half-open at both ends — a quote at the instant a
//     suspension LIFTS counts, one at the instant it BEGINS does not — so a market
//     suspended and reopened before the start needs no special case at all;
//   - the lateral is an INNER join, so an unpriced selection produces no row and
//     the caller's completeness check (`len(rows) < MarketSelections`) is the only
//     thing standing between a partial outcome set and a devig of a subset;
//   - `market_selections` is a correlated count over the same market, so the
//     completeness check has a denominator that cannot drift from the numerator.
//
// None of those is visible to sqlc, which parses SQL and never contacts a server.
//
// # No mock data
//
// Every row is written by this test for this test, in an id and time namespace it
// owns. Nothing is seeded.

// postgresImage is the compose stack's `postgres` image, pinned by digest — the
// same image at the same digest the other integration tiers use. A stock postgres
// has no TimescaleDB, and `prices` is a hypertable.
const postgresImage = "timescale/timescaledb:latest-pg17@sha256:981e3016a2810fec47515e3828ad70ae97b84f4c9ef63d032180b54f61566fd6"

const (
	pgUser     = "sharpline"
	pgPassword = "test-only-throwaway"
	pgDatabase = "sharpline_clvstore"

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
	return c, fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, url.QueryEscape(pgPassword), host, port.Port(), pgDatabase), nil
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

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

var seq = struct{ n int }{}

// market is one test's private market: two selections at one book, and its own
// hour of the clock so a parallel test's prices cannot land inside its window.
type market struct {
	db *postgres.DB

	leagueID domain.LeagueID
	bookID   domain.BookID
	eventID  domain.EventID
	marketID domain.MarketID
	home     domain.SelectionID
	away     domain.SelectionID

	// start is the event's scheduled start, which IS the closing instant.
	start time.Time
}

func newMarket(t *testing.T) market {
	t.Helper()
	ctx := t.Context()

	db, err := postgres.Connect(ctx, postgres.Options{
		DSN: testDSN, Service: "clv-store-test", Logger: discard(),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	seq.n++
	tok := fmt.Sprintf("%d", seq.n)
	m := market{
		db:       db,
		leagueID: domain.LeagueID("lg_" + tok),
		bookID:   domain.BookID("bk_" + tok),
		eventID:  domain.EventID("ev_" + tok),
		marketID: domain.MarketID("mk_" + tok),
		home:     domain.SelectionID("mk_" + tok + ".home"),
		away:     domain.SelectionID("mk_" + tok + ".away"),
		start:    time.Now().UTC().Add(time.Duration(seq.n) * 24 * time.Hour).Truncate(time.Second),
	}

	exec(t, db, `INSERT INTO sports (id, slug, name) VALUES ($1, $2, $3)`,
		"sp_"+tok, "sp-"+tok, "Sport "+tok)
	exec(t, db, `INSERT INTO leagues (id, sport_id, slug, name) VALUES ($1, $2, $3, $4)`,
		m.leagueID, "sp_"+tok, "lg-"+tok, "League "+tok)
	exec(t, db, `INSERT INTO books (id, slug, name, kind, is_reference)
	             VALUES ($1, $2, $3, 'external', FALSE)`,
		m.bookID, "bk-"+tok, "Book "+tok)
	exec(t, db, `
INSERT INTO events (id, league_id, kind, name, home_competitor_name, away_competitor_name,
                    scheduled_start, status, observed_at)
VALUES ($1, $2, 'match', 'Home at Away', 'Home', 'Away', $3, 'ended', $4)`,
		m.eventID, m.leagueID, m.start, time.Now().UTC())
	exec(t, db, `
INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
VALUES ($1, $2, 'moneyline', NULL, NULL, 'open', $3)`,
		m.marketID, m.eventID, time.Now().UTC())
	for _, s := range []struct {
		id   domain.SelectionID
		role string
	}{{m.home, "home"}, {m.away, "away"}} {
		exec(t, db, `
INSERT INTO selections (id, market_id, market_type, role, name)
VALUES ($1, $2, 'moneyline', $3, $4)`, s.id, m.marketID, s.role, "Selection "+s.role)
	}
	return m
}

func (m market) price(t *testing.T, sel domain.SelectionID, dec float64, at time.Time) {
	t.Helper()
	exec(t, m.db, `
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES ($1, $2, $3, NULL, $4, $5)`, sel, m.bookID, dec, at, at.Add(time.Second))
}

func (m market) suspend(t *testing.T, from time.Time, until *time.Time) {
	t.Helper()
	// suspended_by / lifted_by are NOT NULL denormalised actor identities: the
	// audit trail on a suspension is the point of the table, so it cannot hold an
	// episode nobody is attributed with.
	liftedBy := any(nil)
	if until != nil {
		liftedBy = "test-operator"
	}
	exec(t, m.db, `
INSERT INTO market_suspensions (market_id, suspended_at, lifted_at, reason, suspended_by, lifted_by)
VALUES ($1, $2, $3, 'trading', 'test-operator', $4)`, m.marketID, from, until, liftedBy)
}

func exec(t *testing.T, db *postgres.DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Pool().Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("exec %.60s...: %v", sql, err)
	}
}

func newStore(t *testing.T, db *postgres.DB) *pgclv.Store {
	t.Helper()
	s, err := pgclv.NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

// TestMarketCloseReportsTheScheduledStart.
//
// `scheduled_start` is the closing instant, and it is a stored column rather than
// a consequence of when the poller fired — which is the whole reason it was
// chosen over actual kickoff. This asserts the adapter reads it, along with the
// identity columns settle stamps onto the CLV row.
func TestMarketCloseReportsTheScheduledStart(t *testing.T) {
	m := newMarket(t)
	store := newStore(t, m.db)

	got, err := store.MarketClose(t.Context(), m.marketID)
	if err != nil {
		t.Fatalf("MarketClose: %v", err)
	}
	if !got.ScheduledStart.Equal(m.start) {
		t.Fatalf("closing instant is %s, want the scheduled start %s", got.ScheduledStart, m.start)
	}
	if got.LeagueID != m.leagueID {
		t.Fatalf("league is %s, want %s", got.LeagueID, m.leagueID)
	}
	if got.MarketType != domain.MarketTypeMoneyline {
		t.Fatalf("market type is %v, want moneyline", got.MarketType)
	}
}

// TestSnapshotTakesTheLastQuoteBeforeTheClose.
//
// The bounds are the assertion: `as_of` inclusive, `not_before` exclusive. The
// inclusive upper bound is what makes a leg's own quote eligible for its own taken
// snapshot, and getting it wrong would silently score every wager against the
// price BEFORE the one it was struck at.
func TestSnapshotTakesTheLastQuoteBeforeTheClose(t *testing.T) {
	m := newMarket(t)
	store := newStore(t, m.db)

	m.price(t, m.home, 3.00, m.start.Add(-2*time.Hour))
	m.price(t, m.home, 2.00, m.start) // exactly at the close: eligible
	m.price(t, m.home, 1.50, m.start.Add(time.Minute))
	m.price(t, m.away, 2.10, m.start.Add(-time.Hour))
	m.price(t, m.away, 9.00, m.start.Add(time.Minute)) // in play: never a close

	snap, err := store.Snapshot(t.Context(), clv.SnapshotRequest{
		Market:    m.marketID,
		Book:      m.bookID,
		AsOf:      m.start,
		NotBefore: m.start.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Quotes) != 2 {
		t.Fatalf("snapshot holds %d quotes, want 2", len(snap.Quotes))
	}
	if snap.MarketSelections != 2 {
		t.Fatalf("market_selections is %d, want 2: it is the completeness denominator",
			snap.MarketSelections)
	}
	got := map[domain.SelectionID]float64{}
	for _, q := range snap.Quotes {
		got[q.Selection] = float64(q.Decimal)
	}
	if got[m.home] != 2.00 {
		t.Fatalf("home closed at %v, want 2.00 (the quote exactly AT the close; as_of is "+
			"inclusive, and an exclusive bound would score every wager against the price "+
			"before the one it was struck at)", got[m.home])
	}
	if got[m.away] != 2.10 {
		t.Fatalf("away closed at %v, want 2.10; a quote observed after the scheduled start is "+
			"an in-play price and answers a different question", got[m.away])
	}
}

// TestSnapshotIsIncompleteWhenASelectionHasNoEligibleQuote.
//
// The lateral is an INNER join, so a selection with nothing eligible simply
// produces no row — and the caller's `len(rows) < MarketSelections` check is the
// only thing between that and devigging a partial outcome set, which
// `NewFairMarketSnapshot` would then refuse for a reason that names probabilities
// rather than naming the missing side.
func TestSnapshotIsIncompleteWhenASelectionHasNoEligibleQuote(t *testing.T) {
	m := newMarket(t)
	store := newStore(t, m.db)

	m.price(t, m.home, 2.00, m.start.Add(-time.Hour))
	// The away side's only quote is older than the lookback allows.
	m.price(t, m.away, 2.10, m.start.Add(-48*time.Hour))

	snap, err := store.Snapshot(t.Context(), clv.SnapshotRequest{
		Market:    m.marketID,
		Book:      m.bookID,
		AsOf:      m.start,
		NotBefore: m.start.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Quotes) != 1 {
		t.Fatalf("snapshot holds %d quotes, want 1", len(snap.Quotes))
	}
	if snap.Complete() {
		t.Fatal("a snapshot missing a side reported itself complete; the lookback is a SEMANTIC " +
			"bound as well as a chunk filter, and a market nobody has repriced in two days has " +
			"not closed at that price")
	}
}

// TestSuspendedQuotesAreNotCloses.
//
// The predicate is half-open at both ends: `suspended_at <= observed_at` and
// `observed_at < lifted_at`. A quote at the instant the suspension LIFTS counts, a
// quote at the instant it BEGINS does not — which is what makes the
// suspended-and-reopened case need no special handling at all, and is exactly the
// case the phase-9 brief called out.
func TestSuspendedQuotesAreNotCloses(t *testing.T) {
	t.Run("a market suspended and reopened before the start closes on the reopened price", func(t *testing.T) {
		m := newMarket(t)
		store := newStore(t, m.db)

		from := m.start.Add(-30 * time.Minute)
		until := m.start.Add(-10 * time.Minute)
		m.suspend(t, from, &until)

		m.price(t, m.home, 2.00, m.start.Add(-time.Hour)) // before the suspension
		m.price(t, m.home, 5.00, from)                    // AT the suspension: excluded
		m.price(t, m.home, 6.00, from.Add(time.Minute))   // inside: excluded
		m.price(t, m.home, 2.50, until)                   // AT the lift: eligible
		m.price(t, m.away, 2.10, m.start.Add(-time.Hour))

		snap, err := store.Snapshot(t.Context(), clv.SnapshotRequest{
			Market: m.marketID, Book: m.bookID,
			AsOf: m.start, NotBefore: m.start.Add(-24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if !snap.Complete() {
			t.Fatalf("snapshot is incomplete: %d of %d", len(snap.Quotes), snap.MarketSelections)
		}
		for _, q := range snap.Quotes {
			if q.Selection != m.home {
				continue
			}
			if float64(q.Decimal) != 2.50 {
				t.Fatalf("home closed at %v, want 2.50 — the first quote at or after the lift. "+
					"5.00 means the instant a suspension begins was treated as eligible; "+
					"6.00 means the episode was not excluded at all", float64(q.Decimal))
			}
		}
	})

	t.Run("a market suspended and never reopened falls back to the last open price", func(t *testing.T) {
		m := newMarket(t)
		store := newStore(t, m.db)

		from := m.start.Add(-30 * time.Minute)
		m.suspend(t, from, nil)

		m.price(t, m.home, 2.00, m.start.Add(-time.Hour))
		m.price(t, m.home, 7.00, from.Add(time.Minute)) // inside an open episode: excluded
		m.price(t, m.away, 2.10, m.start.Add(-time.Hour))

		snap, err := store.Snapshot(t.Context(), clv.SnapshotRequest{
			Market: m.marketID, Book: m.bookID,
			AsOf: m.start, NotBefore: m.start.Add(-24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if !snap.Complete() {
			t.Fatalf("snapshot is incomplete: %d of %d", len(snap.Quotes), snap.MarketSelections)
		}
		for _, q := range snap.Quotes {
			if q.Selection == m.home && float64(q.Decimal) != 2.00 {
				t.Fatalf("home closed at %v, want the last price at which the market was OPEN "+
					"(2.00); a suspended market's stale quote is not a close",
					float64(q.Decimal))
			}
		}
	})
}

// TestGradedLegsAwaitingCLVIsAWorkQueueAndNotACursor.
//
// The CLV pass keeps no cursor: it recomputes the window `[now − RetryWindow, now)`
// on every tick and asks for the legs in it that have no row yet. Two properties
// make that terminate, and both live in the statement:
//
//   - a leg that already has a `wager_leg_clv` row is not returned again, so the
//     queue drains as rows land rather than being re-measured for ever;
//   - a PENDING leg is never returned, because CLV is a claim about a graded
//     outcome and `wager_leg_clv.leg_status` cannot hold `pending` at all.
//
// The ordering `(graded_at, id)` ASC is the third: it is what lets the pass step
// its lower bound forward through the window so a batch of permanently
// unmeasurable legs cannot starve everything graded behind them.
func TestGradedLegsAwaitingCLVIsAWorkQueueAndNotACursor(t *testing.T) {
	m := newMarket(t)
	store := newStore(t, m.db)
	ctx := t.Context()

	user := "usr_" + string(m.marketID)
	const hash = "$argon2id$v=19$m=65536,t=3,p=4$c2hhcnBsaW5lLXBnY2x2AAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	exec(t, m.db, `
INSERT INTO users (id, email, password_hash, password_changed_at, status)
VALUES ($1, $2, $3, $4, 'active')`,
		user, string(m.marketID)+"@sharpline.invalid", hash, time.Now().UTC())

	graded := m.start.Add(3 * time.Hour)
	settled := m.wager(t, user, m.home, "home", "won", 2.0, graded)
	pending := m.wager(t, user, m.away, "away", "pending", 2.0, time.Time{})

	legs, err := store.GradedLegsAwaitingCLV(ctx, graded.Add(-time.Hour), graded.Add(time.Hour), 50)
	if err != nil {
		t.Fatalf("GradedLegsAwaitingCLV: %v", err)
	}
	if len(legs) != 1 {
		t.Fatalf("the queue holds %d legs, want 1: a pending leg has no graded outcome to be "+
			"measured against and cannot be stored under a status the column admits", len(legs))
	}
	if legs[0].LegID != settled {
		t.Fatalf("the queue served %s, want the graded leg %s", legs[0].LegID, settled)
	}
	if legs[0].Status != domain.LegStatusWon {
		t.Fatalf("leg status came back as %v, want won", legs[0].Status)
	}
	_ = pending

	// Writing the row must take the leg OFF the queue. Without that the pass
	// re-measures the same leg on every tick for the whole retry window.
	writeMeasurement(t, store, m, legs[0])
	legs, err = store.GradedLegsAwaitingCLV(ctx, graded.Add(-time.Hour), graded.Add(time.Hour), 50)
	if err != nil {
		t.Fatalf("GradedLegsAwaitingCLV: %v", err)
	}
	if len(legs) != 0 {
		t.Fatalf("the queue still holds %d legs after the row landed; it is not an anti-join and "+
			"the pass would never drain", len(legs))
	}

	t.Run("a non-positive batch limit is refused rather than read as a drained queue", func(t *testing.T) {
		if _, err := store.GradedLegsAwaitingCLV(ctx, graded.Add(-time.Hour), graded, 0); err == nil {
			t.Fatal("LIMIT 0 was accepted; it returns no rows, which a caller reads as " +
				"'nothing to measure' — indistinguishable from a drained queue, and permanent")
		}
	})

	t.Run("an unbounded snapshot request is refused", func(t *testing.T) {
		_, err := store.Snapshot(ctx, clv.SnapshotRequest{
			Market: m.marketID, Book: m.bookID, AsOf: m.start,
		})
		if err == nil {
			t.Fatal("a snapshot with no lower bound was accepted; an unbounded read of the " +
				"prices hypertable consults every chunk that has ever existed")
		}
	})
}

// wager writes one straight wager and its single leg in ONE transaction, because
// migration 00006 declares a deferrable trigger that refuses a wager with no legs
// at COMMIT.
func (m market) wager(t *testing.T, user string, sel domain.SelectionID, role, legStatus string,
	dec float64, graded time.Time,
) domain.LegID {
	t.Helper()
	ctx := t.Context()

	seq.n++
	tok := fmt.Sprintf("%s-%d", m.marketID, seq.n)
	wid, lid := "wgr_"+tok, "leg_"+tok

	status, returned := "placed", any(nil)
	var gradedCol any
	transitioned := m.start
	if legStatus != "pending" {
		status, returned = "won", int64(2000)
		gradedCol = graded
		transitioned = graded
	}

	tx, err := m.db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var net any
	if returned != nil {
		net = returned.(int64) - 1000
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO wagers (id, user_id, kind, status, stake_minor, accepted_decimal, rounding,
                    potential_payout_minor, potential_profit_minor,
                    returned_minor, net_return_minor, placed_at, transitioned_at)
VALUES ($1, $2, 'straight', $3, 1000, $4, 'half_to_even', 2000, 1000, $5, $6, $7, $8)`,
		wid, user, status, dec, returned, net, m.start, transitioned); err != nil {
		t.Fatalf("insert wager: %v", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO legs (id, wager_id, event_id, market_id, market_type, selection_id, role,
                  price_book_id, price_decimal, price_line, price_observed_at, status, graded_at)
VALUES ($1, $2, $3, $4, 'moneyline', $5, $6, $7, $8, NULL, $9, $10, $11)`,
		lid, wid, m.eventID, m.marketID, sel, role, m.bookID, dec,
		m.start.Add(-time.Hour), legStatus, gradedCol); err != nil {
		t.Fatalf("insert leg: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return domain.LegID(lid)
}

// writeMeasurement records a minimal, schema-valid measurement for a leg. The
// numbers are arithmetic on the two fair probabilities rather than chosen, because
// migration 00009 re-derives `probability_clv`, `magnitude`, `beat_close`,
// `line_moved` and `voided` and refuses a row whose fields disagree.
func writeMeasurement(t *testing.T, store *pgclv.Store, m market, leg clv.Leg) {
	t.Helper()

	// float64 VARIABLES, not untyped constants. Go evaluates constant arithmetic at
	// arbitrary precision, so `const c = 0.52 - 0.50` is the float64 nearest 0.02,
	// while PostgreSQL subtracts two stored doubles and gets 0.020000000000000018 —
	// and `wager_leg_clv_probability_identity` compares them for EQUALITY, on
	// purpose, because a single IEEE subtraction is the one CLV quantity that is
	// bit-identical across Go and SQL. Writing this the constant way makes the
	// database reject the row, which is the constraint working.
	var takenFair, closingFair = 0.50, 0.52
	prob := closingFair - takenFair
	pct := prob / takenFair * 100
	if err := store.WriteLegCLV(t.Context(), clv.Measurement{
		Leg:         leg,
		LeagueID:    m.leagueID,
		ClosingBook: m.bookID,
		DevigMethod: odds.MethodShin,
		Result: odds.CLVResult{
			Market:         m.marketID,
			Selection:      leg.SelectionID,
			TakenBook:      m.bookID,
			ClosingBook:    m.bookID,
			Line:           domain.NoLine(),
			ClosingLine:    domain.NoLine(),
			TakenAt:        leg.ObservedAt,
			ClosedAt:       m.start,
			TakenFair:      odds.Probability(takenFair),
			ClosingFair:    odds.Probability(closingFair),
			TakenPrice:     odds.Decimal(1 / takenFair),
			ClosingPrice:   odds.Decimal(1 / closingFair),
			ProbabilityCLV: prob,
			PercentCLV:     pct,
			Beat:           prob > odds.CLVTieBand,
			Magnitude:      pct,
			LineMoved:      false,
		},
		ComputedAt: leg.GradedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("WriteLegCLV: %v", err)
	}
}
