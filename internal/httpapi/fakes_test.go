package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/httpapi/middleware"
)

// In-memory doubles for every port, so the handler layer is testable without a
// database, a Redis or a signing key.
//
// These are FAKES, NOT MOCKS: they hold real data in maps and answer real
// queries against it, so a test asserts on behaviour rather than on which method
// was called. They exist ONLY here, in _test.go files. No handler has a fallback
// that would serve canned data at runtime — CLAUDE.md's "no mock data" rule is
// about what the SERVICE returns, and every value a running api returns comes
// from Postgres or Redis.

// testNow is the pinned clock. Every test that asserts on `as_of` or on a
// cooling-off instant compares against this, so no test depends on wall time.
var testNow = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func fixedClock() Clock { return func() time.Time { return testNow } }

// discardLogger keeps test output readable and, more usefully, is where a test
// can swap in a capturing handler to assert that a secret NEVER appears in a log
// line (see TestNoSecretReachesTheLog).
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// -----------------------------------------------------------------------------
// Catalogue
// -----------------------------------------------------------------------------

type fakeCatalogue struct {
	sports     []Sport
	leagues    []League
	books      []Book
	events     []Event
	markets    []Market
	selections []Selection

	err error
}

func (f *fakeCatalogue) Sports(context.Context) ([]Sport, error) { return f.sports, f.err }

