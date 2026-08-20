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
	"github.com/anpl1623/sharpline/internal/betting/pgstore"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/migrate"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// The betting data plane, against a REAL TimescaleDB.
//
// # Why this tier exists when test/integration already drives this adapter
//
// test/integration proves the four headline properties of the placement path —
// a balanced movement commits, a deferred violation surfaces from COMMIT, a
// replay collides on wagers_pkey, migration 00008's trigger refuses an excluded
// customer. What it does not reach is the ADAPTER'S EDGES, and this package is
// the translation layer between the domain and the money tables, so its edges
// are where a defect is both most likely and least visible:
//
//   - every leg test/integration writes is a MONEYLINE leg, whose line is NULL.
//     Nothing there has ever round-tripped a leg that carries a line, so nothing
//     has ever proved that a stored 0.0 comes back as a PICK'EM rather than as
//     an absent line. That single collapse turns every pick'em spread into a
//     moneyline at grading time.
//   - [betting.Tx.GrantCredit] is not exercised at all, and its whole job is to
//     return ONE side of a two-sided movement for ONE customer. A predicate the
//     wrong way round returns the issuance half — the same magnitude, negative —
//     or somebody else's grant.
//   - the not-found mappings (ErrQuoteUnavailable, ErrMarketNotOpen,
//     ErrWagerNotFound, ErrGrantNotFound) are the difference between a slip
//     saying "this book is not pricing that" and a 500.
//
// # No mock data
//
// Every row here is written BY a test FOR that test and asserted on by the test
// that wrote it. Nothing is seeded, nothing stands in for the ingest pipeline,
// and no canned query result appears anywhere. Where a CHECK constraint demands
// a particular shape — a spread market needs a line, users.password_hash must
// match `$argon2id$%` — the fixture satisfies the shape and says which
// constraint demanded it.
//
// # It fails rather than skips
//
// A silently skipped integration test reports green while proving nothing, and
// the CI job meant to enforce the prime directive becomes decorative.

// postgresImage is the compose stack's `postgres` image, pinned by digest — the
// same image at the same digest test/integration and internal/httpapi/pgstore
// use. A stock postgres has no TimescaleDB and the `prices` hypertable, which
// every booked quote here is written into, does not exist without it.
const postgresImage = "timescale/timescaledb:latest-pg17@sha256:981e3016a2810fec47515e3828ad70ae97b84f4c9ef63d032180b54f61566fd6"

const (
	pgUser     = "sharpline"
	pgPassword = "test-only-throwaway"
	pgDatabase = "sharpline_bettingstore"

	// startDeadline bounds one container boot including the image pull on a cold
	// cache. TimescaleDB's entrypoint starts the server twice — once to run the
	// initdb scripts, once for real — which is why the log wait asks for two
	// occurrences.
	startDeadline = 4 * time.Minute
)

// testDSN addresses the one container this package starts. A single migrated
// server is shared by every test because every test mints its own ids from
// uniqueID and asserts only on rows it wrote, so nothing here is order-dependent
// or unsafe under t.Parallel().
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

