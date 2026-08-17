package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Test-owned fixtures.
//
// EVERY ROW IN THIS FILE IS CREATED BY A TEST, FOR THAT TEST, AND ASSERTED ON BY
// THAT TEST. Nothing here is seeded into a real database, shipped, read by
// application code, or standing in for ingested data. Ids and slugs come from
// uniqueID/uniqueSlug so two tests can never see each other's rows, which is what
// makes the suite order-independent and safe under t.Parallel().
//
// The shapes are dictated by the schema's own CHECK constraints, not invented:
// events.kind = 'match' requires both competitor names; a spread market requires a
// line and a total requires a positive one; users.password_hash must match
// `$argon2id$%` and be 40..512 characters. Where a value looks arbitrary it is
// there because a constraint demands something in that position, and the comment
// says which constraint.

// execer is the one pgx method every fixture needs. Declared here, at the
// consumer, and kept to a single method (CLAUDE.md §12: "Interfaces are declared
// by the consumer, not the producer. Keep them small.").
//
// Both *pgx.Conn and pgx.Tx satisfy it, which is the point: a fixture can be built
// on a plain connection, or inside a transaction the test controls — which is how
// the ledger tests get several statements to arrive under one COMMIT.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// catalogue is one complete, self-contained slice of the catalogue: a sport, a
// league under it, a match event in that league, and a book.
type catalogue struct {
	SportID   domain.SportID
	SportSlug domain.Slug
	SportName string

	LeagueID   domain.LeagueID
	LeagueSlug domain.Slug
	LeagueName string

	EventID   domain.EventID
	EventName string
	HomeName  string
	AwayName  string
	Start     time.Time

	BookID   domain.BookID
	BookSlug domain.Slug
	BookName string
}

// newCatalogue inserts sport -> league -> event -> book and returns their ids.
//
// The event is 'match' with status 'scheduled', which is the only combination that
// satisfies events_competitors_match_kind (both competitor names present) and
// events_clock_only_in_play (no clock outside live/suspended) without any extra
// columns. It is also, deliberately, a status inside the partial index predicate
// `status IN ('scheduled','live','suspended')`, so the board queries can find it.
func newCatalogue(t *testing.T, ctx context.Context, x execer) catalogue {
	t.Helper()

	c := catalogue{
		SportID:   sportID(t, uniqueID("sport")),
		SportSlug: slug(t, uniqueSlug("sport")),
		SportName: "Integration Sport " + nextToken(),

		LeagueID:   leagueID(t, uniqueID("league")),
		LeagueSlug: slug(t, uniqueSlug("league")),
		LeagueName: "Integration League " + nextToken(),

		EventID:  eventID(t, uniqueID("event")),
		HomeName: "Home " + nextToken(),
		AwayName: "Away " + nextToken(),
		Start:    time.Now().UTC().Add(3 * time.Hour).Truncate(time.Microsecond),

		BookID:   bookID(t, uniqueID("book")),
		BookSlug: slug(t, uniqueSlug("book")),
		BookName: "Integration Book " + nextToken(),
	}
	c.EventName = c.HomeName + " at " + c.AwayName

	mustExec(t, ctx, x,
		`INSERT INTO sports (id, slug, name) VALUES ($1, $2, $3)`,
		c.SportID, c.SportSlug, c.SportName)

	mustExec(t, ctx, x,
		`INSERT INTO leagues (id, sport_id, slug, name) VALUES ($1, $2, $3, $4)`,
		c.LeagueID, c.SportID, c.LeagueSlug, c.LeagueName)

	mustExec(t, ctx, x, `
INSERT INTO events (id, league_id, kind, name,
                    home_competitor_name, away_competitor_name,
                    scheduled_start, status, observed_at)
VALUES ($1, $2, 'match', $3, $4, $5, $6, 'scheduled', $7)`,
		c.EventID, c.LeagueID, c.EventName, c.HomeName, c.AwayName, c.Start, time.Now().UTC())

	mustExec(t, ctx, x,
		`INSERT INTO books (id, slug, name, kind, is_reference) VALUES ($1, $2, $3, 'external', FALSE)`,
		c.BookID, c.BookSlug, c.BookName)

	return c
}

