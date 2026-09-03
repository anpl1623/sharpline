package pgstore_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/anpl1623/sharpline/internal/betting"
	bettingpg "github.com/anpl1623/sharpline/internal/betting/pgstore"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/migrate"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
	"github.com/anpl1623/sharpline/internal/settlement"
	"github.com/anpl1623/sharpline/internal/settlement/pgstore"
)

// The settlement data plane, against a REAL TimescaleDB.
//
// # What this tier adds to test/integration's
//
// test/integration proves the rows-affected contract holds under redelivery:
// the second grade and the second settlement both report their sentinel. This
// package goes at the two places that contract is thinnest.
//
//   - A REFUSAL THAT CHANGES THE ROW ANYWAY is indistinguishable from a refusal
//     that does not, if nobody reads the row back. Both guarded UPDATEs are
//     driven a second time HERE with DIFFERENT VALUES — a different grading, a
//     smaller payout — and the stored row is then read to prove it did not move.
//     A redelivery carrying a corrected result is exactly how that would happen
//     in production, and it is the one shape "call it twice with the same
//     argument" cannot see.
//
//   - EVERY LEG test/integration GRADES IS A MONEYLINE LEG, whose grading line
//     is NULL. ListPendingLegsForEvent projects COALESCE(teased_line, price_line)
//     — Leg.GradingLine() in SQL — and neither branch of that COALESCE has ever
//     produced a value in a test. A spread graded against an absent line is
//     graded as a moneyline, and a teaser graded against its BOOKED line rather
//     than its teased one grades at a handicap nobody sold.
//
// # No mock data
//
// Every ticket here is placed through internal/betting/pgstore, the same path a
// customer's slip takes, and every row is written by the test that asserts on
// it. Nothing is seeded and no canned result appears anywhere.
//
// # It fails rather than skips
//
// A silently skipped integration test reports green while proving nothing, and
// the CI job meant to enforce the prime directive becomes decorative.

// postgresImage is the compose stack's `postgres` image, pinned by digest — the
// same image at the same digest test/integration uses.
const postgresImage = "timescale/timescaledb:latest-pg17@sha256:981e3016a2810fec47515e3828ad70ae97b84f4c9ef63d032180b54f61566fd6"

