package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// -----------------------------------------------------------------------------
// Empty is correct
// -----------------------------------------------------------------------------

// TestEmptyCatalogueAnswers200WithEmptyCollections is the "no mock data" rule as
// an assertion.
//
// Against a database `ingest` has not filled, every read endpoint must answer
// 200 with an EMPTY collection — not a 404, not a 500, and emphatically not a
// sample event. An empty board is then diagnosable as "the pipeline is not
// running" instead of being mistaken for "the API is broken", and a demo can
// never accidentally show fabricated prices.
//
// `[]` and not `null`: a JSON null makes a client branch on a distinction this
// API never intends, and it is the difference between a board that renders
// nothing and a board that throws.
func TestEmptyCatalogueAnswers200WithEmptyCollections(t *testing.T) {
	t.Parallel()

	d := newDeps()
	api := d.api(t)

	t.Run("sports", func(t *testing.T) {
		rec, req := newRequest(http.MethodGet, "/v1/sports", nil)
		api.handleListSports(rec, req)
		requireStatus(t, rec, http.StatusOK)
		assertJSONArrayNotNull(t, rec, "data")
	})

	t.Run("books", func(t *testing.T) {
		rec, req := newRequest(http.MethodGet, "/v1/books", nil)
		api.handleListBooks(rec, req)
		requireStatus(t, rec, http.StatusOK)
		assertJSONArrayNotNull(t, rec, "data")
	})

	t.Run("board", func(t *testing.T) {
		rec, req := newRequest(http.MethodGet, "/v1/board", nil)
		api.handleBoard(rec, req)
		requireStatus(t, rec, http.StatusOK)
		assertJSONArrayNotNull(t, rec, "data")

		page := decodeJSONBody[gen.BoardPage](t, rec)
		if page.Page.HasMore {
			t.Error("empty board reports has_more")
		}
		if page.Page.NextCursor != nil {
			t.Error("empty board minted a cursor that would return nothing")
		}
	})
}

// -----------------------------------------------------------------------------
// The board
// -----------------------------------------------------------------------------

func TestBoardRendersPricesFromTheStore(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	api := d.api(t)

	rec, req := newRequest(http.MethodGet, "/v1/board", nil)
	api.handleBoard(rec, req)
	requireStatus(t, rec, http.StatusOK)

	page := decodeJSONBody[gen.BoardPage](t, rec)
	if len(page.Data) != 1 {
		t.Fatalf("board returned %d entries, want 1", len(page.Data))
	}
	entry := page.Data[0]
	if len(entry.Markets) != 1 {
		t.Fatalf("event carries %d markets, want 1", len(entry.Markets))
	}

	// Selections come back in domain display order — home before away — which is
	// NOT the lexicographic order of the role strings and therefore cannot come
	// from any SQL ORDER BY. The fake deliberately stores them away-first.
	roles := []gen.SelectionRole{}
	for _, s := range entry.Markets[0].Selections {
		roles = append(roles, s.Role)
	}
	want := []gen.SelectionRole{gen.SelectionRole("home"), gen.SelectionRole("away")}
	if fmt.Sprint(roles) != fmt.Sprint(want) {
		t.Errorf("selection order = %v, want %v (domain.SelectionRole.DisplayOrder)", roles, want)
	}

	// The best price is the LONGEST odds, and it is computed over the quotes in
	// this very response so a client can check it.
	home := entry.Markets[0].Selections[0]
	if home.BestPrice == nil {
		t.Fatal("no best price on a selection with two quotes")
	}
	if home.BestPrice.DecimalOdds != 1.95 {
		t.Errorf("best price = %v, want 1.95 (the longer of 1.91 and 1.95)", home.BestPrice.DecimalOdds)
	}

	// as_of is the pinned clock, read once.
	if !page.AsOf.Equal(testNow) {
		t.Errorf("as_of = %v, want the injected clock %v", page.AsOf, testNow)
	}
}

// TestBoardAlwaysBoundsTheQuoteWindow guards the property prices.sql calls
// non-negotiable: a current-line read must carry a lower bound on observed_at,
// or it defeats chunk exclusion on a hypertable whose chunk count grows for the
// life of the deployment.
func TestBoardAlwaysBoundsTheQuoteWindow(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	api := d.api(t)

	rec, req := newRequest(http.MethodGet, "/v1/board", nil)
	api.handleBoard(rec, req)
	requireStatus(t, rec, http.StatusOK)

	if d.prices.lastSince.IsZero() {
		t.Fatal("the board asked for quotes with no freshness horizon: chunk exclusion is defeated")
	}
	if got, want := d.prices.lastSince, testNow.Add(-quoteFreshness); !got.Equal(want) {
		t.Errorf("freshness horizon = %v, want %v", got, want)
	}
}

