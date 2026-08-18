// The container harness for this package's integration tier, plus the payload
// builder every test writes records with.
//
// # Real containers, and why they FAIL rather than skip
//
// CLAUDE.md §10: "Integration tests use testcontainers-go against real
// Postgres/Redis/Kafka — no mocked databases, and no mocked broker either,
// because the interesting bugs live in consumer-group rebalancing and offset
// handling." Everything this package must prove is a property of the real
// engine: that the natural-key index absorbs a redelivery, that a trigger
// refuses an update, that `xmax = 0` distinguishes an insert from an update,
// that a hypertable accepts ON CONFLICT at all. A fake database reproduces the
// API and none of it.
//
// A missing Docker socket FAILS these tests. A silently skipped integration test
// reports green while proving nothing, which is worse than not having one.
//
// # NO MOCK DATA
//
// newPayload builds a market from arguments the calling test supplies, and the
// test then asserts on the rows THAT payload produced. Nothing is seeded, no
// fixture file is loaded, and no canned row stands in for ingested data. The
// builder exists so that a test can state the one thing it is about — a line
// that moved, an observation that arrived late — without respelling the other
// forty fields, not so that a shared blob of pretend odds can be reused.
package writer_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/writer"
	"github.com/anpl1623/sharpline/internal/platform/migrate"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// postgresImage is the compose stack's `postgres` image, pinned by digest, from
// the contract ledger. Do not float it: `prices` is a TimescaleDB hypertable and
// stock postgres has none of the behaviour under test here.
const postgresImage = "timescale/timescaledb:latest-pg17@sha256:981e3016a2810fec47515e3828ad70ae97b84f4c9ef63d032180b54f61566fd6"

// NO BROKER IS STARTED HERE, deliberately.
//
// This package's unit of work is one *kafka.Delivery, and every assertion below
// is about what that record does to Postgres. Standing a broker up would add
// ~20s and a hundred lines of KRaft listener wiring to prove something this
// package does not own.
//
// The end-to-end assertion — a real Consumer driving a real Writer, so that "the
// handler returned nil" and "the offset was committed" are observed together —
// is a genuine gap and it belongs in test/integration, where
// kafka_fixture_test.go already stands up a KRaft broker with the correct
// advertised-listener dance and auto-topic-creation OFF. It is recorded as a
// cross-package request rather than reimplemented here.

const (
	pgUser     = "sharpline_writer_it"
	pgPassword = "sharpline_writer_it_password"
	pgDatabase = "sharpline_writer_it"
)

// containerStartDeadline bounds one container boot including a cold image pull.
// TimescaleDB's entrypoint starts the server twice, which is why the log wait
// asks for two occurrences.
const containerStartDeadline = 4 * time.Minute

// -----------------------------------------------------------------------------
// Postgres
// -----------------------------------------------------------------------------

var (
	pgOnce sync.Once
	pgDSN  string
	pgErr  error
)

// sharedDSN returns a migrated database, started at most once for the package.
//
// It is shared because a migrated TimescaleDB boot is several seconds and every
// test here only reads and writes rows in a key space it mints for itself — see
// uniqueSuffix. Nothing in this file reads a row another test wrote.
func sharedDSN(t *testing.T) string {
	t.Helper()

	pgOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), containerStartDeadline+2*time.Minute)
		defer cancel()

		dsn, err := startPostgres(ctx)
		if err != nil {
			pgErr = fmt.Errorf("start postgres: %w", err)
			return
		}
		if err := applyMigrations(ctx, dsn); err != nil {
			pgErr = fmt.Errorf("migrate: %w", err)
			return
		}
		pgDSN = dsn
	})

	if pgErr != nil {
		t.Fatalf("the shared database is unavailable, so nothing in this package can run: %v", pgErr)
	}
	return pgDSN
}

func startPostgres(ctx context.Context) (string, error) {
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        postgresImage,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     pgUser,
				"POSTGRES_PASSWORD": pgPassword,
				"POSTGRES_DB":       pgDatabase,
				// timescaledb-tune rewrites postgresql.conf on first boot, which
				// would make one container's settings depend on how much memory
				// the Docker VM happened to have. Off, so the engine under test
				// is the same every run.
				"TIMESCALEDB_TELEMETRY":     "off",
				"NO_TS_TUNE":                "true",
				"POSTGRES_HOST_AUTH_METHOD": "scram-sha-256",
			},
			WaitingFor: wait.ForAll(
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithDeadline(containerStartDeadline),
		},
		Started: true,
	})
	if err != nil {
		return "", err
	}

	host, err := container.Host(ctx)
	if err != nil {
		return "", err
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, host, port.Port(), pgDatabase), nil
}