const (
	pgUser     = "sharpline"
	pgPassword = "test-only-throwaway"
	pgDatabase = "sharpline_settlementstore"

	// startDeadline bounds one container boot including the image pull on a cold
	// cache. TimescaleDB's entrypoint starts the server twice, which is why the
	// log wait below asks for two occurrences.
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

// stores opens a pool and builds BOTH adapters over it.
//
// The betting adapter is here because settlement has no INSERT statement of its
// own — by design, since wagers_assert_transition freezes the booked terms after
// insert — so the only honest way to obtain a ticket to settle is to place one
// down the path a customer's slip takes. A test that hand-wrote its rows would
// be settling a fixture rather than a bet.
func stores(t *testing.T) (*pgstore.Store, *bettingpg.Store, *postgres.DB) {
	t.Helper()

	db, err := postgres.Connect(t.Context(), postgres.Options{
		DSN:     testDSN,
		Service: "settlement-store-test",
		Logger:  discard(),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	settleStore, err := pgstore.NewStore(db)
	if err != nil {
		t.Fatalf("new settlement store: %v", err)
	}
	betStore, err := bettingpg.New(db)
	if err != nil {
		t.Fatalf("new betting store: %v", err)
	}
	return settleStore, betStore, db
}

// results builds the results feed over the same pool.
func results(t *testing.T, db *postgres.DB) *pgstore.Results {
	t.Helper()

	r, err := pgstore.NewResults(pgstore.ResultsOptions{DB: db, Logger: discard()})
	if err != nil {
		t.Fatalf("new results source: %v", err)
	}
	return r
}

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

// execer is the one pgx method every fixture needs, declared at the consumer and
// kept to a single method (CLAUDE.md §12).
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// seq numbers every fixture id in the package. Tests call t.Parallel(), so the
// increment has to be atomic — a bare seq++ is a read-modify-write the race
// detector flags, and `make test-race` runs these.
var seq atomic.Uint64

func uniqueID(prefix string) string { return fmt.Sprintf("%s_ss%d", prefix, seq.Add(1)) }

// catalogue is one test's private sport -> league -> event -> book chain.
type catalogue struct {
	EventID domain.EventID
	BookID  domain.BookID
	Home    string
	Away    string
}

// newCatalogue inserts the catalogue rows a leg's foreign keys require.
//
// The event is 'match' with status 'scheduled', the only combination that
// satisfies events_competitors_match_kind and events_clock_only_in_play with no
// extra columns.
func newCatalogue(t *testing.T, ctx context.Context, x execer) catalogue {
	t.Helper()

	tok := uniqueID("cat")
	c := catalogue{
		EventID: mustEventID(t, "event_"+tok),
		BookID:  mustBookID(t, "book_"+tok),
		Home:    "Home " + tok,
		Away:    "Away " + tok,
	}
	sport := mustSportID(t, "sport_"+tok)
	league := mustLeagueID(t, "league_"+tok)

	mustExec(t, ctx, x, `INSERT INTO sports (id, slug, name) VALUES ($1, $2, $3)`,
		sport, mustSlug(t, "sport-"+tok), "Sport "+tok)
	mustExec(t, ctx, x, `INSERT INTO leagues (id, sport_id, slug, name) VALUES ($1, $2, $3, $4)`,
		league, sport, mustSlug(t, "league-"+tok), "League "+tok)
	mustExec(t, ctx, x, `
INSERT INTO events (id, league_id, kind, name,
                    home_competitor_name, away_competitor_name,
                    scheduled_start, status, observed_at)
VALUES ($1, $2, 'match', $3, $4, $5, $6, 'scheduled', $7)`,
		c.EventID, league, c.Home+" at "+c.Away, c.Home, c.Away,
		time.Now().UTC().Add(3*time.Hour).Truncate(time.Microsecond), time.Now().UTC())
	mustExec(t, ctx, x,
		`INSERT INTO books (id, slug, name, kind, is_reference) VALUES ($1, $2, $3, 'external', FALSE)`,
		c.BookID, mustSlug(t, "book-"+tok), "Book "+tok)

	return c
}

// market is a market plus its home and away selections.
type market struct {
	ID   domain.MarketID
	Type domain.MarketType
	Line domain.Line
	Home domain.SelectionID
	Away domain.SelectionID
}

// newMarket inserts a market of the given type and its two selections.
//
// `line` is a domain.Line and not a float64 because markets_line_rule
// distinguishes NULL from a value and 0.0 IS a value — a pick'em. That
// distinction is the subject of the grading-line test, so the fixture has to be
// able to express it.
func newMarket(t *testing.T, ctx context.Context, x execer, c catalogue,
	typ domain.MarketType, line domain.Line,
) market {
	t.Helper()

	tok := uniqueID("mkt")
	m := market{
		ID:   mustMarketID(t, "market_"+tok),
		Type: typ,
		Line: line,
		Home: mustSelectionID(t, "sel_h_"+tok),
		Away: mustSelectionID(t, "sel_a_"+tok),
	}

	var stored *float64
	if v, ok := line.Value(); ok {
		stored = &v
	}
	mustExec(t, ctx, x, `
INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
VALUES ($1, $2, $3, $4, NULL, 'open', $5)`,
		m.ID, c.EventID, typ.String(), stored, time.Now().UTC())

	for i, s := range []domain.SelectionID{m.Home, m.Away} {
		role := domain.SelectionRoleHome
		if i == 1 {
			role = domain.SelectionRoleAway
		}
		mustExec(t, ctx, x, `
INSERT INTO selections (id, market_id, market_type, role, name)
VALUES ($1, $2, $3, $4, $5)`, s, m.ID, typ.String(), role.String(), "Selection "+s.String())
	}
	return m
}

// newUser inserts an active customer.
//
// THE PASSWORD HASH IS A SHAPE, NOT A SECRET. users_password_hash_is_argon2id
// requires the value to start `$argon2id$` and users_password_hash_length
// requires 40..512 characters, so the column cannot hold a placeholder. This
// hashes no password and is never verified; it exists so the row is insertable.
func newUser(t *testing.T, ctx context.Context, x execer) domain.UserID {
	t.Helper()

	tok := uniqueID("user")
	id := mustUserID(t, "usr_"+tok)
	email := fmt.Sprintf("ss-%s@sharpline.invalid", tok)
	const hash = "$argon2id$v=19$m=65536,t=3,p=4$c2hhcnBsaW5lLXNldHRsZW1lbnQ$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	mustExec(t, ctx, x, `
INSERT INTO users (id, email, password_hash, password_changed_at, status)
VALUES ($1, $2, $3, $4, 'active')`, id, email, hash, time.Now().UTC())
	return id
}

// bookedQuote writes one quote into the hypertable and returns it as the
// domain.Price a leg is booked at. The row is written as well as returned
// because a leg's booked price is a COPY of a quote that really existed.
func bookedQuote(t *testing.T, ctx context.Context, x execer, sel domain.SelectionID, book domain.BookID,
	decimal float64, line domain.Line, at time.Time,
) domain.Price {
	t.Helper()

	var stored *float64
	if v, ok := line.Value(); ok {
		stored = &v
	}
	mustExec(t, ctx, x, `
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES ($1, $2, $3, $4, $5, $5)`, sel, book, decimal, stored, at)

	price, err := domain.NewPrice(domain.PriceParams{
		SelectionID: sel,
		BookID:      book,
		Decimal:     decimal,
		Line:        line,
		ObservedAt:  at,
	})
	if err != nil {
		t.Fatalf("NewPrice: %v", err)
	}
	return price
}

// homeLeg builds one pending leg on a market's home side, optionally teased.
func homeLeg(t *testing.T, c catalogue, m market, price domain.Price, teased domain.Line) domain.Leg {
	t.Helper()

	l, err := domain.NewLeg(domain.LegParams{
		ID:          mustLegID(t, uniqueID("leg")),
		EventID:     c.EventID,
		MarketID:    m.ID,
		MarketType:  m.Type,
		Role:        domain.SelectionRoleHome,
		SelectionID: m.Home,
		Price:       price,
		TeasedLine:  teased,
	})
	if err != nil {
		t.Fatalf("NewLeg: %v", err)
	}
	return l
}

// placeStraight books a single-leg ticket on the given market through the
// betting adapter, and returns the domain value that was written.
//
// It goes through the placement path rather than raw SQL on purpose: a grading
// test that built its own rows would be grading a fixture, and the question
// worth asking is whether a ticket the placement path produced can be settled by
// the settlement path.
//
// The accepted price EQUALS the leg's quote, because validateTicketPrice and the
// deferred wagers_shape_at_commit trigger both require it of a straight — the
// two numbers are one value travelling by two routes.
func placeStraight(t *testing.T, ctx context.Context, x execer, store *bettingpg.Store, c catalogue,
	m market, user domain.UserID, decimal float64, stake domain.Money, at time.Time,
) domain.Wager {
	t.Helper()

	price := bookedQuote(t, ctx, x, m.Home, c.BookID, decimal, m.Line, at)
	w, err := domain.NewWager(domain.WagerParams{
		ID:              mustWagerID(t, uniqueID("wager")),
		UserID:          user,
		Kind:            domain.WagerKindStraight,
		Legs:            []domain.Leg{homeLeg(t, c, m, price, domain.NoLine())},
		Stake:           stake,
		AcceptedDecimal: price.Decimal(),
		Rounding:        domain.RoundHalfAwayFromZero,
		PlacedAt:        at,
	})
	if err != nil {
		t.Fatalf("NewWager: %v", err)
	}
	place(t, ctx, store, w)
	return w
}

// place writes a ticket through the betting adapter, in one transaction.
//
// A wager and its legs CANNOT be written on autocommit: wagers_shape_at_commit
// is DEFERRABLE INITIALLY DEFERRED precisely so the parent may exist before the
// legs, and on autocommit it would fire at the end of the INSERT into wagers and
// find zero legs.
func place(t *testing.T, ctx context.Context, store *bettingpg.Store, w domain.Wager) {
	t.Helper()

	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertWager(ctx, w)
	}); err != nil {
		t.Fatalf("place wager %s: %v", w.ID(), err)
	}
}

// settleTx runs one settlement transaction and fails the test if it does not
// commit. It exists so a test's assertions read as the settlement they describe
// rather than as transaction plumbing.
func settleTx(t *testing.T, ctx context.Context, store *pgstore.Store,
	fn func(ctx context.Context, tx settlement.Tx) error,
) {
	t.Helper()
	if err := store.InTx(ctx, fn); err != nil {
		t.Fatalf("settlement transaction: %v", err)
	}
}

func mustExec(t *testing.T, ctx context.Context, x execer, sql string, args ...any) {
	t.Helper()
	if _, err := x.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec failed: %v\nSQL: %s", err, sql)
	}
}

// -----------------------------------------------------------------------------
// Typed constructors
// -----------------------------------------------------------------------------
//
// Every fixture id goes through the domain's own constructor rather than being
// cast: the constructors are the bound the schema's charset CHECKs mirror, so a
// fixture the domain would reject fails here with the domain's message instead
// of as a constraint violation forty lines later.

func mustSportID(t *testing.T, s string) domain.SportID {
	t.Helper()
	v, err := domain.NewSportID(s)
	if err != nil {
		t.Fatalf("NewSportID(%q): %v", s, err)
	}
	return v
}

func mustLeagueID(t *testing.T, s string) domain.LeagueID {
	t.Helper()
	v, err := domain.NewLeagueID(s)
	if err != nil {
		t.Fatalf("NewLeagueID(%q): %v", s, err)
	}
	return v
}

func mustEventID(t *testing.T, s string) domain.EventID {
	t.Helper()
	v, err := domain.NewEventID(s)
	if err != nil {
		t.Fatalf("NewEventID(%q): %v", s, err)
	}
	return v
}

func mustMarketID(t *testing.T, s string) domain.MarketID {
	t.Helper()
	v, err := domain.NewMarketID(s)
	if err != nil {
		t.Fatalf("NewMarketID(%q): %v", s, err)
	}
	return v
}

func mustSelectionID(t *testing.T, s string) domain.SelectionID {
	t.Helper()
	v, err := domain.NewSelectionID(s)
	if err != nil {
		t.Fatalf("NewSelectionID(%q): %v", s, err)
	}
	return v
}

func mustBookID(t *testing.T, s string) domain.BookID {
	t.Helper()
	v, err := domain.NewBookID(s)
	if err != nil {
		t.Fatalf("NewBookID(%q): %v", s, err)
	}
	return v
}

func mustUserID(t *testing.T, s string) domain.UserID {
	t.Helper()
	v, err := domain.NewUserID(s)
	if err != nil {
		t.Fatalf("NewUserID(%q): %v", s, err)
	}
	return v
}

func mustWagerID(t *testing.T, s string) domain.WagerID {
	t.Helper()
	v, err := domain.NewWagerID(s)
	if err != nil {
		t.Fatalf("NewWagerID(%q): %v", s, err)
	}
	return v
}

func mustLegID(t *testing.T, s string) domain.LegID {
	t.Helper()
	v, err := domain.NewLegID(s)
	if err != nil {
		t.Fatalf("NewLegID(%q): %v", s, err)
	}
	return v
}

func mustSlug(t *testing.T, s string) domain.Slug {
	t.Helper()
	v, err := domain.NewSlug(s)
	if err != nil {
		t.Fatalf("NewSlug(%q): %v", s, err)
	}
	return v
}

func mustLine(t *testing.T, v float64) domain.Line {
	t.Helper()
	l, err := domain.NewLine(v)
	if err != nil {
		t.Fatalf("NewLine(%v): %v", v, err)
	}
	return l
}
