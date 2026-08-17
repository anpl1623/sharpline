package domain

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"math"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Shared float tolerance
// ---------------------------------------------------------------------------

// floatTolerance is the RELATIVE tolerance every float comparison in this
// package's tests uses. Exact == on a float64 is never used, per the phase
// brief.
//
// Justification for the magnitude. One ULP of a float64 is ~2.2e-16 relative
// (2^-52), so 1e-12 relative is roughly 4,500 ULPs: loose enough to absorb the
// handful of rounding steps any conversion chain in this domain performs, and
// still ten orders of magnitude tighter than the smallest difference the domain
// can express. The finest increments here are a quarter-point line (0.25) and a
// one-cent odds tick (0.01 on a decimal price near 2.0, i.e. ~5e-3 relative);
// a discrepancy the domain cares about is at least 1e9 times this tolerance, so
// the test cannot pass a genuinely wrong answer.
//
// Relative rather than absolute, because the values under test span five orders
// of magnitude — a total line of 249.5 and a futures price of 1001.0 sit beside
// a spread of -3.5 — and one ULP at 100000 is ~1.5e-11, already larger than any
// absolute epsilon small enough to be meaningful near 1.0.
const floatTolerance = 1e-12

// approxEqual reports whether a and b agree to within a relative tolerance.
func approxEqual(a, b, tol float64) bool {
	if a == b { // Exact hit, and the only branch that admits infinities.
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
		return false
	}
	diff := math.Abs(a - b)
	scale := math.Max(math.Abs(a), math.Abs(b))
	if scale == 0 {
		return diff <= tol
	}
	return diff/scale <= tol
}

// assertApprox fails the test unless got and want agree within floatTolerance.
func assertApprox(t *testing.T, what string, got, want float64) {
	t.Helper()
	if !approxEqual(got, want, floatTolerance) {
		t.Errorf("%s = %v, want %v (relative tolerance %g)", what, got, want, floatTolerance)
	}
}

// mustLine builds a present Line or fails the test.
func mustLine(t *testing.T, v float64) Line {
	t.Helper()
	l, err := NewLine(v)
	if err != nil {
		t.Fatalf("NewLine(%v): %v", v, err)
	}
	return l
}

// ts is a fixed instant used across the tests. Nothing in this package reads a
// clock, so every temporal assertion is against a literal.
func ts(offset time.Duration) time.Time {
	base := time.Date(2026, time.August, 16, 18, 0, 0, 0, time.UTC)
	return base.Add(offset)
}

// ---------------------------------------------------------------------------
// The dependency guard
// ---------------------------------------------------------------------------