// market is a market plus the two selections under it.
type market struct {
	ID   domain.MarketID
	Type string
	Line any // float64 for a lined market, nil for moneyline/futures

	HomeSelection domain.SelectionID
	AwaySelection domain.SelectionID
	HomeRole      string
	AwayRole      string
}

// newMoneylineMarket inserts a moneyline market with home and away selections.
//
// moneyline is the market type with the fewest constraints on it: markets_line_rule
// requires line IS NULL, markets_subject_matches_type requires subject IS NULL, and
// selections_role_allowed permits home/away/draw. Where a test does not care about
// the market type, this is the one it should use.
func newMoneylineMarket(t *testing.T, ctx context.Context, x execer, c catalogue) market {
	t.Helper()

	m := market{
		ID:            marketID(t, uniqueID("market")),
		Type:          domain.MarketTypeMoneyline.String(),
		HomeSelection: selectionID(t, uniqueID("sel")),
		AwaySelection: selectionID(t, uniqueID("sel")),
		HomeRole:      domain.SelectionRoleHome.String(),
		AwayRole:      domain.SelectionRoleAway.String(),
	}

	mustExec(t, ctx, x, `
INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
VALUES ($1, $2, $3, NULL, NULL, 'open', $4)`,
		m.ID, c.EventID, m.Type, time.Now().UTC())

	for _, s := range []struct {
		id   domain.SelectionID
		role string
		name string
	}{
		{m.HomeSelection, m.HomeRole, c.HomeName},
		{m.AwaySelection, m.AwayRole, c.AwayName},
	} {
		mustExec(t, ctx, x, `
INSERT INTO selections (id, market_id, market_type, role, name)
VALUES ($1, $2, $3, $4, $5)`, s.id, m.ID, m.Type, s.role, s.name)
	}

	return m
}

// newSpreadMarket inserts a spread market at the given line, with home and away
// selections. Needed by the teaser fixture, because legs_teasable_market_type
// restricts a teased leg to a spread or a total.
func newSpreadMarket(t *testing.T, ctx context.Context, x execer, c catalogue, line float64) market {
	t.Helper()

	m := market{
		ID:            marketID(t, uniqueID("market")),
		Type:          domain.MarketTypeSpread.String(),
		Line:          line,
		HomeSelection: selectionID(t, uniqueID("sel")),
		AwaySelection: selectionID(t, uniqueID("sel")),
		HomeRole:      domain.SelectionRoleHome.String(),
		AwayRole:      domain.SelectionRoleAway.String(),
	}

	mustExec(t, ctx, x, `
INSERT INTO markets (id, event_id, type, line, subject, status, observed_at)
VALUES ($1, $2, $3, $4, NULL, 'open', $5)`,
		m.ID, c.EventID, m.Type, line, time.Now().UTC())

	for _, s := range []struct {
		id   domain.SelectionID
		role string
		name string
	}{
		{m.HomeSelection, m.HomeRole, c.HomeName},
		{m.AwaySelection, m.AwayRole, c.AwayName},
	} {
		mustExec(t, ctx, x, `
INSERT INTO selections (id, market_id, market_type, role, name)
VALUES ($1, $2, $3, $4, $5)`, s.id, m.ID, m.Type, s.role, s.name)
	}

	return m
}