func (f *fakeCatalogue) LeaguesInSport(_ context.Context, id domain.SportID) ([]League, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []League
	for _, l := range f.leagues {
		if l.SportID == id {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *fakeCatalogue) LeagueBySlug(_ context.Context, slug domain.Slug) (League, error) {
	if f.err != nil {
		return League{}, f.err
	}
	for _, l := range f.leagues {
		if l.Slug == slug {
			return l, nil
		}
	}
	return League{}, ErrNotFound
}

func (f *fakeCatalogue) Books(context.Context) ([]Book, error) { return f.books, f.err }

func (f *fakeCatalogue) EventWithBreadcrumb(_ context.Context, id domain.EventID) (Event, League, Sport, error) {
	if f.err != nil {
		return Event{}, League{}, Sport{}, f.err
	}
	for _, e := range f.events {
		if e.ID != id {
			continue
		}
		for _, l := range f.leagues {
			if l.ID != e.LeagueID {
				continue
			}
			for _, s := range f.sports {
				if s.ID == l.SportID {
					return e, l, s, nil
				}
			}
		}
		return Event{}, League{}, Sport{}, ErrNotFound
	}
	return Event{}, League{}, Sport{}, ErrNotFound
}

func (f *fakeCatalogue) Market(_ context.Context, id domain.MarketID) (Market, error) {
	if f.err != nil {
		return Market{}, f.err
	}
	for _, m := range f.markets {
		if m.ID == id {
			return m, nil
		}
	}
	return Market{}, ErrNotFound
}

func (f *fakeCatalogue) Selection(_ context.Context, id domain.SelectionID) (Selection, error) {
	if f.err != nil {
		return Selection{}, f.err
	}
	for _, s := range f.selections {
		if s.ID == id {
			return s, nil
		}
	}
	return Selection{}, ErrNotFound
}

func (f *fakeCatalogue) MarketsForEvents(_ context.Context, ids []domain.EventID) ([]Market, error) {
	if f.err != nil {
		return nil, f.err
	}
	want := map[domain.EventID]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []Market
	for _, m := range f.markets {
		if want[m.EventID] {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeCatalogue) SelectionsForMarkets(_ context.Context, ids []domain.MarketID) ([]Selection, error) {
	if f.err != nil {
		return nil, f.err
	}
	want := map[domain.MarketID]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []Selection
	for _, s := range f.selections {
		if want[s.MarketID] {
			out = append(out, s)
		}
	}
	return out, nil
}

// EventPage implements keyset paging over the fake's event slice with the SAME
// semantics the SQL has: filter, order by (scheduled_start, id), skip past the
// cursor, return Limit rows and report whether more exist.
//
// Reimplementing the semantics here is the point: a test can then assert that
// the pagination CONTRACT holds — no row served twice, no row skipped when the
// set grows mid-page — without a database, and the same assertions run against
// the real store in the integration tier.
func (f *fakeCatalogue) EventPage(_ context.Context, q EventPageQuery) (EventPage, error) {
	if f.err != nil {
		return EventPage{}, f.err
	}

	var rows []Event
	for _, e := range f.events {
		if !q.LeagueID.IsZero() && e.LeagueID != q.LeagueID {
			continue
		}
		if !e.ScheduledStart.Before(q.StartingBefore) {
			continue
		}
		switch e.Status {
		case domain.EventStatusScheduled, domain.EventStatusLive, domain.EventStatusSuspended:
		default:
			continue
		}
		if q.After != nil && !afterKey(e, *q.After) {
			continue
		}
		rows = append(rows, e)
	}
	sortEvents(rows)

	page := EventPage{}
	if int32(len(rows)) > q.Limit {
		page.HasMore = true
		rows = rows[:q.Limit]
	}
	page.Events = rows
	return page, nil
}

func (f *fakeCatalogue) SearchEvents(_ context.Context, q SearchQuery) (SearchPage, error) {
	if f.err != nil {
		return SearchPage{}, f.err
	}
	var rows []Event
	for _, e := range f.events {
		switch e.Status {
		case domain.EventStatusScheduled, domain.EventStatusLive:
		default:
			continue
		}
		lower := strings.ToLower(q.Prefix)
		if !strings.HasPrefix(strings.ToLower(e.HomeCompetitorName), lower) &&
			!strings.HasPrefix(strings.ToLower(e.AwayCompetitorName), lower) {
			continue
		}
		if q.After != nil && !afterKey(e, *q.After) {
			continue
		}
		rows = append(rows, e)
	}
	sortEvents(rows)

	page := SearchPage{}
	if int32(len(rows)) > q.Limit {
		page.HasMore = true
		rows = rows[:q.Limit]
	}
	page.Events = rows
	return page, nil
}

// afterKey is the Go form of `(scheduled_start, id) > (@after_start, @after_id)`.
func afterKey(e Event, k EventKey) bool {
	switch {
	case e.ScheduledStart.After(k.ScheduledStart):
		return true
	case e.ScheduledStart.Equal(k.ScheduledStart):
		return e.ID.String() > k.ID.String()
	default:
		return false
	}
}

func sortEvents(rows []Event) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, b := rows[j-1], rows[j]
			if a.ScheduledStart.Before(b.ScheduledStart) ||
				(a.ScheduledStart.Equal(b.ScheduledStart) && a.ID.String() <= b.ID.String()) {
				break
			}
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}

// -----------------------------------------------------------------------------
// Prices
// -----------------------------------------------------------------------------

type fakePrices struct {
	quotes  []Quote
	history []HistoryPoint
	err     error

	// lastSince records the freshness horizon the handler passed, so a test can
	// assert the board never asks for an unbounded window.
	lastSince time.Time
}

func (f *fakePrices) CurrentQuotes(_ context.Context, selections []domain.SelectionID, since time.Time) ([]Quote, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastSince = since
	want := map[domain.SelectionID]bool{}
	for _, id := range selections {
		want[id] = true
	}
	var out []Quote
	for _, q := range f.quotes {
		if want[q.SelectionID] && q.ObservedAt.After(since) {
			out = append(out, q)
		}
	}
	return out, nil
}

func (f *fakePrices) History(_ context.Context, _ HistoryQuery) ([]HistoryPoint, error) {
	return f.history, f.err
}

// -----------------------------------------------------------------------------
// Ledger, accounts, limits
// -----------------------------------------------------------------------------

type fakeLedger struct {
	balances []Balance
	err      error
}

func (f *fakeLedger) Balances(context.Context, domain.UserID) ([]Balance, error) {
	return f.balances, f.err
}

type fakeAccounts struct {
	profiles map[domain.UserID]Profile
	err      error
}

func (f *fakeAccounts) Profile(_ context.Context, id domain.UserID) (Profile, error) {
	if f.err != nil {
		return Profile{}, f.err
	}
	p, ok := f.profiles[id]
	if !ok {
		return Profile{}, ErrNotFound
	}
	return p, nil
}

type fakeLimits struct {
	current []Limit
	set     []SetLimit
	result  Limit
	err     error
}

func (f *fakeLimits) Current(context.Context, domain.UserID) ([]Limit, error) {
	return f.current, f.err
}

func (f *fakeLimits) Set(_ context.Context, req SetLimit) (Limit, error) {
	f.set = append(f.set, req)
	if f.err != nil {
		return Limit{}, f.err
	}
	return f.result, nil
}

// -----------------------------------------------------------------------------
// Sessions
// -----------------------------------------------------------------------------

type fakeSessions struct {
	session   Session
	enrolment Enrolment

	registerErr error
	authErr     error
	redeemErr   error
	revokeErr   error
	totpErr     error

	// seen records every credential the handlers passed down, so a test can
	// prove the handler forwarded what the client sent without the fake ever
	// putting it anywhere a log could reach.
	seenPasswords []string
	seenTokens    []string
	seenCodes     []string
}

func (f *fakeSessions) Register(_ context.Context, _, password string, _ AuditContext) (Session, error) {
	f.seenPasswords = append(f.seenPasswords, password)
	return f.session, f.registerErr
}

func (f *fakeSessions) Authenticate(_ context.Context, _, password, code string, _ AuditContext) (Session, error) {
	f.seenPasswords = append(f.seenPasswords, password)
	if code != "" {
		f.seenCodes = append(f.seenCodes, code)
	}
	return f.session, f.authErr
}

func (f *fakeSessions) Redeem(_ context.Context, token string, _ AuditContext) (Session, error) {
	f.seenTokens = append(f.seenTokens, token)
	return f.session, f.redeemErr
}

func (f *fakeSessions) Revoke(_ context.Context, token string, _ AuditContext) error {
	f.seenTokens = append(f.seenTokens, token)
	return f.revokeErr
}

func (f *fakeSessions) BeginTOTP(context.Context, domain.UserID, AuditContext) (Enrolment, error) {
	return f.enrolment, f.totpErr
}

func (f *fakeSessions) ConfirmTOTP(_ context.Context, _ domain.UserID, code string, _ AuditContext) error {
	f.seenCodes = append(f.seenCodes, code)
	return f.totpErr
}

func (f *fakeSessions) RemoveTOTP(_ context.Context, _ domain.UserID, code string, _ AuditContext) error {
	f.seenCodes = append(f.seenCodes, code)
	return f.totpErr
}

// -----------------------------------------------------------------------------
// Audit
// -----------------------------------------------------------------------------

type fakeAudit struct {
	entries []AuditEntry
	err     error
}

func (f *fakeAudit) Record(_ context.Context, e AuditEntry) error {
	f.entries = append(f.entries, e)
	return f.err
}

// -----------------------------------------------------------------------------
// Wiring
// -----------------------------------------------------------------------------

// deps bundles the fakes so a test can reach into them after a request.
type deps struct {
	catalogue *fakeCatalogue
	prices    *fakePrices
	ledger    *fakeLedger
	accounts  *fakeAccounts
	limits    *fakeLimits
	sessions  *fakeSessions
	audit     *fakeAudit
	logger    *slog.Logger
}

func newDeps() *deps {
	return &deps{
		catalogue: &fakeCatalogue{},
		prices:    &fakePrices{},
		ledger:    &fakeLedger{},
		accounts:  &fakeAccounts{profiles: map[domain.UserID]Profile{}},
		limits:    &fakeLimits{},
		sessions:  &fakeSessions{},
		audit:     &fakeAudit{},
		logger:    discardLogger(),
	}
}

func (d *deps) api(t *testing.T) *API {
	t.Helper()
	a, err := NewAPI(APIOptions{
		Catalogue: d.catalogue,
		Prices:    d.prices,
		Ledger:    d.ledger,
		Accounts:  d.accounts,
		Limits:    d.limits,
		Sessions:  d.sessions,
		Audit:     d.audit,
		Logger:    d.logger,
		Now:       fixedClock(),
		// A no-op stand-in for the real Authenticate + RequireIdentity chain.
		// The route table's shape is what this package owns; the verification
		// itself is internal/httpapi/middleware's and is tested there.
		RequireAuth: []Middleware{func(next http.Handler) http.Handler { return next }},
	})
	if err != nil {
		t.Fatalf("NewAPI: %v", err)
	}
	return a
}

func newTestAPI(t *testing.T) *API {
	t.Helper()
	return newDeps().api(t)
}

// newRequest builds a request and recorder for a direct handler call, bypassing
// the router. Path wildcards are set explicitly with SetPathValue, because
// net/http only populates them when the request actually matched a pattern.
func newRequest(method, target string, body io.Reader, pathValues ...string) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest(method, target, body)
	for i := 0; i+1 < len(pathValues); i += 2 {
		req.SetPathValue(pathValues[i], pathValues[i+1])
	}
	return httptest.NewRecorder(), req
}

func decodeJSONBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return out
}

// -----------------------------------------------------------------------------
// Fixture builders
// -----------------------------------------------------------------------------

func mustSportID(t *testing.T, s string) domain.SportID {
	t.Helper()
	id, err := domain.NewSportID(s)
	if err != nil {
		t.Fatalf("sport id %q: %v", s, err)
	}
	return id
}

func mustSlug(t *testing.T, s string) domain.Slug {
	t.Helper()
	v, err := domain.NewSlug(s)
	if err != nil {
		t.Fatalf("slug %q: %v", s, err)
	}
	return v
}

func mustEventID(t *testing.T, s string) domain.EventID {
	t.Helper()
	id, err := domain.NewEventID(s)
	if err != nil {
		t.Fatalf("event id %q: %v", s, err)
	}
	return id
}

func mustMarketID(t *testing.T, s string) domain.MarketID {
	t.Helper()
	id, err := domain.NewMarketID(s)
	if err != nil {
		t.Fatalf("market id %q: %v", s, err)
	}
	return id
}

func mustSelectionID(t *testing.T, s string) domain.SelectionID {
	t.Helper()
	id, err := domain.NewSelectionID(s)
	if err != nil {
		t.Fatalf("selection id %q: %v", s, err)
	}
	return id
}

func mustBookID(t *testing.T, s string) domain.BookID {
	t.Helper()
	id, err := domain.NewBookID(s)
	if err != nil {
		t.Fatalf("book id %q: %v", s, err)
	}
	return id
}

func mustUserID(t *testing.T, s string) domain.UserID {
	t.Helper()
	id, err := domain.NewUserID(s)
	if err != nil {
		t.Fatalf("user id %q: %v", s, err)
	}
	return id
}

func mustMoney(t *testing.T, minor int64) domain.Money {
	t.Helper()
	m, err := domain.FromMinorUnits(minor)
	if err != nil {
		t.Fatalf("money %d: %v", minor, err)
	}
	return m
}

func mustDecimal(t *testing.T, v float64) odds.Decimal {
	t.Helper()
	d, err := odds.NewDecimal(v)
	if err != nil {
		t.Fatalf("decimal odds %v: %v", v, err)
	}
	return d
}

// seedBoard fills the fakes with one league, one event, one moneyline market,
// two selections and two books' quotes on each — the smallest fixture that
// exercises the whole board path including best-price selection and overround.
func seedBoard(t *testing.T, d *deps) {
	t.Helper()

	sportID := mustSportID(t, "basketball")
	leagueID, err := domain.NewLeagueID("nba")
	if err != nil {
		t.Fatalf("league id: %v", err)
	}
	eventID := mustEventID(t, "evt_nba_bos_lal")
	marketID := mustMarketID(t, "mkt_nba_bos_lal_ml")
	home := mustSelectionID(t, "sel_bos")
	away := mustSelectionID(t, "sel_lal")
	sharp := mustBookID(t, "bk_sharp")
	soft := mustBookID(t, "bk_soft")

	d.catalogue.sports = []Sport{{ID: sportID, Slug: mustSlug(t, "basketball"), Name: "Basketball"}}
	d.catalogue.leagues = []League{{ID: leagueID, SportID: sportID, Slug: mustSlug(t, "nba"), Name: "NBA"}}
	d.catalogue.books = []Book{
		{ID: sharp, Slug: mustSlug(t, "sharp"), Name: "Sharp Book", Kind: domain.BookKindExternal, Reference: true},
		{ID: soft, Slug: mustSlug(t, "soft"), Name: "Soft Book", Kind: domain.BookKindSynthetic},
	}
	d.catalogue.events = []Event{{
		ID:                 eventID,
		LeagueID:           leagueID,
		Kind:               domain.EventKindMatch,
		Name:               "Boston Celtics at Los Angeles Lakers",
		HomeCompetitorName: "Los Angeles Lakers",
		AwayCompetitorName: "Boston Celtics",
		ScheduledStart:     testNow.Add(2 * time.Hour),
		Status:             domain.EventStatusScheduled,
		ObservedAt:         testNow.Add(-time.Minute),
	}}
	d.catalogue.markets = []Market{{
		ID: marketID, EventID: eventID, Type: domain.MarketTypeMoneyline,
		Status: domain.MarketStatusOpen, ObservedAt: testNow.Add(-time.Minute),
	}}
	d.catalogue.selections = []Selection{
		{ID: away, MarketID: marketID, MarketType: domain.MarketTypeMoneyline, Role: domain.SelectionRoleAway, Name: "Boston Celtics"},
		{ID: home, MarketID: marketID, MarketType: domain.MarketTypeMoneyline, Role: domain.SelectionRoleHome, Name: "Los Angeles Lakers"},
	}
	d.prices.quotes = []Quote{
		{SelectionID: home, BookID: sharp, Odds: mustDecimal(t, 1.91), ObservedAt: testNow.Add(-30 * time.Second), IngestedAt: testNow.Add(-29 * time.Second)},
		{SelectionID: home, BookID: soft, Odds: mustDecimal(t, 1.95), ObservedAt: testNow.Add(-20 * time.Second), IngestedAt: testNow.Add(-19 * time.Second)},
		{SelectionID: away, BookID: sharp, Odds: mustDecimal(t, 1.95), ObservedAt: testNow.Add(-30 * time.Second), IngestedAt: testNow.Add(-29 * time.Second)},
		{SelectionID: away, BookID: soft, Odds: mustDecimal(t, 1.87), ObservedAt: testNow.Add(-20 * time.Second), IngestedAt: testNow.Add(-19 * time.Second)},
	}
}

// authedHandler wraps h in the REAL authentication middleware with a fake
// verifier, so an account-handler test exercises the same path production does.
//
// A test cannot fabricate an identity directly, and that is deliberate:
// internal/httpapi/middleware keeps `withIdentity` unexported precisely so that
// "only Authenticate may establish an identity" is enforced by the compiler
// rather than by convention. Going through the middleware is the only way in,
// which is exactly the property worth having.
func authedHandler(t *testing.T, h http.Handler, user domain.UserID) http.Handler {
	t.Helper()
	mw, err := middleware.Authenticate(middleware.AuthOptions{
		Authenticator: middleware.AuthenticatorFunc(func(_ context.Context, token string) (middleware.Identity, error) {
			if token != testAccessToken {
				return middleware.Identity{}, auth.ErrAccessTokenInvalid
			}
			return middleware.Identity{
				UserID:    user,
				SessionID: "fam_test",
				IssuedAt:  testNow.Add(-time.Minute),
				ExpiresAt: testNow.Add(10 * time.Minute),
				AMR:       []string{"pwd"},
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("build Authenticate middleware: %v", err)
	}
	return mw(h)
}

// testAccessToken is the only token the fake verifier accepts. It is a
// meaningless string: nothing in this package parses an access token, so the
// tests do not need a real one, and not minting one keeps a signing key out of
// the test tier entirely.
const testAccessToken = "test-access-token"

func bearer(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer "+testAccessToken)
	return r
}