// TestPackageHasNoExternalDependencies parses every Go file in this directory
// and asserts that no import reaches outside the standard library.
//
// CLAUDE.md §8 annotates internal/domain as "types + odds math — zero external
// deps", and that is the kind of constraint that holds until the first
// convenient helper is added and is then never true again. Asserting it
// mechanically means a future import of a validation library, a decimal
// library, or a logger fails a test rather than passing review.
//
// The stdlib test is that the first path element carries no dot: every module
// path outside the standard library begins with a hostname.
func TestPackageHasNoExternalDependencies(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package directory: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("parsed no packages; the guard would pass vacuously")
	}

	checked := 0
	for _, pkg := range pkgs {
		for filename, file := range pkg.Files {
			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				first := path
				if i := strings.Index(path, "/"); i >= 0 {
					first = path[:i]
				}
				if strings.Contains(first, ".") {
					t.Errorf("%s imports %q, which is outside the standard library", filename, path)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("inspected no imports; the guard would pass vacuously")
	}
	t.Logf("checked %d imports across %d package(s), all stdlib", checked, len(pkgs))
}

// TestNoConstructorProducesAZeroValue asserts the invariant the whole
// unexported-fields design rests on: a value that came out of a constructor is
// never mistakable for a zero value, so IsZero is a reliable "unset" test
// everywhere downstream.
func TestNoConstructorProducesAZeroValue(t *testing.T) {
	sport, league, event, market, selection, price, book := buildHierarchy(t)

	zeroChecks := []struct {
		name   string
		isZero bool
	}{
		{"Sport", sport.IsZero()},
		{"League", league.IsZero()},
		{"Event", event.IsZero()},
		{"Market", market.IsZero()},
		{"Selection", selection.IsZero()},
		{"Price", price.IsZero()},
		{"Book", book.IsZero()},
	}
	for _, c := range zeroChecks {
		if c.isZero {
			t.Errorf("a constructed %s reports IsZero", c.name)
		}
	}

	if !(Sport{}).IsZero() || !(League{}).IsZero() || !(Event{}).IsZero() ||
		!(Market{}).IsZero() || !(Selection{}).IsZero() || !(Price{}).IsZero() || !(Book{}).IsZero() {
		t.Error("a zero-valued entity does not report IsZero")
	}
}

// buildHierarchy constructs one instance of every entity, wired parent to
// child, and is the fixture several tests share.
//
// The data is real in the sense the ledger's NO MOCK DATA rule requires of a
// test fixture: the NBA and the Boston Celtics exist, -3.5 is a real spread
// increment, and 1.909090… is the decimal form of -110, the standard US juice
// price. No assertion here claims to reproduce a specific historical quote from
// a specific book on a specific date — that claim would need a recorded
// provider payload, which is what the golden files in the ingest phase are for.
func buildHierarchy(t *testing.T) (Sport, League, Event, Market, Selection, Price, Book) {
	t.Helper()

	sport, err := NewSport(SportParams{ID: "sport-basketball", Slug: "basketball", Name: "Basketball"})
	if err != nil {
		t.Fatalf("NewSport: %v", err)
	}
	league, err := NewLeague(LeagueParams{
		ID: "league-nba", SportID: sport.ID(), Slug: "nba", Name: "NBA",
	})
	if err != nil {
		t.Fatalf("NewLeague: %v", err)
	}
	home, err := NewCompetitor("team-bos", "Boston Celtics")
	if err != nil {
		t.Fatalf("NewCompetitor home: %v", err)
	}
	away, err := NewCompetitor("team-lal", "Los Angeles Lakers")
	if err != nil {
		t.Fatalf("NewCompetitor away: %v", err)
	}
	event, err := NewEvent(EventParams{
		ID: "evt-1", LeagueID: league.ID(), Kind: EventKindMatch,
		Name: "Los Angeles Lakers at Boston Celtics", Home: home, Away: away,
		ScheduledStart: ts(2 * time.Hour), Status: EventStatusScheduled, UpdatedAt: ts(0),
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	market, err := NewMarket(MarketParams{
		ID: "mkt-1", EventID: event.ID(), Type: MarketTypeSpread,
		Line: mustLine(t, -3.5), Status: MarketStatusOpen, UpdatedAt: ts(0),
	})
	if err != nil {
		t.Fatalf("NewMarket: %v", err)
	}
	selection, err := NewSelection(SelectionParams{
		ID: "sel-1", MarketID: market.ID(), Role: SelectionRoleHome, Name: "Boston Celtics",
	})
	if err != nil {
		t.Fatalf("NewSelection: %v", err)
	}
	book, err := NewBook(BookParams{
		ID: "book-1", Slug: "reference", Name: "Reference Book",
		Kind: BookKindExternal, Reference: true,
	})
	if err != nil {
		t.Fatalf("NewBook: %v", err)
	}
	price, err := NewPrice(PriceParams{
		SelectionID: selection.ID(), BookID: book.ID(),
		Decimal: 1 + 100.0/110.0, Line: market.Line(), ObservedAt: ts(time.Minute),
	})
	if err != nil {
		t.Fatalf("NewPrice: %v", err)
	}
	return sport, league, event, market, selection, price, book
}

// TestHierarchyLinksParentToChild walks Sport → League → Event → Market →
// Selection → Price and asserts every parent link resolves, which is the whole
// point of the distinct identifier types.
func TestHierarchyLinksParentToChild(t *testing.T) {
	sport, league, event, market, selection, price, book := buildHierarchy(t)

	if !league.BelongsTo(sport) {
		t.Error("league does not belong to its sport")
	}
	if event.LeagueID() != league.ID() {
		t.Errorf("event league = %s, want %s", event.LeagueID(), league.ID())
	}
	if !market.BelongsTo(event) {
		t.Error("market does not belong to its event")
	}
	if !selection.BelongsTo(market) {
		t.Error("selection does not belong to its market")
	}
	if !book.Quoted(price) {
		t.Error("book does not recognise its own price")
	}
	if err := ValidatePriceForSelection(market, selection, price); err != nil {
		t.Errorf("ValidatePriceForSelection on a consistent triple: %v", err)
	}

	// The price is the home side of a -3.5 spread, so it trades at -3.5.
	got, ok := price.Line().Value()
	if !ok {
		t.Fatal("price line is absent on a spread")
	}
	assertApprox(t, "price line", got, -3.5)
}

// TestFullLifecycleFromScheduledToSettled walks an event and its market through
// the lifecycle the settle service will drive, asserting that each step is
// legal and that the terminal state is reached.
func TestFullLifecycleFromScheduledToSettled(t *testing.T) {
	_, _, event, market, _, _, _ := buildHierarchy(t)

	steps := []struct {
		at     time.Duration
		status EventStatus
	}{
		{2 * time.Hour, EventStatusLive},
		{3 * time.Hour, EventStatusSuspended},
		{3*time.Hour + 10*time.Minute, EventStatusLive},
		{4 * time.Hour, EventStatusEnded},
		{4*time.Hour + time.Minute, EventStatusSettled},
	}
	for _, s := range steps {
		next, err := event.WithStatus(s.status, ts(s.at))
		if err != nil {
			t.Fatalf("event → %s: %v", s.status, err)
		}
		event = next
	}
	if !event.IsTerminal() {
		t.Errorf("event status = %s, want a terminal status", event.Status())
	}

	for _, s := range []MarketStatus{MarketStatusSuspended, MarketStatusOpen, MarketStatusClosed, MarketStatusSettled} {
		next, err := market.WithStatus(s, ts(5*time.Hour))
		if err != nil {
			t.Fatalf("market → %s: %v", s, err)
		}
		market = next
	}
	if !market.Status().IsTerminal() {
		t.Errorf("market status = %s, want a terminal status", market.Status())
	}
	if market.AcceptsWagers() {
		t.Error("a settled market still accepts wagers")
	}
}

// TestEveryErrorReachesARoot asserts the error taxonomy holds: the two roots
// are distinct, and every error this package returns wraps exactly one of them
// so that an HTTP layer can map a failure to 400 or 409 without a type switch.
func TestEveryErrorReachesARoot(t *testing.T) {
	if errors.Is(ErrInvalid, ErrConflict) || errors.Is(ErrConflict, ErrInvalid) {
		t.Fatal("the two error roots are not distinct")
	}

	invalid := []error{
		ErrEmptyID, ErrIDTooLong, ErrIDCharset,
		ErrEmptySlug, ErrSlugTooLong, ErrSlugCharset,
		ErrEmptyName, ErrNameTooLong, ErrNameCharset,
		ErrMoneyOverflow, ErrMoneyPrecision, ErrMoneySyntax, ErrMoneyNotFinite,
		ErrMoneyDivideByZero, ErrUnknownRounding,
		ErrZeroTime,
		ErrUnknownEventKind, ErrUnknownEventStatus, ErrUnknownMarketType,
		ErrUnknownMarketStatus, ErrUnknownSelectionRole, ErrUnknownBookKind,
		ErrCompetitorsRequired, ErrCompetitorsNotApplicable, ErrNegativeScore,
		ErrInvalidPeriod, ErrInvalidElapsed,
		ErrLineRequired, ErrLineNotApplicable, ErrLineNotFinite,
		ErrLineNotPositive, ErrLineSyntax,
		ErrSubjectRequired, ErrSubjectNotApplicable,
		ErrRoleNotApplicable, ErrMismatchedParent,
		ErrOddsNotFinite, ErrOddsOutOfRange,
	}
	for _, err := range invalid {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%v does not wrap ErrInvalid", err)
		}
		if errors.Is(err, ErrConflict) {
			t.Errorf("%v wraps both roots", err)
		}
	}

	conflict := []error{
		ErrStaleUpdate, ErrIllegalTransition,
		ErrClockNotInPlay, ErrScoreNotApplicable, ErrLineMismatch,
	}
	for _, err := range conflict {
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%v does not wrap ErrConflict", err)
		}
		if errors.Is(err, ErrInvalid) {
			t.Errorf("%v wraps both roots", err)
		}
	}
}

// TestContextualErrorsPreserveTheirSentinel checks that the fmt.Errorf wrapping
// every constructor does keeps errors.Is working all the way down to the root.
func TestContextualErrorsPreserveTheirSentinel(t *testing.T) {
	_, err := NewEventID("has:colon")
	if err == nil {
		t.Fatal("NewEventID accepted a colon")
	}
	for _, target := range []error{ErrIDCharset, ErrInvalid} {
		if !errors.Is(err, target) {
			t.Errorf("errors.Is(%v, %v) = false", err, target)
		}
	}
	if !strings.Contains(err.Error(), "event id") {
		t.Errorf("error %q does not name the field it came from", err)
	}
}

func ExampleLine() {
	absent := NoLine()
	pickem, _ := NewLine(0)
	home, _ := NewLine(-3.5)

	fmt.Println(absent, pickem.SignedString(), home.SignedString(), home.Invert().SignedString())
	fmt.Println(absent.Present(), pickem.Present())
	// Output:
	// none 0 -3.5 +3.5
	// false true
}
