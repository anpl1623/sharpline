package pgstore_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
)

// Fixtures for the pgstore integration tier.
//
// Every row is written BY a test FOR that test, in an id and time namespace it
// owns, so the tests are order-independent and safe under t.Parallel() against
// one shared server. Nothing here is seeded data and nothing stands in for the
// ingest pipeline.
//
// Ids go through the DOMAIN'S OWN CONSTRUCTORS rather than being cast: the
// constructors are the bound the schema's charset CHECKs mirror, so a fixture
// the domain would reject fails with the domain's message instead of as a
// constraint violation forty lines later.

var fixtureSeq atomic.Uint64

func token() string {
	return fmt.Sprintf("%d", fixtureSeq.Add(1))
}

// fixture is one test's private slice of the catalogue.
type fixture struct {
	prefix string

	sportID  domain.SportID
	leagueID domain.LeagueID
	bookID   domain.BookID

	// window is this test's private time window. Tests that assert on chunk
	// boundaries or on ordering must not share one, or a row from another test
	// lands inside the range being asserted on.
	window time.Time
}

func seedCatalogue(t *testing.T, ctx context.Context, db *postgres.DB) fixture {
	t.Helper()

	tok := token()
	f := fixture{
		prefix: "t" + tok,
		// Each test gets its own hour, far enough in the future that no other
		// test's window overlaps and that the events are genuinely "upcoming".
		window: time.Now().UTC().Add(time.Duration(fixtureSeq.Load()) * 24 * time.Hour).
			Truncate(time.Microsecond),
	}

	f.sportID = mustSportID(t, "sport_"+tok)
	f.leagueID = mustLeagueID(t, "league_"+tok)
	f.bookID = mustBookID(t, "book_"+tok)

	exec(t, ctx, db, `INSERT INTO sports (id, slug, name) VALUES ($1, $2, $3)`,
		f.sportID, mustSlug(t, "sport-"+tok), "Sport "+tok)

	exec(t, ctx, db, `INSERT INTO leagues (id, sport_id, slug, name) VALUES ($1, $2, $3, $4)`,
		f.leagueID, f.sportID, mustSlug(t, "league-"+tok), "League "+tok)

	exec(t, ctx, db,
		`INSERT INTO books (id, slug, name, kind, is_reference) VALUES ($1, $2, $3, 'external', FALSE)`,
		f.bookID, mustSlug(t, "book-"+tok), "Book "+tok)

	return f
}

// insertEvent writes a 'match' event with generic competitor names.
//
// A match event needs BOTH competitor names (events_competitors_match_kind) and
// must carry no clock outside live/suspended (events_clock_only_in_play), so
// this is the minimal insertable shape.
func insertEvent(t *testing.T, ctx context.Context, db *postgres.DB, f fixture, id string, start time.Time, status string) domain.EventID {
	t.Helper()
	return insertNamedEvent(t, ctx, db, f, id, start, status, "Home "+id, "Away "+id)
}

func insertNamedEvent(t *testing.T, ctx context.Context, db *postgres.DB, f fixture,
	id string, start time.Time, status, home, away string,
) domain.EventID {
	t.Helper()

	eid := mustEventID(t, id)
	exec(t, ctx, db, `
INSERT INTO events (id, league_id, kind, name,
                    home_competitor_name, away_competitor_name,
                    scheduled_start, status, observed_at)
VALUES ($1, $2, 'match', $3, $4, $5, $6, $7, $8)`,
		eid, f.leagueID, home+" at "+away, home, away,
		start.Truncate(time.Microsecond), status, time.Now().UTC())
	return eid
}

// insertMarket writes a market. `line` must be nil for moneyline and non-nil for
// spread; markets_line_rule enforces exactly that.
func insertMarket(t *testing.T, ctx context.Context, db *postgres.DB, f fixture,
	event domain.EventID, marketType string, line *float64,
) domain.MarketID {
	t.Helper()

	id := mustMarketID(t, fmt.Sprintf("mkt_%s_%s", f.prefix, token()))
	exec(t, ctx, db, `
INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
VALUES ($1, $2, $3, $4, NULL, 'open', $5)`,
		id, event, marketType, line, time.Now().UTC())
	return id
}

func insertSelection(t *testing.T, ctx context.Context, db *postgres.DB,
	market domain.MarketID, id, role string,
) domain.SelectionID {
	t.Helper()

	var marketType string
	if err := db.Pool().QueryRow(ctx, `SELECT type FROM markets WHERE id = $1`, market).Scan(&marketType); err != nil {
		t.Fatalf("read market type: %v", err)
	}

	sid := mustSelectionID(t, id)
	exec(t, ctx, db, `
INSERT INTO selections (id, market_id, market_type, role, name)
VALUES ($1, $2, $3, $4, $5)`, sid, market, marketType, role, "Selection "+id)
	return sid
}

// insertPrice writes one quote into the hypertable.
//
// observed_at is the PROVIDER's instant and ingested_at is when this system
// received it; they are given separately because their difference is the
// provider-attributable share of the staleness SLO and a fixture that collapsed
// them would make that difference untestable.
func insertPrice(t *testing.T, ctx context.Context, db *postgres.DB,
	selection domain.SelectionID, book domain.BookID, odds float64, line *float64, observed time.Time,
) {
	t.Helper()
	exec(t, ctx, db, `
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
		selection, book, odds, line,
		observed.Truncate(time.Microsecond), observed.Add(time.Second).Truncate(time.Microsecond))
}

// insertUser writes a user row.
//
// THE PASSWORD HASH IS A SHAPE, NOT A SECRET. users_password_hash_is_argon2id
// requires the value to start `$argon2id$` and users_password_hash_length
// requires 40..512 characters, so the column cannot hold a placeholder. This is
// a structurally valid argon2id encoding of nothing: it hashes no password, is
// never verified, and exists only so the row is insertable.
func insertUser(t *testing.T, ctx context.Context, db *postgres.DB) domain.UserID {
	t.Helper()

	tok := token()
	id := mustUserID(t, "usr_"+tok)
	email := fmt.Sprintf("store-%s@sharpline.invalid", tok)
	const hash = "$argon2id$v=19$m=65536,t=3,p=4$c2hhcnBsaW5lLXBnc3RvcmU$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	exec(t, ctx, db, `
INSERT INTO users (id, email, password_hash, password_changed_at, status)
VALUES ($1, $2, $3, $4, 'active')`, id, email, hash, time.Now().UTC())
	return id
}

func exec(t *testing.T, ctx context.Context, db *postgres.DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Pool().Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %.60s...: %v", sql, err)
	}
}

// -----------------------------------------------------------------------------
// Typed constructors
// -----------------------------------------------------------------------------

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

func mustSlug(t *testing.T, s string) domain.Slug {
	t.Helper()
	v, err := domain.NewSlug(s)
	if err != nil {
		t.Fatalf("NewSlug(%q): %v", s, err)
	}
	return v
}

func money(t *testing.T, minor int64) domain.Money {
	t.Helper()
	v, err := domain.FromMinorUnits(minor)
	if err != nil {
		t.Fatalf("FromMinorUnits(%d): %v", minor, err)
	}
	return v
}