// TestBoardPaginationNeitherSkipsNorDuplicates is the reason keyset pagination
// exists.
//
// It walks the whole set one page at a time and asserts that every event appears
// exactly once. The same walk under OFFSET pagination against a set that grows
// between pages would drop a row silently, which is precisely the failure this
// design refuses to accept.
func TestBoardPaginationNeitherSkipsNorDuplicates(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)

	// Ten events, two of them sharing a scheduled_start so the tie-break in the
	// ordering is actually exercised. A cursor on scheduled_start alone could
	// not disambiguate these two.
	base := testNow.Add(time.Hour)
	d.catalogue.events = nil
	for i := range 10 {
		start := base.Add(time.Duration(i/2) * time.Hour)
		d.catalogue.events = append(d.catalogue.events, Event{
			ID:             mustEventID(t, fmt.Sprintf("evt_%02d", i)),
			LeagueID:       d.catalogue.leagues[0].ID,
			Kind:           domain.EventKindMatch,
			Name:           fmt.Sprintf("Fixture %d", i),
			ScheduledStart: start,
			Status:         domain.EventStatusScheduled,
			ObservedAt:     testNow,
		})
	}
	api := d.api(t)

	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 10; page++ {
		target := "/v1/board?limit=3&starting_before=" + testNow.Add(48*time.Hour).Format(time.RFC3339)
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec, req := newRequest(http.MethodGet, target, nil)
		api.handleBoard(rec, req)
		requireStatus(t, rec, http.StatusOK)

		body := decodeJSONBody[gen.BoardPage](t, rec)
		for _, e := range body.Data {
			seen[e.Event.Id]++
		}
		if !body.Page.HasMore {
			if body.Page.NextCursor != nil {
				t.Error("last page minted a cursor")
			}
			break
		}
		if body.Page.NextCursor == nil {
			t.Fatal("has_more is true but no cursor was minted; the listing cannot continue")
		}
		cursor = *body.Page.NextCursor
	}

	if len(seen) != 10 {
		t.Errorf("walked %d distinct events, want 10 — the pagination skipped rows", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("event %s appeared %d times; keyset pagination must not duplicate", id, n)
		}
	}
}

// TestCursorIsRejectedWhenTheQueryChanges is the scope fingerprint doing its job.
//
// A client that changes `starting_before` mid-listing would otherwise receive a
// page from a DIFFERENT set, ordered consistently, with nothing anywhere
// reporting a problem.
func TestCursorIsRejectedWhenTheQueryChanges(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	for i := range 4 {
		d.catalogue.events = append(d.catalogue.events, Event{
			ID:             mustEventID(t, fmt.Sprintf("evt_extra_%d", i)),
			LeagueID:       d.catalogue.leagues[0].ID,
			Kind:           domain.EventKindMatch,
			Name:           "Extra",
			ScheduledStart: testNow.Add(time.Duration(i+1) * time.Hour),
			Status:         domain.EventStatusScheduled,
			ObservedAt:     testNow,
		})
	}
	api := d.api(t)

	horizonA := testNow.Add(48 * time.Hour).Format(time.RFC3339)
	rec, req := newRequest(http.MethodGet, "/v1/board?limit=1&starting_before="+horizonA, nil)
	api.handleBoard(rec, req)
	requireStatus(t, rec, http.StatusOK)

	first := decodeJSONBody[gen.BoardPage](t, rec)
	if first.Page.NextCursor == nil {
		t.Fatal("expected a cursor on a page with more results")
	}

	// Same cursor, different window.
	horizonB := testNow.Add(72 * time.Hour).Format(time.RFC3339)
	rec2, req2 := newRequest(http.MethodGet,
		"/v1/board?limit=1&starting_before="+horizonB+"&cursor="+*first.Page.NextCursor, nil)
	api.handleBoard(rec2, req2)

	requireStatus(t, rec2, http.StatusBadRequest)
	body := decodeJSONBody[gen.Error](t, rec2)
	if body.Code != gen.ErrorCodeInvalidCursor {
		t.Errorf("code = %q, want %q", body.Code, gen.ErrorCodeInvalidCursor)
	}
}