// newUser inserts a user and returns its id.
//
// THE PASSWORD HASH IS A SHAPE, NOT A SECRET. users_password_hash_is_argon2id
// requires the value to be LIKE '$argon2id$%' and users_password_hash_length
// requires 40..512 characters, so the column cannot hold a placeholder. This is a
// structurally valid argon2id encoding of nothing — it hashes no password, is
// never verified, and exists only so the row is insertable. Real hashes are phase
// 5's business.
func newUser(t *testing.T, ctx context.Context, x execer) domain.UserID {
	t.Helper()

	id := userID(t, uniqueID("user"))
	// users_email_normalised requires lower(email) = email, and
	// users_email_shape requires exactly one @ with no whitespace.
	email := fmt.Sprintf("it-%s@sharpline.invalid", nextToken())
	const hash = "$argon2id$v=19$m=65536,t=3,p=4$c2hhcnBsaW5lLWludGVncmF0aW9u$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	mustExec(t, ctx, x, `
INSERT INTO users (id, email, password_hash, password_changed_at, status)
VALUES ($1, $2, $3, $4, 'active')`, id, email, hash, time.Now().UTC())

	return id
}

// -----------------------------------------------------------------------------
// Id constructors
// -----------------------------------------------------------------------------
//
// Every fixture id goes through the domain's own constructor rather than being
// cast. Two reasons: the constructors are the bound the schema's charset CHECKs
// mirror, so a fixture that the domain would reject fails here with the domain's
// message instead of as a constraint violation forty lines later; and it keeps the
// test honest about using the typed ids the access layer returns.

func sportID(t *testing.T, s string) domain.SportID {
	t.Helper()
	v, err := domain.NewSportID(s)
	if err != nil {
		t.Fatalf("NewSportID(%q): %v", s, err)
	}
	return v
}

func leagueID(t *testing.T, s string) domain.LeagueID {
	t.Helper()
	v, err := domain.NewLeagueID(s)
	if err != nil {
		t.Fatalf("NewLeagueID(%q): %v", s, err)
	}
	return v
}

func eventID(t *testing.T, s string) domain.EventID {
	t.Helper()
	v, err := domain.NewEventID(s)
	if err != nil {
		t.Fatalf("NewEventID(%q): %v", s, err)
	}
	return v
}

func marketID(t *testing.T, s string) domain.MarketID {
	t.Helper()
	v, err := domain.NewMarketID(s)
	if err != nil {
		t.Fatalf("NewMarketID(%q): %v", s, err)
	}
	return v
}

func selectionID(t *testing.T, s string) domain.SelectionID {
	t.Helper()
	v, err := domain.NewSelectionID(s)
	if err != nil {
		t.Fatalf("NewSelectionID(%q): %v", s, err)
	}
	return v
}

func bookID(t *testing.T, s string) domain.BookID {
	t.Helper()
	v, err := domain.NewBookID(s)
	if err != nil {
		t.Fatalf("NewBookID(%q): %v", s, err)
	}
	return v
}

func slug(t *testing.T, s string) domain.Slug {
	t.Helper()
	v, err := domain.NewSlug(s)
	if err != nil {
		t.Fatalf("NewSlug(%q): %v", s, err)
	}
	return v
}

func userID(t *testing.T, s string) domain.UserID {
	t.Helper()
	v, err := domain.NewUserID(s)
	if err != nil {
		t.Fatalf("NewUserID(%q): %v", s, err)
	}
	return v
}

func wagerID(t *testing.T, s string) domain.WagerID {
	t.Helper()
	v, err := domain.NewWagerID(s)
	if err != nil {
		t.Fatalf("NewWagerID(%q): %v", s, err)
	}
	return v
}

func legID(t *testing.T, s string) domain.LegID {
	t.Helper()
	v, err := domain.NewLegID(s)
	if err != nil {
		t.Fatalf("NewLegID(%q): %v", s, err)
	}
	return v
}

func transactionID(t *testing.T, s string) domain.TransactionID {
	t.Helper()
	v, err := domain.NewTransactionID(s)
	if err != nil {
		t.Fatalf("NewTransactionID(%q): %v", s, err)
	}
	return v
}

func roundRobinID(t *testing.T, s string) domain.RoundRobinID {
	t.Helper()
	v, err := domain.NewRoundRobinID(s)
	if err != nil {
		t.Fatalf("NewRoundRobinID(%q): %v", s, err)
	}
	return v
}

// -----------------------------------------------------------------------------
// Exec helper
// -----------------------------------------------------------------------------

func mustExec(t *testing.T, ctx context.Context, x execer, sql string, args ...any) {
	t.Helper()
	if _, err := x.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec failed: %v\nSQL: %s", err, sql)
	}
}