// applyMigrations runs the real migrate runner against the container, exactly as
// the compose `migrate` service does. Nothing here hand-writes DDL: a test that
// built its own schema would prove the writer works against a schema that does
// not exist.
func applyMigrations(ctx context.Context, dsn string) error {
	runner, err := migrate.New(migrate.Options{DSN: dsn, Logger: discardLogger()})
	if err != nil {
		return err
	}
	defer func() { _ = runner.Close() }()

	summary, err := runner.Run(ctx, migrate.Invocation{Command: migrate.CommandUp})
	if err != nil {
		return err
	}
	if summary.VersionAfter <= 0 {
		return fmt.Errorf("migrate up left the schema at version %d", summary.VersionAfter)
	}
	return nil
}

// openPool connects the real pool the writer runs on, and closes it with the
// test.
func openPool(t *testing.T) *postgres.DB {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	db, err := postgres.Connect(ctx, postgres.Options{
		DSN:     sharedDSN(t),
		Service: "writer-it",
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// newWriter builds the shipped Writer on a real pool, with a registry of its own
// so a test can gather its metrics without seeing another test's counts.
func newWriter(t *testing.T, db writer.DB, opts ...func(*writer.Options)) (*writer.Writer, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()
	o := writer.Options{
		DB:       db,
		Logger:   discardLogger(),
		Registry: reg,
	}
	for _, fn := range opts {
		fn(&o)
	}

	w, err := writer.New(o)
	if err != nil {
		t.Fatalf("build writer: %v", err)
	}
	return w, reg
}

func discardLogger() *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// -----------------------------------------------------------------------------
// Unique key space
// -----------------------------------------------------------------------------

// suffixCounter makes every identifier a test mints unique within the process,
// so tests sharing one database are order-independent and safe under
// t.Parallel(). The charset is what domain.validID and every *_id_charset CHECK
// accept.
var suffixCounter atomic.Uint64

func uniqueSuffix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("it%06d", suffixCounter.Add(1))
}

// -----------------------------------------------------------------------------
// Payload builder
// -----------------------------------------------------------------------------

// market is one test's private market: the identifiers it minted and the payload
// it produces. Every field is settable so a test can move exactly the one thing
// it is about.
type market struct {
	suffix string

	sportID  domain.SportID
	leagueID domain.LeagueID
	eventID  domain.EventID
	marketID domain.MarketID
	homeSel  domain.SelectionID
	awaySel  domain.SelectionID
	bookA    domain.BookID
	bookB    domain.BookID

	// Everything below is what a test moves.
	eventStatus  domain.EventStatus
	marketStatus domain.MarketStatus
	marketLine   domain.Line
	marketObsAt  time.Time
	ingestedAt   time.Time
	quoteObsAt   time.Time
	homeDecimal  float64
	awayDecimal  float64
	homeLine     domain.Line
	awayLine     domain.Line

	// bookBSlug is separable from bookB's id so a test can force a violation
	// only the DATABASE can raise: books.slug is UNIQUE, and the writer's own
	// validator checks duplicate book IDs, not duplicate slugs.
	bookBSlug domain.Slug
}

// eventObsAt is the event's provider observation instant.
//
// It is DERIVED from the market's, not stored beside it, because the wire record
// carries exactly one observation instant per record: normalizer's EventRef has
// no time field and EventRef.domain takes the market's. A second copy of one
// fact is a second copy that drifts — which is precisely the bug this test file
// was rewritten to catch, in the other direction.
func (m *market) eventObsAt() time.Time { return m.marketObsAt }

// newMarket mints a fresh, self-consistent spread market: one event, two
// selections, two books, four quotes.
//
// A spread rather than a moneyline on purpose: it is the market type where
// Market.Line and Quote.Line are both present and DIFFERENT in perspective (the
// away side's line is the home side's, inverted), which is the distinction
// migrations/00003 says CLV depends on. A moneyline would exercise neither
// column.
func newMarket(t *testing.T) *market {
	t.Helper()

	s := uniqueSuffix(t)
	// Truncated to microseconds: TIMESTAMPTZ stores microsecond resolution, so
	// a nanosecond-precision Go instant would not compare equal after a round
	// trip and every assertion here would be about float rounding instead of
	// about the writer.
	base := time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC).Truncate(time.Microsecond)

	line, err := domain.NewLine(-3.5)
	if err != nil {
		t.Fatalf("build line: %v", err)
	}

	return &market{
		suffix:   s,
		sportID:  domain.SportID("sport-" + s),
		leagueID: domain.LeagueID("league-" + s),
		eventID:  domain.EventID("event-" + s),
		marketID: domain.MarketID("market-" + s),
		homeSel:  domain.SelectionID("sel-home-" + s),
		awaySel:  domain.SelectionID("sel-away-" + s),
		bookA:    domain.BookID("book-a-" + s),
		bookB:    domain.BookID("book-b-" + s),

		eventStatus:  domain.EventStatusScheduled,
		marketStatus: domain.MarketStatusOpen,
		bookBSlug:    domain.Slug("book-b-" + s),
		marketLine:   line,
		marketObsAt:  base,
		ingestedAt:   base.Add(2 * time.Second),
		quoteObsAt:   base,
		homeDecimal:  1.91,
		awayDecimal:  1.95,
		homeLine:     line,
		awayLine:     line.Invert(),
	}
}

// payload renders the current state of the builder as a wire record.
//
// The type is the PRODUCER's, normalizer.NormalizedMarket, aliased as
// writer.Record. That is the whole point: this harness used to build a
// writer-owned struct set that described the same JSON in different words, and
// every test here passed while the live pipeline rejected 100% of real records.
// Building the producer's type means a rename on either side is a compile error
// in this file.
func (m *market) payload() writer.Record {
	return writer.Record{
		SchemaVersion: normalizer.SchemaVersion,
		Provider:      "synthetic",
		Sport: normalizer.SportRef{
			ID:   string(m.sportID),
			Slug: "basketball-" + m.suffix,
			Name: "Basketball",
		},
		League: normalizer.LeagueRef{
			ID:      string(m.leagueID),
			SportID: string(m.sportID),
			Slug:    "nba-" + m.suffix,
			Name:    "NBA",
		},
		Event: normalizer.EventRef{
			ID:             string(m.eventID),
			LeagueID:       string(m.leagueID),
			Kind:           domain.EventKindMatch.String(),
			Name:           "Away at Home " + m.suffix,
			Home:           normalizer.CompetitorRef{ID: "home-" + m.suffix, Name: "Home Team"},
			Away:           normalizer.CompetitorRef{ID: "away-" + m.suffix, Name: "Away Team"},
			ScheduledStart: m.eventObsAt().Add(3 * time.Hour),
			Status:         m.eventStatus.String(),
		},
		Market: normalizer.MarketRef{
			ID:          string(m.marketID),
			EventID:     string(m.eventID),
			Type:        domain.MarketTypeSpread.String(),
			ProviderKey: "spreads",
			Line:        m.marketLine,
			Status:      m.marketStatus.String(),
			UpdatedAt:   m.marketObsAt,
		},
		Selections: []normalizer.SelectionRef{
			{ID: string(m.homeSel), MarketID: string(m.marketID), Role: domain.SelectionRoleHome.String(), Name: "Home Team"},
			{ID: string(m.awaySel), MarketID: string(m.marketID), Role: domain.SelectionRoleAway.String(), Name: "Away Team"},
		},
		Books: []normalizer.BookRef{
			{ID: string(m.bookA), Slug: "book-a-" + m.suffix, Name: "Book A", Kind: domain.BookKindExternal.String()},
			{ID: string(m.bookB), Slug: string(m.bookBSlug), Name: "Book B", Kind: domain.BookKindSynthetic.String()},
		},
		Prices: []normalizer.PriceRef{
			{SelectionID: string(m.homeSel), BookID: string(m.bookA), Decimal: m.homeDecimal, Line: m.homeLine, ObservedAt: m.quoteObsAt},
			{SelectionID: string(m.awaySel), BookID: string(m.bookA), Decimal: m.awayDecimal, Line: m.awayLine, ObservedAt: m.quoteObsAt},
			{SelectionID: string(m.homeSel), BookID: string(m.bookB), Decimal: m.homeDecimal + 0.02, Line: m.homeLine, ObservedAt: m.quoteObsAt},
			{SelectionID: string(m.awaySel), BookID: string(m.bookB), Decimal: m.awayDecimal - 0.02, Line: m.awayLine, ObservedAt: m.quoteObsAt},
		},
		ObservedAt: m.quoteObsAt,
		IngestedAt: m.ingestedAt,
	}
}