// TestUnknownBookSlugIsRejected: a typo must not quietly return an empty price
// grid, which a client would render as "no book is quoting this".
func TestUnknownBookSlugIsRejected(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	api := d.api(t)

	rec, req := newRequest(http.MethodGet, "/v1/board?book=nosuchbook", nil)
	api.handleBoard(rec, req)

	requireStatus(t, rec, http.StatusBadRequest)
	body := decodeJSONBody[gen.Error](t, rec)
	if body.Code != gen.ErrorCodeInvalidParameter {
		t.Errorf("code = %q, want %q", body.Code, gen.ErrorCodeInvalidParameter)
	}
	if body.InvalidParams == nil || len(*body.InvalidParams) == 0 {
		t.Fatal("no invalid_params: the client cannot tell which parameter was wrong")
	}
	if (*body.InvalidParams)[0].Name != "book" {
		t.Errorf("invalid param = %q, want \"book\"", (*body.InvalidParams)[0].Name)
	}
	// The offending VALUE must not be reflected: an error body is rendered by
	// clients and is the one place a caller controls the bytes.
	if strings.Contains(rec.Body.String(), "nosuchbook") {
		t.Error("the response reflects the client-supplied parameter value verbatim")
	}
}

// TestOddsFormatAddsDisplayAndNeverReplacesDecimal.
//
// Phase 1 established Decimal as canonical and American/Fractional as LOSSY
// display formats. If the server ever substituted the rendering for the
// canonical value, a client could not switch format without refetching and any
// arithmetic it did would be on a rounded number.
func TestOddsFormatAddsDisplayAndNeverReplacesDecimal(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	api := d.api(t)

	for _, tc := range []struct {
		format      string
		wantDisplay bool
	}{
		{"", false},
		{"decimal", false},
		{"american", true},
		{"fractional", true},
	} {
		t.Run("format="+tc.format, func(t *testing.T) {
			target := "/v1/board"
			if tc.format != "" {
				target += "?odds_format=" + tc.format
			}
			rec, req := newRequest(http.MethodGet, target, nil)
			api.handleBoard(rec, req)
			requireStatus(t, rec, http.StatusOK)

			page := decodeJSONBody[gen.BoardPage](t, rec)
			price := page.Data[0].Markets[0].Selections[0].Prices[0]

			if price.DecimalOdds <= 1.0 {
				t.Fatalf("canonical decimal_odds missing or invalid: %v", price.DecimalOdds)
			}
			if tc.wantDisplay && price.Display == nil {
				t.Error("no display string for a non-decimal format")
			}
			if !tc.wantDisplay && price.Display != nil {
				t.Errorf("display string %q present for the canonical format", *price.Display)
			}
		})
	}
}

func TestBadOddsFormatIsRejected(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t)
	rec, req := newRequest(http.MethodGet, "/v1/board?odds_format=binary", nil)
	api.handleBoard(rec, req)
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestLimitOutOfRangeIsRejectedRatherThanClamped(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t)
	for _, limit := range []string{"0", "-1", "201", "abc"} {
		rec, req := newRequest(http.MethodGet, "/v1/board?limit="+limit, nil)
		api.handleBoard(rec, req)
		// Clamping would make a client believe it received a whole set it did
		// not, and the bug would surface much later and somewhere else.
		requireStatus(t, rec, http.StatusBadRequest)
	}
}

// -----------------------------------------------------------------------------
// Multi-book comparison
// -----------------------------------------------------------------------------