// newStore opens a pool and builds the adapter.
//
// Every test gets its OWN pool, which is not tidiness: one test here closes its
// pool deliberately, to prove that a dead database surfaces as an error rather
// than as a zero balance, and a shared pool would take the rest of the package
// down with it.
func newStore(t *testing.T) (*pgstore.Store, *postgres.DB) {
	t.Helper()

	db, err := postgres.Connect(t.Context(), postgres.Options{
		DSN:     testDSN,
		Service: "betting-store-test",
		Logger:  discard(),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	store, err := pgstore.New(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store, db
}

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------
//
// Shapes are dictated by the schema's own CHECK constraints, not invented. Where
// a value looks arbitrary it is there because a constraint demands something in
// that position, and the comment names the constraint.

// execer is the one pgx method every fixture needs, declared at the consumer and
// kept to a single method (CLAUDE.md §12). Both *pgxpool.Pool and pgx.Tx satisfy
// it, so a fixture can be built on the pool or inside a transaction the test
// controls.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// seq numbers every fixture id in the package. Tests call t.Parallel(), so the
// increment has to be atomic: a bare seq++ is a read-modify-write the race
// detector flags, and `make test-race` runs these.
var seq atomic.Uint64

func uniqueID(prefix string) string { return fmt.Sprintf("%s_bs%d", prefix, seq.Add(1)) }

// catalogue is one test's private slice of the catalogue: a sport, a league, a
// match event and a book, all of which a leg's foreign keys require.
type catalogue struct {
	EventID domain.EventID
	BookID  domain.BookID
	Home    string
	Away    string
}

// newCatalogue inserts sport -> league -> event -> book.
//
// The event is 'match' with status 'scheduled', the only combination that
// satisfies events_competitors_match_kind (both competitor names present) and
// events_clock_only_in_play (no clock outside live/suspended) with no extra
// columns.
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
	ID    domain.MarketID
	Type  domain.MarketType
	Line  domain.Line
	Home  domain.SelectionID
	Away  domain.SelectionID
	Roles [2]domain.SelectionRole
}

// newMarket inserts a market of the given type and its two selections.
//
// markets_line_rule is the reason `line` is a domain.Line rather than a float64:
// a moneyline must carry NULL, a spread must carry a value — and 0.0 IS a value,
// a pick'em, which is precisely the distinction the tests below exist to prove
// survives a round trip. Passing a float64 with a sentinel here would have made
// the fixture unable to express the case under test.
func newMarket(t *testing.T, ctx context.Context, x execer, c catalogue, typ domain.MarketType, line domain.Line) market {
	t.Helper()

	tok := uniqueID("mkt")
	m := market{
		ID:   mustMarketID(t, "market_"+tok),
		Type: typ,
		Line: line,
		Home: mustSelectionID(t, "sel_h_"+tok),
		Away: mustSelectionID(t, "sel_a_"+tok),
	}
	// Home and away, which selections_role_allowed permits for both market types
	// the tests here use. A total would need over/under and a different pair of
	// field names; nothing in this package books one, so the fixture does not
	// pretend to.
	m.Roles = [2]domain.SelectionRole{domain.SelectionRoleHome, domain.SelectionRoleAway}

	var stored *float64
	if v, ok := line.Value(); ok {
		stored = &v
	}
	mustExec(t, ctx, x, `
INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
VALUES ($1, $2, $3, $4, NULL, 'open', $5)`,
		m.ID, c.EventID, typ.String(), stored, time.Now().UTC())

	for i, s := range []domain.SelectionID{m.Home, m.Away} {
		mustExec(t, ctx, x, `
INSERT INTO selections (id, market_id, market_type, role, name)
VALUES ($1, $2, $3, $4, $5)`, s, m.ID, typ.String(), m.Roles[i].String(), "Selection "+s.String())
	}
	return m
}

// newUser inserts an active customer.
//
// THE PASSWORD HASH IS A SHAPE, NOT A SECRET. users_password_hash_is_argon2id
// requires the value to start `$argon2id$` and users_password_hash_length
// requires 40..512 characters, so the column cannot hold a placeholder. This is
// a structurally valid argon2id encoding of nothing: it hashes no password, is
// never verified, and exists only so the row is insertable.
func newUser(t *testing.T, ctx context.Context, x execer) domain.UserID {
	t.Helper()

	tok := uniqueID("user")
	id := mustUserID(t, "usr_"+tok)
	// users_email_normalised requires lower(email) = email and users_email_shape
	// requires exactly one @ with no whitespace.
	email := fmt.Sprintf("bs-%s@sharpline.invalid", tok)
	const hash = "$argon2id$v=19$m=65536,t=3,p=4$c2hhcnBsaW5lLWJldHRpbmc$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	mustExec(t, ctx, x, `
INSERT INTO users (id, email, password_hash, password_changed_at, status)
VALUES ($1, $2, $3, $4, 'active')`, id, email, hash, time.Now().UTC())
	return id
}

// bookedQuote writes one quote into the hypertable and returns it as the
// domain.Price a leg would be booked at.
//
// The row is written as well as returned because [betting.Tx.QuoteFor] reads it
// back — the whole point of that method is that the customer never supplies a
// price — and because a leg's booked price is a COPY of a quote that really
// existed, not a number invented at placement.
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
//
// Pass domain.NoLine() for teased on everything that is not a teaser leg:
// validateTeaser refuses a teased line on any other wager kind, and
// legs_teasable_market_type refuses one on any market type with no line to move.
func homeLeg(t *testing.T, c catalogue, m market, price domain.Price, teased domain.Line) domain.Leg {
	t.Helper()

	l, err := domain.NewLeg(domain.LegParams{
		ID:          mustLegID(t, uniqueID("leg")),
		EventID:     c.EventID,
		MarketID:    m.ID,
		MarketType:  m.Type,
		Role:        m.Roles[0],
		SelectionID: m.Home,
		Price:       price,
		TeasedLine:  teased,
	})
	if err != nil {
		t.Fatalf("NewLeg: %v", err)
	}
	return l
}

// straight builds a placeable single-leg ticket.
//
// The accepted price EQUALS the leg's quote, because validateTicketPrice and the
// deferred wagers_shape_at_commit trigger both require it of a straight — the
// two numbers are one value travelling by two routes.
func straight(t *testing.T, user domain.UserID, l domain.Leg, stake domain.Money,
	rounding domain.Rounding, at time.Time,
) domain.Wager {
	t.Helper()

	w, err := domain.NewWager(domain.WagerParams{
		ID:              mustWagerID(t, uniqueID("wager")),
		UserID:          user,
		Kind:            domain.WagerKindStraight,
		Legs:            []domain.Leg{l},
		Stake:           stake,
		AcceptedDecimal: l.QuotedDecimal(),
		Rounding:        rounding,
		PlacedAt:        at,
	})
	if err != nil {
		t.Fatalf("NewWager: %v", err)
	}
	return w
}

// place writes a ticket through the adapter, in one transaction, as the service
// would.
//
// A wager and its legs CANNOT be written on autocommit: wagers_shape_at_commit
// is DEFERRABLE INITIALLY DEFERRED precisely so the parent may exist before the
// legs that would otherwise contradict it, and on autocommit it would fire at
// the end of the INSERT into wagers and find zero legs.
func place(t *testing.T, ctx context.Context, store *pgstore.Store, w domain.Wager) {
	t.Helper()

	if err := store.InTx(ctx, func(ctx context.Context, tx betting.Tx) error {
		return tx.InsertWager(ctx, w)
	}); err != nil {
		t.Fatalf("place wager %s: %v", w.ID(), err)
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

func mustTransactionID(t *testing.T, s string) domain.TransactionID {
	t.Helper()
	v, err := domain.NewTransactionID(s)
	if err != nil {
		t.Fatalf("NewTransactionID(%q): %v", s, err)
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