func TestComparisonReportsOverroundOnlyOnACompleteMarket(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	api := d.api(t)

	// Drop one of the soft book's two quotes so its market is partial.
	d.prices.quotes = d.prices.quotes[:3]

	rec, req := newRequest(http.MethodGet, "/v1/markets/mkt_nba_bos_lal_ml/prices", nil,
		"marketId", "mkt_nba_bos_lal_ml")
	api.handleCompareMarket(rec, req)
	requireStatus(t, rec, http.StatusOK)

	body := decodeJSONBody[gen.MarketComparison](t, rec)
	if len(body.Books) != 2 {
		t.Fatalf("got %d books, want 2", len(body.Books))
	}
	for _, b := range body.Books {
		switch b.BookSlug {
		case "sharp":
			if b.Overround == nil {
				t.Error("no overround on a book quoting the whole market")
			}
		case "soft":
			// AN OVERROUND OVER A PARTIAL MARKET IS NOT A MARGIN: it is a
			// smaller number that renders as a tighter, more attractive price
			// than any book actually offers.
			if b.Overround != nil {
				t.Errorf("overround %v reported for a book quoting 1 of 2 selections", *b.Overround)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// History
// -----------------------------------------------------------------------------

func TestHistoryRequiresABoundedWindow(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	d.catalogue.selections = append(d.catalogue.selections, Selection{
		ID: mustSelectionID(t, "sel_bos"), MarketID: mustMarketID(t, "mkt_nba_bos_lal_ml"),
		MarketType: domain.MarketTypeMoneyline, Role: domain.SelectionRoleAway, Name: "Boston",
	})
	api := d.api(t)

	// No `from` at all.
	rec, req := newRequest(http.MethodGet, "/v1/selections/sel_bos/history?book=sharp", nil,
		"selectionId", "sel_bos")
	api.handleHistory(rec, req)
	requireStatus(t, rec, http.StatusBadRequest)

	body := decodeJSONBody[gen.Error](t, rec)
	if body.InvalidParams == nil {
		t.Fatal("no invalid_params naming the missing bound")
	}
}

func TestHistoryRefusesAWindowItCannotDrawRatherThanTruncating(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	api := d.api(t)

	from := testNow.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	target := "/v1/selections/sel_bos/history?book=sharp&resolution=10s&max_points=100&from=" + from
	rec, req := newRequest(http.MethodGet, target, nil, "selectionId", "sel_bos")
	api.handleHistory(rec, req)

	// A truncated line-movement chart does not LOOK truncated: it looks like a
	// market that stopped moving, or one that closed at a price it never closed
	// at. Refusing is the only behaviour that cannot mislead.
	requireStatus(t, rec, http.StatusUnprocessableEntity)

	body := decodeJSONBody[gen.Error](t, rec)
	if body.Code != gen.ErrorCodeUnprocessable {
		t.Errorf("code = %q, want %q", body.Code, gen.ErrorCodeUnprocessable)
	}
	if body.InvalidParams == nil || len(*body.InvalidParams) == 0 {
		t.Fatal("the 422 does not say which parameter to change")
	}
	// It must be actionable: name a resolution that would fit.
	if !strings.Contains((*body.InvalidParams)[0].Reason, "try ") {
		t.Errorf("reason %q does not suggest a workable resolution", (*body.InvalidParams)[0].Reason)
	}
}

func TestHistoryRejectsAnInvertedWindow(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	api := d.api(t)

	from := testNow.Format(time.RFC3339)
	to := testNow.Add(-time.Hour).Format(time.RFC3339)
	target := "/v1/selections/sel_bos/history?book=sharp&from=" + from + "&to=" + to
	rec, req := newRequest(http.MethodGet, target, nil, "selectionId", "sel_bos")
	api.handleHistory(rec, req)
	requireStatus(t, rec, http.StatusUnprocessableEntity)
}

func TestHistoryRendersRawAndBucketedIdentically(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	d.prices.history = []HistoryPoint{{
		At: testNow.Add(-time.Hour), Open: mustDecimal(t, 1.90), High: mustDecimal(t, 1.95),
		Low: mustDecimal(t, 1.85), Close: mustDecimal(t, 1.92), Samples: 7,
	}}
	api := d.api(t)

	from := testNow.Add(-2 * time.Hour).Format(time.RFC3339)
	target := "/v1/selections/sel_bos/history?book=sharp&resolution=1m&from=" + from
	rec, req := newRequest(http.MethodGet, target, nil, "selectionId", "sel_bos")
	api.handleHistory(rec, req)
	requireStatus(t, rec, http.StatusOK)

	body := decodeJSONBody[gen.HistorySeries](t, rec)
	if len(body.Points) != 1 {
		t.Fatalf("got %d points, want 1", len(body.Points))
	}
	p := body.Points[0]
	if p.Open != 1.90 || p.High != 1.95 || p.Low != 1.85 || p.Close != 1.92 || p.Samples != 7 {
		t.Errorf("point = %+v, want the store's OHLC and sample count unchanged", p)
	}
}

// -----------------------------------------------------------------------------
// Account: money on the wire
// -----------------------------------------------------------------------------

// TestBalanceIsAnIntegerNumberOfMinorUnits is the money contract, asserted
// against the raw JSON rather than against a decoded struct.
//
// Decoding into gen.BalanceResponse would prove nothing: the Go field is int64
// and would round-trip whatever came in. The bytes are what a JavaScript client
// sees, so the bytes are what the test reads.
func TestBalanceIsAnIntegerNumberOfMinorUnits(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_test")
	d := newDeps()
	d.accounts.profiles[user] = Profile{
		ID: user, Email: "sharp@example.test", Status: auth.UserStatusActive, CreatedAt: testNow,
	}
	d.ledger.balances = []Balance{
		{Kind: domain.AccountKindUserCash, Amount: mustMoney(t, 1234567), Entries: 12},
		{Kind: domain.AccountKindUserEscrow, Amount: mustMoney(t, 250000), Entries: 3},
	}
	api := d.api(t)

	rec := serveAuthed(t, api.handleGetBalance, user, http.MethodGet, "/v1/account/balance", nil)
	requireStatus(t, rec, http.StatusOK)

	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// A float would serialise with a decimal point or an exponent. Neither may
	// appear anywhere in a money field.
	for _, field := range []string{"total_minor"} {
		got := string(raw[field])
		if strings.ContainsAny(got, ".eE") {
			t.Errorf("%s = %s: money must be an integer, never a float", field, got)
		}
		if strings.HasPrefix(got, `"`) {
			t.Errorf("%s = %s: money is a JSON number here, not a string", field, got)
		}
	}

	body := decodeJSONBody[gen.BalanceResponse](t, rec)
	if body.Cash.BalanceMinor != 1234567 {
		t.Errorf("cash = %d, want 1234567", body.Cash.BalanceMinor)
	}
	if body.TotalMinor != 1234567+250000 {
		t.Errorf("total = %d, want %d", body.TotalMinor, 1234567+250000)
	}
	if body.Currency != gen.PLAY {
		t.Errorf("currency = %q: play money must never be labelled as a real currency", body.Currency)
	}
	if body.MinorUnitsPerMajor != gen.N100 {
		t.Errorf("minor_units_per_major = %d, want 100", body.MinorUnitsPerMajor)
	}
}

// TestBalanceOfAnUntouchedAccountIsZeroWithZeroEntries.
//
// "Never funded" and "funded and spent to zero" are different facts, and
// entry_count is the only thing that distinguishes them. An account with no
// ledger rows must not 404 and must not be omitted.
func TestBalanceOfAnUntouchedAccountIsZeroWithZeroEntries(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_new")
	d := newDeps()
	d.ledger.balances = []Balance{
		{Kind: domain.AccountKindUserCash},
		{Kind: domain.AccountKindUserEscrow},
	}
	api := d.api(t)

	rec := serveAuthed(t, api.handleGetBalance, user, http.MethodGet, "/v1/account/balance", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeJSONBody[gen.BalanceResponse](t, rec)
	if body.Cash.BalanceMinor != 0 || body.Cash.EntryCount != 0 {
		t.Errorf("cash = %+v, want a zero balance with zero entries", body.Cash)
	}
	if body.TotalMinor != 0 {
		t.Errorf("total = %d, want 0", body.TotalMinor)
	}
}

// -----------------------------------------------------------------------------
// Account: profile and limits
// -----------------------------------------------------------------------------

// TestProfileCarriesNoKYCField is CLAUDE.md §0 as a test.
//
// It reads the SERIALISED KEYS rather than the struct, because the failure this
// guards against is somebody adding a field to the spec — which would generate a
// Go field and pass any struct-level assertion.
func TestProfileCarriesNoKYCField(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_test")
	d := newDeps()
	d.accounts.profiles[user] = Profile{
		ID: user, Email: "sharp@example.test", Status: auth.UserStatusActive, CreatedAt: testNow,
	}
	api := d.api(t)

	rec := serveAuthed(t, api.handleGetAccount, user, http.MethodGet, "/v1/account", nil)
	requireStatus(t, rec, http.StatusOK)

	keys := map[string]json.RawMessage{}
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Migration 00005 lists every one of these as deliberately absent. Adding
	// one turns a rigorous simulation into an unlicensed real-money book.
	forbidden := []string{
		"ssn", "tax_id", "passport", "drivers_license", "date_of_birth", "dob",
		"legal_name", "first_name", "last_name", "address", "postcode", "zip",
		"country", "state", "jurisdiction", "geolocation", "ip_country",
		"payment_method", "card", "bank_account", "wallet", "balance",
		"verification_status", "kyc_status", "document",
	}
	for _, f := range forbidden {
		if _, ok := keys[f]; ok {
			t.Errorf("the account response carries %q; CLAUDE.md §0 forbids KYC, geolocation and payment fields", f)
		}
	}

	want := []string{"id", "email", "status", "totp_enrolled", "created_at"}
	if len(keys) != len(want) {
		t.Errorf("account has %d fields (%v), want exactly %v", len(keys), sortedJSONKeys(keys), want)
	}
}

// TestUnconfirmedTOTPIsNotReportedAsEnrolled.
//
// An enrolment that has been started and not proved is NOT a second factor.
// Reporting it as one tells a user they are protected when a mis-scanned QR code
// means they are not — and locks them out at the next login.
func TestUnconfirmedTOTPIsNotReportedAsEnrolled(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_test")
	d := newDeps()
	d.accounts.profiles[user] = Profile{
		ID: user, Email: "a@b.test", Status: auth.UserStatusActive, CreatedAt: testNow,
		TOTPConfirmed: false, TOTPPending: true,
	}
	api := d.api(t)

	rec := serveAuthed(t, api.handleGetAccount, user, http.MethodGet, "/v1/account", nil)
	requireStatus(t, rec, http.StatusOK)

	body := decodeJSONBody[gen.Account](t, rec)
	if body.TotpEnrolled {
		t.Error("an unconfirmed enrolment is reported as an enrolled second factor")
	}
}

func TestSetLimitRejectsImpossibleCombinations(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_test")

	for _, tc := range []struct {
		name string
		body string
	}{
		{"session limit denominated in money", `{"kind":"session","period":"session","amount_minor":1000}`},
		{"money limit denominated in time", `{"kind":"loss","period":"day","duration_seconds":600}`},
		{"session kind with a money period", `{"kind":"session","period":"day","duration_seconds":600}`},
		{"money kind with the session period", `{"kind":"loss","period":"session","amount_minor":1000}`},
		{"negative amount", `{"kind":"loss","period":"day","amount_minor":-5}`},
		{"duration beyond one day", `{"kind":"session","period":"session","duration_seconds":90000}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeps()
			api := d.api(t)
			rec := serveAuthed(t, api.handleSetLimit, user, http.MethodPost,
				"/v1/account/limits", strings.NewReader(tc.body))

			requireStatus(t, rec, http.StatusUnprocessableEntity)
			if len(d.limits.set) != 0 {
				t.Error("an impossible limit reached the store")
			}
		})
	}
}

func TestSetLimitRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_test")
	d := newDeps()
	api := d.api(t)

	// Every request schema declares additionalProperties:false. A server that
	// ignored an extra field would let a client believe it had set something it
	// had not — the failure mode where a caller "sets" a limit through a
	// misspelled key and is told it worked.
	rec := serveAuthed(t, api.handleSetLimit, user, http.MethodPost, "/v1/account/limits",
		strings.NewReader(`{"kind":"loss","period":"day","amount_minor":1000,"amount":50}`))

	requireStatus(t, rec, http.StatusBadRequest)
	if len(d.limits.set) != 0 {
		t.Error("a request with an unknown field reached the store")
	}
}

func TestSetLimitReportsTheStoresEffectiveFrom(t *testing.T) {
	t.Parallel()

	user := mustUserID(t, "usr_test")
	amount := mustMoney(t, 500000)
	effective := testNow.Add(24 * time.Hour)

	d := newDeps()
	d.limits.result = Limit{
		ID: "lim_1", UserID: user, Kind: auth.LimitKindLoss, Period: auth.LimitPeriodDay,
		Amount: &amount, RequestedAt: testNow, EffectiveFrom: effective,
	}
	api := d.api(t)

	rec := serveAuthed(t, api.handleSetLimit, user, http.MethodPost, "/v1/account/limits",
		strings.NewReader(`{"kind":"loss","period":"day","amount_minor":500000}`))
	requireStatus(t, rec, http.StatusCreated)

	body := decodeJSONBody[gen.Limit](t, rec)
	if !body.EffectiveFrom.Equal(effective) {
		t.Errorf("effective_from = %v, want the store's %v", body.EffectiveFrom, effective)
	}
	// A pending loosening is returned with in_force false rather than hidden:
	// the user asked for it, it is going to happen, and seeing it coming is the
	// point of the cooling-off period.
	if body.InForce {
		t.Error("a limit whose effective_from is in the future reports in_force")
	}
	if body.AmountMinor == nil || *body.AmountMinor != 500000 {
		t.Errorf("amount_minor = %v, want 500000", body.AmountMinor)
	}
	// The audit context must reach the store, or nothing can be recorded.
	if len(d.limits.set) != 1 {
		t.Fatalf("store saw %d requests, want 1", len(d.limits.set))
	}
	if d.limits.set[0].Audit.At.IsZero() {
		t.Error("the audit context carries no instant")
	}
}

// -----------------------------------------------------------------------------
// Auth
// -----------------------------------------------------------------------------

// TestLoginDoesNotEnumerateUsers.
//
// An unknown address and a wrong password must be indistinguishable in status
// code AND response body. internal/auth guarantees they are indistinguishable in
// TIMING by doing the same work; this test guards the half that lives here — the
// error mapping — because one extra `case` in failAuth would undo it.
func TestLoginDoesNotEnumerateUsers(t *testing.T) {
	t.Parallel()

	responses := make([]string, 0, 2)
	statuses := make([]int, 0, 2)

	// internal/auth returns the SAME sentinel for both, by construction. The
	// assertion is that this package cannot turn one sentinel into two answers.
	for _, email := range []string{"nobody@example.test", "known@example.test"} {
		d := newDeps()
		d.sessions.authErr = auth.ErrCredentials
		api := d.api(t)

		body := fmt.Sprintf(`{"email":%q,"password":"correct-horse-battery"}`, email)
		rec, req := newRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(body))
		api.handleLogin(rec, req)

		statuses = append(statuses, rec.Code)
		responses = append(responses, redactRequestID(rec.Body.String()))
	}

	if statuses[0] != statuses[1] {
		t.Errorf("status codes differ: %v", statuses)
	}
	if responses[0] != responses[1] {
		t.Errorf("response bodies differ:\n  %s\n  %s", responses[0], responses[1])
	}
	if statuses[0] != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", statuses[0])
	}
}

// TestRefreshReportsReuseIdenticallyToAnUnknownToken.
//
// A thief replaying a stolen refresh token must not learn from the response that
// they tripped the reuse detector — that would tell them the legitimate user is
// still active and that the family is now dead, which is exactly the signal that
// makes a targeted follow-up worth attempting.
func TestRefreshReportsReuseIdenticallyToAnUnknownToken(t *testing.T) {
	t.Parallel()

	bodies := map[error]string{}
	statuses := map[error]int{}

	for _, sentinel := range []error{
		auth.ErrTokenUnknown, auth.ErrTokenExpired, auth.ErrTokenReuse, auth.ErrSessionRevoked,
	} {
		d := newDeps()
		d.sessions.redeemErr = sentinel
		api := d.api(t)

		rec, req := newRequest(http.MethodPost, "/v1/auth/refresh",
			strings.NewReader(`{"refresh_token":"0123456789abcdef0123456789abcdef"}`))
		api.handleRefresh(rec, req)

		statuses[sentinel] = rec.Code
		bodies[sentinel] = redactRequestID(rec.Body.String())
	}

	want := bodies[auth.ErrTokenUnknown]
	for sentinel, got := range bodies {
		if got != want {
			t.Errorf("%v produced a distinguishable body:\n  %s\n  %s", sentinel, got, want)
		}
		if statuses[sentinel] != http.StatusUnauthorized {
			t.Errorf("%v produced status %d, want 401", sentinel, statuses[sentinel])
		}
	}
}

// TestBlockedAccountIsForbiddenNotUnauthorized.
//
// The credential was correct. Telling a self-excluded user "wrong password"
// would be both a lie and an invitation to keep trying.
func TestBlockedAccountIsForbiddenNotUnauthorized(t *testing.T) {
	t.Parallel()

	for _, sentinel := range []error{auth.ErrAccountSuspended, auth.ErrAccountClosed, auth.ErrSelfExcluded} {
		d := newDeps()
		d.sessions.authErr = sentinel
		api := d.api(t)

		rec, req := newRequest(http.MethodPost, "/v1/auth/login",
			strings.NewReader(`{"email":"a@b.test","password":"correct-horse-battery"}`))
		api.handleLogin(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%v produced status %d, want 403", sentinel, rec.Code)
		}
		body := decodeJSONBody[gen.Error](t, rec)
		if body.Code != gen.ErrorCodeAccountNotActive {
			t.Errorf("%v produced code %q, want %q", sentinel, body.Code, gen.ErrorCodeAccountNotActive)
		}
	}
}

func TestTOTPRequiredIsItsOwnCode(t *testing.T) {
	t.Parallel()

	d := newDeps()
	d.sessions.authErr = auth.ErrSecondFactorRequired
	api := d.api(t)

	rec, req := newRequest(http.MethodPost, "/v1/auth/login",
		strings.NewReader(`{"email":"a@b.test","password":"correct-horse-battery"}`))
	api.handleLogin(rec, req)

	requireStatus(t, rec, http.StatusUnauthorized)
	body := decodeJSONBody[gen.Error](t, rec)
	// Safe to be specific: this response is only reachable AFTER the password
	// has been verified, so it discloses nothing to anyone who does not already
	// hold it.
	if body.Code != gen.ErrorCodeTotpRequired {
		t.Errorf("code = %q, want %q", body.Code, gen.ErrorCodeTotpRequired)
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	t.Parallel()

	d := newDeps()
	api := d.api(t)

	rec, req := newRequest(http.MethodPost, "/v1/auth/logout",
		strings.NewReader(`{"refresh_token":"0123456789abcdef0123456789abcdef"}`))
	api.handleLogout(rec, req)

	// Reporting "that token was already dead" is an enumeration oracle and no
	// caller can act on the distinction.
	requireStatus(t, rec, http.StatusNoContent)
	if rec.Body.Len() != 0 {
		t.Errorf("204 carries a body: %q", rec.Body.String())
	}
}

// -----------------------------------------------------------------------------
// Secrets
// -----------------------------------------------------------------------------

// TestNoSecretReachesTheLog is the assertion that cannot be made by reading the
// code once and trusting it afterwards.
//
// It drives every endpoint that handles a credential — with a distinctive
// password, refresh token and TOTP code — while capturing EVERY log line the
// package writes, and asserts none of the three appears anywhere. It also checks
// the response bodies, because an error message that echoed the credential would
// be just as bad.
func TestNoSecretReachesTheLog(t *testing.T) {
	t.Parallel()

	const (
		password = "PASSWORD-CANARY-8f2a1c"
		token    = "REFRESHTOKEN-CANARY-4b7e9d"
		code     = "137913"
	)

	var logged bytes.Buffer
	d := newDeps()
	d.logger = slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// Force the failure paths, which are where a careless implementation logs
	// "what was presented".
	d.sessions.authErr = auth.ErrCredentials
	d.sessions.redeemErr = auth.ErrTokenReuse
	d.sessions.totpErr = auth.ErrSecondFactorInvalid
	api := d.api(t)

	user := mustUserID(t, "usr_test")
	var bodies bytes.Buffer

	run := func(h http.HandlerFunc, method, target, body string) {
		rec, req := newRequest(method, target, strings.NewReader(body))
		h(rec, req)
		bodies.WriteString(rec.Body.String())
	}

	run(api.handleRegister, http.MethodPost, "/v1/auth/register",
		fmt.Sprintf(`{"email":"a@b.test","password":%q}`, password))
	run(api.handleLogin, http.MethodPost, "/v1/auth/login",
		fmt.Sprintf(`{"email":"a@b.test","password":%q,"totp_code":%q}`, password, code))
	run(api.handleRefresh, http.MethodPost, "/v1/auth/refresh",
		fmt.Sprintf(`{"refresh_token":%q}`, token))
	run(api.handleLogout, http.MethodPost, "/v1/auth/logout",
		fmt.Sprintf(`{"refresh_token":%q}`, token))

	rec := serveAuthed(t, api.handleConfirmTOTP, user, http.MethodPost,
		"/v1/account/totp/confirm", strings.NewReader(fmt.Sprintf(`{"code":%q}`, code)))
	bodies.WriteString(rec.Body.String())

	// A malformed body is the other place a naive implementation echoes input:
	// encoding/json includes the offending fragment in some of its messages.
	run(api.handleLogin, http.MethodPost, "/v1/auth/login",
		fmt.Sprintf(`{"email":"a@b.test","password":%q,`, password))

	for _, secret := range []string{password, token, code} {
		if strings.Contains(logged.String(), secret) {
			t.Errorf("a credential reached the log: %q\nlog:\n%s", secret, logged.String())
		}
		if strings.Contains(bodies.String(), secret) {
			t.Errorf("a credential was echoed in a response body: %q\nbodies:\n%s", secret, bodies.String())
		}
	}

	// The fakes prove the credentials really did travel through the handlers, so
	// the assertion above is not vacuously true against code that never saw them.
	if len(d.sessions.seenPasswords) == 0 || len(d.sessions.seenTokens) == 0 || len(d.sessions.seenCodes) == 0 {
		t.Fatal("the handlers never forwarded the credentials; the canary test proves nothing")
	}
}

// TestInternalErrorsNeverLeakTheirCause.
//
// An error body is an untrusted output surface. The cause goes to the log under
// the request id and nowhere else.
func TestInternalErrorsNeverLeakTheirCause(t *testing.T) {
	t.Parallel()

	const canary = "pq: relation \"events\" does not exist at /var/lib/secret.sql"

	d := newDeps()
	d.catalogue.err = errors.New(canary)
	api := d.api(t)

	rec, req := newRequest(http.MethodGet, "/v1/board", nil)
	api.handleBoard(rec, req)

	requireStatus(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String(), "events") || strings.Contains(rec.Body.String(), "secret.sql") {
		t.Errorf("the 500 body leaks interior state: %s", rec.Body.String())
	}

	body := decodeJSONBody[gen.Error](t, rec)
	if body.Code != gen.ErrorCodeInternal {
		t.Errorf("code = %q, want %q", body.Code, gen.ErrorCodeInternal)
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, want, rec.Body.String())
	}
	if rec.Code != http.StatusNoContent {
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") && !strings.Contains(ct, "yaml") {
			t.Fatalf("Content-Type = %q, want JSON", ct)
		}
	}
}

// serveAuthed drives a handler through the real authentication middleware.
func serveAuthed(t *testing.T, h http.HandlerFunc, user domain.UserID, method, target string, body *strings.Reader) *httptest.ResponseRecorder {
	t.Helper()

	var rec *httptest.ResponseRecorder
	var req *http.Request
	if body == nil {
		rec, req = newRequest(method, target, nil)
	} else {
		rec, req = newRequest(method, target, body)
	}
	authedHandler(t, h, user).ServeHTTP(rec, bearer(req))
	return rec
}

// redactRequestID removes the one field that legitimately differs between two
// otherwise-identical error responses.
func redactRequestID(body string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return body
	}
	delete(m, "request_id")
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return string(out)
}

func assertJSONArrayNotNull(t *testing.T, rec *httptest.ResponseRecorder, field string) {
	t.Helper()
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if string(raw[field]) != "[]" {
		t.Errorf("%s = %s, want [] (JSON null makes a client branch on a distinction the API never intends)",
			field, string(raw[field]))
	}
}

func sortedJSONKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
