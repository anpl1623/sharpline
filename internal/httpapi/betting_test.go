package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/betting"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// The betting surface's HTTP contract.
//
// WHAT THESE TESTS ASSERT, AND WHAT THEY DELIBERATELY DO NOT. Every rule that
// decides whether a slip becomes a ticket lives in internal/betting and is
// tested there against its own fakes: self-exclusion, the limit sums, the
// balance fold, the price re-read, the round-robin expansion, the double-entry
// movement. Re-testing any of them here would assert one rule twice and prove
// nothing about the layer under test.
//
// What is under test here is the TRANSLATION, and it is where this layer's own
// defects would live:
//
//   - a wire slip becomes the value internal/betting expects, with the seen line
//     and the acceptance intact, and with a rounding mode the caller cannot name;
//   - each sentinel becomes one status and one code, and the table is exhaustive
//     rather than illustrative;
//   - a price move reaches the client with BOTH numbers on it;
//   - a wager belonging to somebody else is indistinguishable from one that does
//     not exist;
//   - every money field on the wire is an integer.

const testUser = domain.UserID("usr_test")

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

func testLeg(t *testing.T, id, selection string, decimal float64) domain.Leg {
	t.Helper()
	price, err := domain.NewPrice(domain.PriceParams{
		SelectionID: mustSelectionID(t, selection),
		BookID:      mustBookID(t, "bk_sharp"),
		Decimal:     decimal,
		Line:        domain.NoLine(),
		ObservedAt:  testNow.Add(-30 * time.Second),
	})
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	legID, err := domain.NewLegID(id)
	if err != nil {
		t.Fatalf("leg id: %v", err)
	}
	leg, err := domain.NewLeg(domain.LegParams{
		ID:          legID,
		EventID:     mustEventID(t, "evt_nba_bos_lal"),
		MarketID:    mustMarketID(t, "mkt_nba_bos_lal_ml"),
		MarketType:  domain.MarketTypeMoneyline,
		Role:        domain.SelectionRoleHome,
		SelectionID: mustSelectionID(t, selection),
		Price:       price,
	})
	if err != nil {
		t.Fatalf("leg: %v", err)
	}
	return leg
}

// testStraight builds a placed straight at 1.91 for the given stake.
func testStraight(t *testing.T, id string, stakeMinor int64) domain.Wager {
	t.Helper()
	wagerID, err := domain.NewWagerID(id)
	if err != nil {
		t.Fatalf("wager id: %v", err)
	}
	w, err := domain.NewWager(domain.WagerParams{
		ID:              wagerID,
		UserID:          testUser,
		Kind:            domain.WagerKindStraight,
		Legs:            []domain.Leg{testLeg(t, "leg_"+id, "sel_bos", 1.91)},
		Stake:           mustMoney(t, stakeMinor),
		AcceptedDecimal: 1.91,
		Rounding:        wagerRounding,
		PlacedAt:        testNow.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("wager: %v", err)
	}
	return w
}

// placeBody is the smallest valid placement body: one leg, one stake.
func placeBody(selection string, seen float64, stakeMinor int64) string {
	return fmt.Sprintf(`{"kind":"straight","stake_minor":%d,"legs":[
		{"selection_id":%q,"book_id":"bk_sharp","seen_decimal":%v}]}`,
		stakeMinor, selection, seen)
}

// place issues a placement through the real authentication middleware.
func place(t *testing.T, d *deps, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if key != "" {
		headers[idempotencyHeader] = key
	}
	return callAuthed(t, d, http.MethodPost, "/v1/wagers", body, headers)
}

// callAuthed dispatches a request through the API's OWN route table, wrapped in
// the real authentication middleware.
//
// It routes rather than calling a handler directly, because half of what these
// tests assert is decided by the route table: which paths exist, which carry
// authentication, and what the path wildcards resolve to. A direct handler call
// with SetPathValue would assert the handler and silently skip the wiring.
//
// A test cannot fabricate an identity — internal/httpapi/middleware keeps
// `withIdentity` unexported so that "only Authenticate may establish an
// identity" is a compiler-enforced rule rather than a convention — so the only
// way in is through the middleware, which is exactly the property worth having.
func callAuthed(t *testing.T, d *deps, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	api := d.api(t)
	mux := http.NewServeMux()
	for _, route := range api.Routes() {
		h := route.Handler
		if len(route.Middleware) > 0 {
			h = authedHandler(t, h, testUser)
		}
		mux.Handle(route.Method+" "+route.Path, h)
	}

	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := bearer(httptest.NewRequest(method, target, reader))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// -----------------------------------------------------------------------------
// Placement
// -----------------------------------------------------------------------------

// TestPlaceWagerRequiresAnIdempotencyKey.
//
// A placement with no key has an AT-LEAST-ONCE money path: the wager id is
// derived from the key, so without one a retry books a second bet. Refusing is
// the only correct behaviour, and it must be a 400 rather than a 422 because the
// fault is in the request's framing.
func TestPlaceWagerRequiresAnIdempotencyKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		key  string
	}{
		{"absent", ""},
		{"blank", "   "},
		{"too long", strings.Repeat("k", betting.MaxIdempotencyKeyLen+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDeps()
			seedBoard(t, d)

			rec := place(t, d, placeBody("sel_bos", 1.91, 2500), tc.key)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			body := decodeJSONBody[gen.Error](t, rec)
			if body.Code != gen.ErrorCodeBadRequest {
				t.Errorf("code = %q, want bad_request", body.Code)
			}
			if d.betting.calls != 0 {
				t.Errorf("the placement service was called %d times; a keyless submit must not reach it",
					d.betting.calls)
			}
		})
	}
}

// TestPlaceWagerStatusDistinguishesFirstFromReplay.
//
// This is the whole point of the idempotency contract at the HTTP layer: a
// client that retried after a timeout and cannot tell whether its first attempt
// landed learns it from the status line, without comparing timestamps against a
// history page.
func TestPlaceWagerStatusDistinguishesFirstFromReplay(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		replayed bool
		want     int
	}{
		{"first submit", false, http.StatusCreated},
		{"replay", true, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDeps()
			seedBoard(t, d)
			d.betting.placement = betting.Placement{
				Wagers:   []domain.Wager{testStraight(t, "wgr_1", 2500)},
				Replayed: tc.replayed,
			}

			rec := place(t, d, placeBody("sel_bos", 1.91, 2500), "key-1")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			body := decodeJSONBody[gen.Placement](t, rec)
			if body.Replayed != tc.replayed {
				t.Errorf("replayed = %v, want %v", body.Replayed, tc.replayed)
			}
			if len(body.Wagers) != 1 {
				t.Fatalf("wagers = %d, want 1", len(body.Wagers))
			}
			// The totals are folded over the tickets in this very response, so
			// a client can check them against the rows beside them.
			if body.TotalStakeMinor != 2500 {
				t.Errorf("total_stake_minor = %d, want 2500", body.TotalStakeMinor)
			}
			if body.PotentialProfitMinor != body.PotentialPayoutMinor-body.TotalStakeMinor {
				t.Errorf("profit %d is not payout %d minus stake %d",
					body.PotentialProfitMinor, body.PotentialPayoutMinor, body.TotalStakeMinor)
			}
		})
	}
}

// TestPlaceWagerParsesTheSlipFaithfully.
//
// The fields that matter are the ones a careless mapping would drop silently: a
// seen LINE (absent and present-zero are different bets), an acceptance (which
// must carry both halves or it is consent to an unstated handicap), and the
// rounding mode, which the caller must not be able to name.
func TestPlaceWagerParsesTheSlipFaithfully(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	d.betting.placement = betting.Placement{Wagers: []domain.Wager{testStraight(t, "wgr_1", 2500)}}

	body := `{"kind":"parlay","stake_minor":2500,"seen_ticket_decimal":3.7,
		"accept_better_price":true,
		"legs":[
			{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91,"seen_line":0},
			{"selection_id":"sel_lal","book_id":"bk_soft","seen_decimal":1.95,
			 "accepted_decimal":1.97,"accepted_line":-3.5}]}`

	rec := place(t, d, body, "key-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	got := d.betting.last
	if got.IdempotencyKey != "key-1" {
		t.Errorf("idempotency key = %q, want key-1", got.IdempotencyKey)
	}
	if got.UserID != testUser {
		t.Errorf("user = %q, want %q", got.UserID, testUser)
	}
	if got.Slip.Rounding != wagerRounding {
		t.Errorf("rounding = %v, want %v (house policy, never the caller's)", got.Slip.Rounding, wagerRounding)
	}
	if !got.Slip.AcceptBetterPrice {
		t.Error("accept_better_price did not reach the slip")
	}
	if got.Slip.SeenTicketDecimal != 3.7 {
		t.Errorf("seen ticket decimal = %v, want 3.7", got.Slip.SeenTicketDecimal)
	}
	if len(got.Slip.Legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(got.Slip.Legs))
	}

	// A seen line of 0 is a traded pick'em and MUST survive as present. If it
	// arrived as domain.NoLine() the placement path would compare a pick'em
	// against a market with no line and wave through a bet on a different
	// handicap.
	line, present := got.Slip.Legs[0].SeenLine.Value()
	if !present || line != 0 {
		t.Errorf("leg 0 seen line = %v present=%v, want a present 0.0 pick'em", line, present)
	}
	if got.Slip.Legs[0].Accept != nil {
		t.Error("leg 0 carried an acceptance it never sent")
	}

	accept := got.Slip.Legs[1].Accept
	if accept == nil {
		t.Fatal("leg 1 lost its acceptance")
	}
	if accept.Decimal != 1.97 {
		t.Errorf("accepted decimal = %v, want 1.97", accept.Decimal)
	}
	if v, ok := accept.Line.Value(); !ok || v != -3.5 {
		t.Errorf("accepted line = %v present=%v, want -3.5", v, ok)
	}
}

// TestPlaceWagerRefusesAnAcceptedLineWithNoAcceptedPrice.
//
// An acceptance is consent to a SPECIFIC re-quote and both halves are required.
// A line with no price would be consent to an unstated number, and honouring it
// would let a client move the customer's bet by sending half a field.
func TestPlaceWagerRefusesAnAcceptedLineWithNoAcceptedPrice(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	body := `{"kind":"straight","stake_minor":2500,"legs":[
		{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91,"accepted_line":-3.5}]}`

	rec := place(t, d, body, "key-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if d.betting.calls != 0 {
		t.Error("a half-formed acceptance reached the placement service")
	}
}

// TestPlaceWagerSentinelMapping is the table this layer exists to get right.
//
// Two of these deliberately diverge from the mapping internal/betting/errors.go
// proposes in its own header, and the reasons are in [API.failBetting]:
// ErrInvalidSlip is 422 rather than 400 because this API reserves 400 for a
// request it could not understand, and ErrLimitExceeded is 422 rather than 403
// because 403 here means a standing condition on the account rather than a
// ceiling on one slip.
func TestPlaceWagerSentinelMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   gen.ErrorCode
	}{
		{"self-excluded", betting.ErrSelfExcluded, http.StatusForbidden, gen.ErrorCodeSelfExcluded},
		{"suspended or closed", betting.ErrAccountNotWagerable, http.StatusForbidden, gen.ErrorCodeAccountNotActive},
		{"insufficient funds", betting.ErrInsufficientFunds, http.StatusUnprocessableEntity, gen.ErrorCodeInsufficientFunds},
		{"limit exceeded", betting.ErrLimitExceeded, http.StatusUnprocessableEntity, gen.ErrorCodeLimitExceeded},
		{"market closed", betting.ErrMarketNotOpen, http.StatusConflict, gen.ErrorCodeMarketUnavailable},
		{"event started", betting.ErrEventStarted, http.StatusConflict, gen.ErrorCodeMarketUnavailable},
		{"stale quote", betting.ErrStaleQuote, http.StatusConflict, gen.ErrorCodeMarketUnavailable},
		{"no quote", betting.ErrQuoteUnavailable, http.StatusConflict, gen.ErrorCodeMarketUnavailable},
		{"price moved", betting.ErrPriceMoved, http.StatusConflict, gen.ErrorCodePriceMoved},
		{"accepted price gone", betting.ErrPriceMovedNotAccepted, http.StatusConflict, gen.ErrorCodePriceMoved},
		{"cash-out unavailable", betting.ErrCashOutUnavailable, http.StatusConflict, gen.ErrorCodeCashOutUnavailable},
		{"duplicate market", betting.ErrDuplicateMarket, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable},
		{"leg count", betting.ErrLegCountForKind, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable},
		{"same game", betting.ErrSameGameUnsupported, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable},
		{"teaser unsupported", betting.ErrTeaserUnsupported, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable},
		{"wager not found", betting.ErrWagerNotFound, http.StatusNotFound, gen.ErrorCodeNotFound},
		{"unknown", errors.New("something else entirely"), http.StatusInternalServerError, gen.ErrorCodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDeps()
			seedBoard(t, d)
			d.betting.err = fmt.Errorf("place: %w", tc.err)

			rec := place(t, d, placeBody("sel_bos", 1.91, 2500), "key-1")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := decodeJSONBody[gen.Error](t, rec)
			if body.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", body.Code, tc.wantCode)
			}
			// respond.go's rule, restated on the path that most nearly breaks
			// it: nothing derived from an error value reaches the wire.
			if strings.Contains(body.Message, "something else entirely") ||
				strings.Contains(body.Message, "betting:") {
				t.Errorf("message %q leaks the underlying error", body.Message)
			}
		})
	}
}

// TestPriceMovedCarriesBothNumbers.
//
// The client has to show the customer exactly what changed. A 409 with only a
// code would make it re-quote to find out what moved — racing the next move, and
// showing a third number that was never the reason the bet was refused.
func TestPriceMovedCarriesBothNumbers(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	d.betting.err = fmt.Errorf("place: %w", &betting.PriceMove{
		SelectionID:    "sel_bos",
		BookID:         "bk_sharp",
		SeenDecimal:    1.91,
		SeenLine:       "none",
		CurrentDecimal: 1.87,
		CurrentLine:    "-3.5",
		Improved:       false,
		Accepted:       true,
	})

	rec := place(t, d, placeBody("sel_bos", 1.91, 2500), "key-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSONBody[gen.Error](t, rec)
	if body.Code != gen.ErrorCodePriceMoved {
		t.Fatalf("code = %q, want price_moved", body.Code)
	}
	if body.PriceMoves == nil || len(*body.PriceMoves) != 1 {
		t.Fatalf("price_moves missing: %s", rec.Body.String())
	}

	move := (*body.PriceMoves)[0]
	if move.Scope != gen.Leg {
		t.Errorf("scope = %q, want leg", move.Scope)
	}
	if move.SeenDecimal == nil || *move.SeenDecimal != 1.91 {
		t.Errorf("seen_decimal = %v, want 1.91", move.SeenDecimal)
	}
	if move.CurrentDecimal == nil || *move.CurrentDecimal != 1.87 {
		t.Errorf("current_decimal = %v, want 1.87", move.CurrentDecimal)
	}
	if move.Movement != gen.Shortened {
		t.Errorf("movement = %q, want shortened", move.Movement)
	}
	// "none" is domain.Line's rendering of an absent line and must come back as
	// JSON null, not as a number. A present -3.5 must come back as -3.5.
	if move.SeenLine != nil {
		t.Errorf("seen_line = %v, want null for an absent line", *move.SeenLine)
	}
	if move.CurrentLine == nil || *move.CurrentLine != -3.5 {
		t.Errorf("current_line = %v, want -3.5", move.CurrentLine)
	}
	if move.Accepted == nil || !*move.Accepted {
		t.Error("accepted flag lost; the client cannot tell a first re-quote from a second")
	}
}

// TestLimitBreachNamesTheLimit.
//
// A customer told only "no" retries the wrong fix. The kind and the period come
// from server-side enums, never from the request, so naming them reflects
// nothing client-controlled.
func TestLimitBreachNamesTheLimit(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	d.betting.err = fmt.Errorf("place: %w", &betting.LimitBreach{
		Kind: auth.LimitKindStake.String(), Period: auth.LimitPeriodDay.String(),
		Limit: 20000, Used: 18000, Requested: 2500,
	})

	rec := place(t, d, placeBody("sel_bos", 1.91, 2500), "key-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	body := decodeJSONBody[gen.Error](t, rec)
	if body.Code != gen.ErrorCodeLimitExceeded {
		t.Fatalf("code = %q, want limit_exceeded", body.Code)
	}
	if body.InvalidParams == nil || len(*body.InvalidParams) != 1 {
		t.Fatalf("invalid_params missing: %s", rec.Body.String())
	}
	reason := (*body.InvalidParams)[0].Reason
	if !strings.Contains(reason, "stake") || !strings.Contains(reason, "day") {
		t.Errorf("reason %q names neither the limit kind nor its period", reason)
	}
	// The amounts belong on the account screen, which reads the limit and the
	// ledger directly. Putting them in a rejection makes the rejection a second
	// place they are computed.
	if strings.Contains(reason, "18000") || strings.Contains(reason, "20000") {
		t.Errorf("reason %q carries amounts; those belong on the account screen", reason)
	}
}

// -----------------------------------------------------------------------------
// History
// -----------------------------------------------------------------------------

// TestGetWagerHidesAnotherCustomersTicket.
//
// 404 and not 403, and from the SAME branch as a missing row. A 403 would
// confirm the id exists, which is a wager-enumeration oracle over every customer
// of the book — so the two responses must be byte-identical.
func TestGetWagerHidesAnotherCustomersTicket(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	theirs := testStraight(t, "wgr_theirs", 2500)
	d.wagers.wagers = []Wager{wagerFromDomain(theirs)}
	d.wagers.wagers[0].UserID = domain.UserID("usr_somebody_else")

	notMine := callAuthed(t, d, http.MethodGet, "/v1/wagers/wgr_theirs", "", nil)
	missing := callAuthed(t, d, http.MethodGet, "/v1/wagers/wgr_nothing", "", nil)

	if notMine.Code != http.StatusNotFound {
		t.Fatalf("another customer's wager = %d, want 404", notMine.Code)
	}
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing wager = %d, want 404", missing.Code)
	}

	// Both bodies carry a request id, which differs per request, so the
	// comparison is on everything else.
	a := decodeJSONBody[gen.Error](t, notMine)
	b := decodeJSONBody[gen.Error](t, missing)
	if a.Code != b.Code || a.Message != b.Message {
		t.Errorf("the two 404s differ (%q/%q vs %q/%q); that is an enumeration oracle",
			a.Code, a.Message, b.Code, b.Message)
	}
}

// TestListWagersFiltersByStatus.
//
// An unknown status is a 400 rather than an empty page: a typo that quietly
// returns nothing is indistinguishable from "you have no wagers", which is the
// one answer a customer must never be given wrongly.
func TestListWagersFiltersByStatus(t *testing.T) {
	t.Parallel()

	open := wagerFromDomain(testStraight(t, "wgr_open", 2500))
	settled := wagerFromDomain(testStraight(t, "wgr_done", 2500))
	settled.Status = domain.WagerStatusLost
	settled.PlacedAt = open.PlacedAt.Add(-time.Minute)

	cases := []struct {
		name       string
		query      string
		wantStatus int
		wantIDs    []string
	}{
		{"no filter", "", http.StatusOK, []string{"wgr_open", "wgr_done"}},
		{"one status", "?status=lost", http.StatusOK, []string{"wgr_done"}},
		{"union", "?status=placed&status=lost", http.StatusOK, []string{"wgr_open", "wgr_done"}},
		{"unknown status", "?status=nonsense", http.StatusBadRequest, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDeps()
			seedBoard(t, d)
			d.wagers.wagers = []Wager{open, settled}

			rec := callAuthed(t, d, http.MethodGet, "/v1/wagers"+tc.query, "", nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			page := decodeJSONBody[gen.WagerPage](t, rec)
			got := make([]string, 0, len(page.Data))
			for _, w := range page.Data {
				got = append(got, w.Id)
			}
			if strings.Join(got, ",") != strings.Join(tc.wantIDs, ",") {
				t.Errorf("ids = %v, want %v", got, tc.wantIDs)
			}
		})
	}
}

// TestWagerCursorRoundTripsAndRefusesAnotherQuery.
//
// The scope fingerprint is what stops a client that changes its status filter
// mid-page from silently receiving a consistently-ordered page of a DIFFERENT
// set. Without it the failure is invisible: every page looks fine.
func TestWagerCursorRoundTripsAndRefusesAnotherQuery(t *testing.T) {
	t.Parallel()

	key := WagerKey{PlacedAt: testNow.Add(-time.Hour), ID: domain.WagerID("wgr_1")}
	scope := cursorScope("wagers", string(testUser), "placed")
	other := cursorScope("wagers", string(testUser), "lost")

	encoded := encodeWagerCursor(key, scope)

	got, err := decodeWagerCursor(encoded, scope)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !got.PlacedAt.Equal(key.PlacedAt) || got.ID != key.ID {
		t.Errorf("decoded %v, want %v", got, key)
	}

	if _, err := decodeWagerCursor(encoded, other); !errors.Is(err, ErrBadCursor) {
		t.Errorf("a cursor presented against a different filter decoded: %v", err)
	}
	if _, err := decodeWagerCursor("not base64!!", scope); !errors.Is(err, ErrBadCursor) {
		t.Errorf("garbage decoded: %v", err)
	}
	if _, err := decodeWagerCursor(strings.Repeat("a", maxCursorLen+1), scope); !errors.Is(err, ErrBadCursor) {
		t.Errorf("an over-long cursor decoded: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Cash-out
// -----------------------------------------------------------------------------

// TestCashOutQuoteShowsTheTake.
//
// The entire reason to quote off the fair value and subtract a NAMED haircut is
// that the take is a number the customer can read. If margin_minor were absent
// the money would be taken all the same and none of the benefit would remain.
func TestCashOutQuoteShowsTheTake(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	w := wagerFromDomain(testStraight(t, "wgr_1", 2500))
	d.wagers.wagers = []Wager{w}
	d.cashOuts.quote = betting.CashOutQuote{
		Value:               mustMoney(t, 2185),
		FairValue:           mustMoney(t, 2300),
		MarginBps:           betting.DefaultCashOutMarginBps,
		SurvivalProbability: 0.482,
		QuotedAt:            testNow,
	}

	rec := callAuthed(t, d, http.MethodGet, "/v1/wagers/wgr_1/cashout", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	q := decodeJSONBody[gen.CashOutQuote](t, rec)

	if q.MarginBps != betting.DefaultCashOutMarginBps {
		t.Errorf("margin_bps = %d, want %d", q.MarginBps, betting.DefaultCashOutMarginBps)
	}
	if q.MarginMinor != q.FairValueMinor-q.ValueMinor {
		t.Errorf("margin %d is not fair %d minus value %d",
			q.MarginMinor, q.FairValueMinor, q.ValueMinor)
	}
	if q.NetReturnMinor != q.ValueMinor-q.StakeMinor {
		t.Errorf("net %d is not value %d minus stake %d",
			q.NetReturnMinor, q.ValueMinor, q.StakeMinor)
	}
	if q.PendingLegCount != 1 {
		t.Errorf("pending_leg_count = %d, want 1", q.PendingLegCount)
	}
}

// TestCashOutRefusesATerminalWager, before pricing rather than after.
func TestCashOutRefusesATerminalWager(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	w := wagerFromDomain(testStraight(t, "wgr_1", 2500))
	w.Status = domain.WagerStatusWon
	returned := mustMoney(t, 4775)
	net := mustMoney(t, 2275)
	w.Returned, w.NetReturn = &returned, &net
	d.wagers.wagers = []Wager{w}

	rec := callAuthed(t, d, http.MethodGet, "/v1/wagers/wgr_1/cashout", "", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if decodeJSONBody[gen.Error](t, rec).Code != gen.ErrorCodeCashOutUnavailable {
		t.Error("a terminal wager must refuse with cash_out_unavailable")
	}
}

// TestTakeCashOutRequiresAKeyAndAnAcceptedValue.
func TestTakeCashOutRequiresAKeyAndAnAcceptedValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		key  string
		body string
		want int
	}{
		{"no key", "", `{"accepted_value_minor":2185}`, http.StatusBadRequest},
		{"zero value", "key-1", `{"accepted_value_minor":0}`, http.StatusUnprocessableEntity},
		{"negative value", "key-1", `{"accepted_value_minor":-1}`, http.StatusUnprocessableEntity},
		{"good", "key-1", `{"accepted_value_minor":2185}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDeps()
			seedBoard(t, d)
			d.wagers.wagers = []Wager{wagerFromDomain(testStraight(t, "wgr_1", 2500))}
			d.cashOuts.settled = testStraight(t, "wgr_1", 2500)

			headers := map[string]string{}
			if tc.key != "" {
				headers[idempotencyHeader] = tc.key
			}
			rec := callAuthed(t, d, http.MethodPost, "/v1/wagers/wgr_1/cashout", tc.body, headers)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK && d.cashOuts.takes != 0 {
				t.Error("a rejected request still reached the cash-out executor")
			}
		})
	}
}

// TestTakeCashOutRefusesAnotherCustomersTicket.
//
// The take port is keyed by wager id alone, so the ownership read is the ONLY
// thing standing between a caller and settling somebody else's ticket. It is
// load-bearing rather than defensive, and it must happen before the executor is
// reached.
func TestTakeCashOutRefusesAnotherCustomersTicket(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	theirs := wagerFromDomain(testStraight(t, "wgr_theirs", 2500))
	theirs.UserID = domain.UserID("usr_somebody_else")
	d.wagers.wagers = []Wager{theirs}

	rec := callAuthed(t, d, http.MethodPost, "/v1/wagers/wgr_theirs/cashout",
		`{"accepted_value_minor":2185}`, map[string]string{idempotencyHeader: "key-1"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if d.cashOuts.takes != 0 {
		t.Fatal("the cash-out executor was reached for a ticket the caller does not own")
	}
}

// -----------------------------------------------------------------------------
// The wire
// -----------------------------------------------------------------------------

// TestEveryMoneyFieldIsAnInteger.
//
// CLAUDE.md §12: "All money and stake values are integer minor units. Floating
// point never touches a balance." The spec bounds every one of them to 2^53-1 so
// a JSON number is lossless — but only if it is a JSON INTEGER. A `2500.0`
// serialised anywhere on this surface would parse back as a float in every
// client and would be the first step to arithmetic in major units.
//
// The check is on the raw bytes rather than on a decoded struct, because Go's
// int64 fields would happily absorb a float the encoder produced.
func TestEveryMoneyFieldIsAnInteger(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedBoard(t, d)
	d.betting.placement = betting.Placement{Wagers: []domain.Wager{testStraight(t, "wgr_1", 2500)}}

	rec := place(t, d, placeBody("sel_bos", 1.91, 2500), "key-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	assertMinorFieldsAreIntegers(t, rec.Body.Bytes())
}

// assertMinorFieldsAreIntegers walks a JSON document and fails on any `*_minor`
// key whose value is not an integer literal.
func assertMinorFieldsAreIntegers(t *testing.T, body []byte) {
	t.Helper()

	// json.Number preserves the literal, which is the only way to tell 2500
	// from 2500.0 after decoding.
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var typed any
	if err := dec.Decode(&typed); err != nil {
		t.Fatalf("decode with numbers: %v", err)
	}

	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch v := node.(type) {
		case map[string]any:
			for key, child := range v {
				next := path + "." + key
				if strings.HasSuffix(key, "_minor") {
					num, ok := child.(json.Number)
					if !ok {
						if child == nil {
							continue // an explicit null is a legal absent amount
						}
						t.Errorf("%s is %T, want an integer", next, child)
						continue
					}
					if strings.ContainsAny(num.String(), ".eE") {
						t.Errorf("%s = %s, want an integer number of minor units", next, num)
					}
				}
				walk(child, next)
			}
		case []any:
			for i, child := range v {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(typed, "")
}

// -----------------------------------------------------------------------------
// The slip quote
// -----------------------------------------------------------------------------

// seedSlip adds the account state a quote needs on top of [seedBoard]: an active
// profile and a cash balance. Both are read on the quote path and neither is on
// the board path, so they live here rather than in the shared fixture.
func seedSlip(t *testing.T, d *deps, cashMinor int64) {
	t.Helper()
	seedBoard(t, d)
	d.accounts.profiles[testUser] = Profile{
		ID: testUser, Email: "sharp@example.test",
		Status: auth.UserStatusActive, CreatedAt: testNow.Add(-24 * time.Hour),
	}
	d.ledger.balances = []Balance{
		{Kind: domain.AccountKindUserCash, Amount: mustMoney(t, cashMinor), Entries: 3},
		{Kind: domain.AccountKindUserEscrow, Amount: domain.ZeroMoney},
	}
}

func quoteSlip(t *testing.T, d *deps, body string) *httptest.ResponseRecorder {
	t.Helper()
	return callAuthed(t, d, http.MethodPost, "/v1/slip/quote", body, nil)
}

// TestQuoteSlipPricesAStraightAndWritesNothing.
//
// The quote is a pure read: it must reach no writer at all. That is asserted
// directly, because the endpoint is called on every keystroke of a slip edit and
// a write on that path would be a wager row per character.
func TestQuoteSlipPricesAStraightAndWritesNothing(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedSlip(t, d, 50000)

	rec := quoteSlip(t, d, `{"kind":"straight","stake_minor":2500,"legs":[
		{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	q := decodeJSONBody[gen.SlipQuote](t, rec)
	if q.DecimalOdds == nil || *q.DecimalOdds != 1.91 {
		t.Fatalf("decimal_odds = %v, want the leg's own price on a straight", q.DecimalOdds)
	}
	if q.TicketCount != 1 || q.TotalStakeMinor != 2500 {
		t.Errorf("ticket_count/total = %d/%d, want 1/2500", q.TicketCount, q.TotalStakeMinor)
	}
	// 2500 x 1.91 = 4775 exactly, so the rounding mode is not doing any work
	// here — which is the point: an assertion that depended on the mode would
	// be asserting the mode rather than the arithmetic.
	if q.PotentialPayoutMinor != 4775 {
		t.Errorf("potential_payout_minor = %d, want 4775", q.PotentialPayoutMinor)
	}
	if q.PotentialProfitMinor != 4775-2500 {
		t.Errorf("potential_profit_minor = %d, want %d", q.PotentialProfitMinor, 4775-2500)
	}
	if q.Rounding != gen.Rounding(wagerRounding.String()) {
		t.Errorf("rounding = %q, want the house policy reported", q.Rounding)
	}
	if !q.Placeable || len(q.Impediments) != 0 {
		t.Errorf("placeable = %v with impediments %+v, want a clean slip", q.Placeable, q.Impediments)
	}
	if q.CashBalanceMinor != 50000 {
		t.Errorf("cash_balance_minor = %d, want the CASH fold only (never cash+escrow)", q.CashBalanceMinor)
	}
	if len(q.Legs) != 1 || q.Legs[0].Movement != gen.Unchanged || q.Legs[0].CurrentDecimal != 1.91 {
		t.Errorf("leg = %+v, want one unchanged leg at 1.91", q.Legs)
	}
	if !q.Legs[0].Tradeable {
		t.Error("an open market on a scheduled event with a fresh quote is not tradeable")
	}

	if d.betting.calls != 0 {
		t.Errorf("quoting called the placement service %d times; it must write nothing", d.betting.calls)
	}
	assertMinorFieldsAreIntegers(t, rec.Body.Bytes())
}

// TestQuoteSlipReportsMovementRatherThanRefusing.
//
// A quote's whole job is to describe the current state, so it must not fail when
// the state is interesting. Placement is the endpoint that refuses.
func TestQuoteSlipReportsMovementRatherThanRefusing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		seen         float64
		wantMovement gen.PriceMovement
	}{
		{"unchanged", 1.91, gen.Unchanged},
		{"lengthened", 1.80, gen.Lengthened},
		{"shortened", 2.05, gen.Shortened},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDeps()
			seedSlip(t, d, 50000)

			body := fmt.Sprintf(`{"kind":"straight","stake_minor":2500,"legs":[
				{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":%v}]}`, tc.seen)
			rec := quoteSlip(t, d, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}

			q := decodeJSONBody[gen.SlipQuote](t, rec)
			if q.Legs[0].Movement != tc.wantMovement {
				t.Errorf("movement = %q, want %q", q.Legs[0].Movement, tc.wantMovement)
			}
			moved := tc.wantMovement != gen.Unchanged
			if q.PriceMoved != moved {
				t.Errorf("price_moved = %v, want %v", q.PriceMoved, moved)
			}
			// A moved price is an impediment because the customer has not agreed
			// to the new number; the button is disabled until they do.
			if q.Placeable == moved {
				t.Errorf("placeable = %v with price_moved = %v", q.Placeable, moved)
			}
		})
	}
}

// TestQuoteSlipReportsAccountAndBalanceImpediments.
//
// All of these are ADVISORY — internal/betting re-evaluates each inside the
// placement transaction — so what is asserted is that the right code reaches the
// client, not that the check binds.
func TestQuoteSlipReportsAccountAndBalanceImpediments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status auth.UserStatus
		cash   int64
		want   gen.SlipImpedimentCode
	}{
		{"self-excluded", auth.UserStatusSelfExcluded, 50000, gen.SlipImpedimentCodeSelfExcluded},
		{"suspended", auth.UserStatusSuspended, 50000, gen.SlipImpedimentCodeAccountNotActive},
		{"closed", auth.UserStatusClosed, 50000, gen.SlipImpedimentCodeAccountNotActive},
		{"broke", auth.UserStatusActive, 100, gen.SlipImpedimentCodeInsufficientFunds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDeps()
			seedSlip(t, d, tc.cash)
			profile := d.accounts.profiles[testUser]
			profile.Status = tc.status
			d.accounts.profiles[testUser] = profile

			rec := quoteSlip(t, d, `{"kind":"straight","stake_minor":2500,"legs":[
				{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91}]}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}

			q := decodeJSONBody[gen.SlipQuote](t, rec)
			if q.Placeable {
				t.Error("placeable = true with an impediment")
			}
			found := false
			for _, imp := range q.Impediments {
				if imp.Code == tc.want {
					found = true
				}
			}
			if !found {
				t.Errorf("impediments %+v do not carry %q", q.Impediments, tc.want)
			}
		})
	}
}

// TestQuoteSlipNeverReportsALimitImpediment.
//
// Stated as a test because it is a decision, not an omission: evaluating a
// self-imposed limit is a period-scoped sum over the ledger taken under the
// placement lock, and a second evaluation on a read path would be a second
// answer to a responsible-gaming control. If a `limit_exceeded` code ever
// appears here, somebody has added that second evaluator.
func TestQuoteSlipNeverReportsALimitImpediment(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"limit_exceeded"} {
		if gen.SlipImpedimentCode(code).Valid() {
			t.Errorf("SlipImpediment.code admits %q; the quote must not evaluate limits", code)
		}
	}
}

// TestQuoteSlipRefusesWhatItCannotPriceCorrectly.
//
// A wrong ticket price is frozen into a row the schema then makes immutable, so
// it is wrong forever and wrong in the direction nobody audits. Refusing is the
// correct failure for both shapes.
func TestQuoteSlipRefusesWhatItCannotPriceCorrectly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{
			"same-game parlay",
			`{"kind":"parlay","stake_minor":2500,"legs":[
				{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91},
				{"selection_id":"sel_lal","book_id":"bk_sharp","seen_decimal":1.95}]}`,
		},
		{
			"teaser",
			`{"kind":"teaser","stake_minor":2500,"teaser_points":6,"legs":[
				{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91,"seen_line":-3.5},
				{"selection_id":"sel_lal","book_id":"bk_sharp","seen_decimal":1.95,"seen_line":3.5}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDeps()
			seedSlip(t, d, 50000)

			rec := quoteSlip(t, d, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
			}
			if decodeJSONBody[gen.Error](t, rec).Code != gen.ErrorCodeUnprocessable {
				t.Error("a shape this book will not price must be unprocessable")
			}
		})
	}
}

// TestQuoteSlipRefusesAnUnquotedLeg.
//
// A partial quote would invite the client to render a payout for a bet it cannot
// place. There is no price, so there is no quote — 409, the same code placement
// answers, so a client has one branch.
func TestQuoteSlipRefusesAnUnquotedLeg(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedSlip(t, d, 50000)

	// bk_soft quotes sel_bos in the fixture; bk_missing quotes nothing.
	rec := quoteSlip(t, d, `{"kind":"straight","stake_minor":2500,"legs":[
		{"selection_id":"sel_bos","book_id":"bk_missing","seen_decimal":1.91}]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if decodeJSONBody[gen.Error](t, rec).Code != gen.ErrorCodeMarketUnavailable {
		t.Error("a leg no book is quoting must be market_unavailable")
	}
}

// TestQuoteSlipRefusesAnUnknownSelection with 404, not 409: the client named
// something that does not exist, which is a different fix from "re-quote".
func TestQuoteSlipRefusesAnUnknownSelection(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedSlip(t, d, 50000)

	rec := quoteSlip(t, d, `{"kind":"straight","stake_minor":2500,"legs":[
		{"selection_id":"sel_nothing","book_id":"bk_sharp","seen_decimal":1.91}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestQuoteSlipMarksAClosedMarketUntradeable.
func TestQuoteSlipMarksAClosedMarketUntradeable(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedSlip(t, d, 50000)
	d.catalogue.markets[0].Status = domain.MarketStatusSuspended

	rec := quoteSlip(t, d, `{"kind":"straight","stake_minor":2500,"legs":[
		{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	q := decodeJSONBody[gen.SlipQuote](t, rec)
	if q.Legs[0].Tradeable {
		t.Error("a suspended market is tradeable")
	}
	if q.Placeable {
		t.Error("a slip with an untradeable leg is placeable")
	}
	if len(q.Impediments) == 0 || q.Impediments[0].Code != gen.SlipImpedimentCodeMarketUnavailable {
		t.Errorf("impediments = %+v, want market_unavailable naming the leg", q.Impediments)
	}
	if q.Impediments[0].SelectionId == nil {
		t.Error("a per-leg impediment must name its selection")
	}
}

// TestQuoteSlipRejectsAMalformedSlipBeforeReadingAnything.
//
// betting.Slip.Validate is the SAME pure validator the placement path runs, and
// it runs first so a malformed slip does no I/O. What is asserted is that its
// refusals reach the client attributed to a field.
func TestQuoteSlipRejectsAMalformedSlipBeforeReadingAnything(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		body      string
		wantParam string
	}{
		{
			"straight with two legs",
			`{"kind":"straight","stake_minor":2500,"legs":[
				{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91},
				{"selection_id":"sel_lal","book_id":"bk_sharp","seen_decimal":1.95}]}`,
			"legs",
		},
		{
			"duplicate selection",
			`{"kind":"parlay","stake_minor":2500,"legs":[
				{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91},
				{"selection_id":"sel_bos","book_id":"bk_soft","seen_decimal":1.95}]}`,
			"legs",
		},
		{
			"zero stake",
			`{"kind":"straight","stake_minor":0,"legs":[
				{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91}]}`,
			"stake_minor",
		},
		{
			"teaser points on a straight",
			`{"kind":"straight","stake_minor":2500,"teaser_points":6,"legs":[
				{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91}]}`,
			"teaser_points",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newDeps()
			seedSlip(t, d, 50000)

			rec := quoteSlip(t, d, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
			}
			body := decodeJSONBody[gen.Error](t, rec)
			if body.InvalidParams == nil {
				t.Fatalf("no invalid_params: %s", rec.Body.String())
			}
			named := false
			for _, p := range *body.InvalidParams {
				if p.Name == tc.wantParam {
					named = true
				}
			}
			if !named {
				t.Errorf("invalid_params %+v does not name %q", *body.InvalidParams, tc.wantParam)
			}
		})
	}
}

// TestQuoteSlipRejectsAnUnknownBodyField.
//
// Every request schema declares additionalProperties: false, and decodeJSON has
// DisallowUnknownFields on. A server that ignored an extra field would let a
// client believe it had set something it had not — the failure mode where a
// customer "accepts" a moved price through a misspelled key.
func TestQuoteSlipRejectsAnUnknownBodyField(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedSlip(t, d, 50000)

	rec := quoteSlip(t, d, `{"kind":"straight","stake_minor":2500,"accept_better_price":true,"legs":[
		{"selection_id":"sel_bos","book_id":"bk_sharp","seen_decimal":1.91}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// -----------------------------------------------------------------------------
// Round robins
// -----------------------------------------------------------------------------

// seedGames adds n further single-selection games on top of [seedSlip], so a
// slip can carry legs on DIFFERENT events.
//
// It exists because the shipped pricer refuses a same-game parlay rather than
// pricing correlated legs as independent, so every multi-leg fixture in this
// file has to span events. That refusal is the behaviour under test elsewhere;
// here it is a constraint on the fixture.
func seedGames(t *testing.T, d *deps, n int) []string {
	t.Helper()

	sharp := mustBookID(t, "bk_sharp")
	ids := make([]string, 0, n)
	for i := range n {
		event := fmt.Sprintf("evt_extra_%d", i)
		market := fmt.Sprintf("mkt_extra_%d", i)
		selection := fmt.Sprintf("sel_extra_%d", i)
		ids = append(ids, selection)

		d.catalogue.events = append(d.catalogue.events, Event{
			ID:             mustEventID(t, event),
			LeagueID:       d.catalogue.leagues[0].ID,
			Kind:           domain.EventKindMatch,
			Name:           fmt.Sprintf("Extra Game %d", i),
			ScheduledStart: testNow.Add(3 * time.Hour),
			Status:         domain.EventStatusScheduled,
			ObservedAt:     testNow.Add(-time.Minute),
		})
		d.catalogue.markets = append(d.catalogue.markets, Market{
			ID: mustMarketID(t, market), EventID: mustEventID(t, event),
			Type: domain.MarketTypeMoneyline, Status: domain.MarketStatusOpen,
			ObservedAt: testNow.Add(-time.Minute),
		})
		d.catalogue.selections = append(d.catalogue.selections, Selection{
			ID: mustSelectionID(t, selection), MarketID: mustMarketID(t, market),
			MarketType: domain.MarketTypeMoneyline, Role: domain.SelectionRoleHome,
			Name: fmt.Sprintf("Home %d", i),
		})
		d.prices.quotes = append(d.prices.quotes, Quote{
			SelectionID: mustSelectionID(t, selection), BookID: sharp,
			Odds:       mustDecimal(t, 2.0),
			ObservedAt: testNow.Add(-30 * time.Second), IngestedAt: testNow.Add(-29 * time.Second),
		})
	}
	return ids
}

// TestQuoteSlipPricesEveryCombinationOfARoundRobin.
//
// A round robin is N INDEPENDENT TICKETS, and the two properties that follow
// from that are both asserted here:
//
//   - `stake_minor` is the stake on EACH ticket and `total_stake_minor` is what
//     the customer actually risks. Quoting the per-ticket figure as the total is
//     how a customer places a $30 bet believing it is a $5 one.
//   - There is NO single ticket price, so `decimal_odds` is null. A headline
//     number would be an average nobody is offered.
func TestQuoteSlipPricesEveryCombinationOfARoundRobin(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedSlip(t, d, 100000)
	ids := seedGames(t, d, 3)

	legs := make([]string, 0, len(ids))
	for _, id := range ids {
		legs = append(legs, fmt.Sprintf(
			`{"selection_id":%q,"book_id":"bk_sharp","seen_decimal":2.0}`, id))
	}
	body := fmt.Sprintf(`{"kind":"round_robin","stake_minor":500,"round_robin_sizes":[2],"legs":[%s]}`,
		strings.Join(legs, ","))

	rec := quoteSlip(t, d, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	q := decodeJSONBody[gen.SlipQuote](t, rec)

	// C(3,2) = 3 combinations.
	if q.TicketCount != 3 {
		t.Errorf("ticket_count = %d, want 3", q.TicketCount)
	}
	if q.StakeMinor != 500 {
		t.Errorf("stake_minor = %d, want the PER-TICKET stake 500", q.StakeMinor)
	}
	if q.TotalStakeMinor != 1500 {
		t.Errorf("total_stake_minor = %d, want 3 x 500", q.TotalStakeMinor)
	}
	if q.DecimalOdds != nil {
		t.Errorf("decimal_odds = %v, want null: a round robin has no single ticket price", *q.DecimalOdds)
	}
	// Each two-leg combination at 2.0 x 2.0 = 4.0 returns 500 x 4 = 2000.
	if q.PotentialPayoutMinor != 6000 {
		t.Errorf("potential_payout_minor = %d, want 3 x 2000", q.PotentialPayoutMinor)
	}
	if q.PotentialProfitMinor != 6000-1500 {
		t.Errorf("potential_profit_minor = %d, want payout minus TOTAL stake", q.PotentialProfitMinor)
	}
	if q.IsSameGame == nil || *q.IsSameGame {
		t.Error("is_same_game is true for legs on three different events")
	}
}

// TestPlacementReportsTheRoundRobinParent.
//
// The parent's own selection set is deliberately absent from the wire: every one
// of its selections appears on at least one ticket in the same response, and a
// second copy could only ever disagree with the tickets it supposedly generated.
func TestPlacementReportsTheRoundRobinParent(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedSlip(t, d, 100000)
	seedGames(t, d, 2)

	legs := []domain.Leg{
		testLeg(t, "leg_a", "sel_extra_0", 2.0),
		testLeg(t, "leg_b", "sel_extra_1", 2.0),
	}
	// The two legs must sit on different events or the domain reports the ticket
	// as same-game; testLeg pins one event, so the second is rebuilt here.
	legs[1] = testLegOnEvent(t, "leg_b", "sel_extra_1", "evt_extra_1", "mkt_extra_1", 2.0)

	rrID, err := domain.NewRoundRobinID("rr_1")
	if err != nil {
		t.Fatalf("round robin id: %v", err)
	}
	rr, err := domain.NewRoundRobin(domain.RoundRobinParams{
		ID:                  rrID,
		UserID:              testUser,
		Legs:                legs,
		Sizes:               []int{2},
		StakePerCombination: mustMoney(t, 500),
		PlacedAt:            testNow.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("round robin: %v", err)
	}

	ticketID, err := domain.NewWagerID("wgr_rr_1")
	if err != nil {
		t.Fatalf("wager id: %v", err)
	}
	ticket, err := domain.NewWager(domain.WagerParams{
		ID:              ticketID,
		UserID:          testUser,
		Kind:            domain.WagerKindRoundRobin,
		Legs:            legs,
		Stake:           mustMoney(t, 500),
		AcceptedDecimal: 4.0,
		Rounding:        wagerRounding,
		RoundRobinID:    rrID,
		PlacedAt:        testNow.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("wager: %v", err)
	}

	d.betting.placement = betting.Placement{Wagers: []domain.Wager{ticket}, RoundRobin: rr}

	rec := place(t, d, placeBody("sel_bos", 1.91, 2500), "key-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	body := decodeJSONBody[gen.Placement](t, rec)
	if body.RoundRobin == nil {
		t.Fatalf("round_robin missing: %s", rec.Body.String())
	}
	set := *body.RoundRobin
	if set.Id != "rr_1" {
		t.Errorf("id = %q, want rr_1", set.Id)
	}
	if set.SelectionCount != 2 || set.CombinationCount != 1 {
		t.Errorf("selection/combination count = %d/%d, want 2/1", set.SelectionCount, set.CombinationCount)
	}
	if len(set.Sizes) != 1 || set.Sizes[0] != 2 {
		t.Errorf("sizes = %v, want [2]", set.Sizes)
	}
	if set.StakePerCombinationMinor != 500 {
		t.Errorf("stake_per_combination_minor = %d, want the PER-TICKET stake", set.StakePerCombinationMinor)
	}
	if body.Wagers[0].RoundRobinId == nil || *body.Wagers[0].RoundRobinId != "rr_1" {
		t.Error("the ticket does not name its parent")
	}
}

// testLegOnEvent is [testLeg] with the event and market named, for fixtures that
// need legs on different games.
func testLegOnEvent(t *testing.T, id, selection, event, market string, decimal float64) domain.Leg {
	t.Helper()
	price, err := domain.NewPrice(domain.PriceParams{
		SelectionID: mustSelectionID(t, selection),
		BookID:      mustBookID(t, "bk_sharp"),
		Decimal:     decimal,
		Line:        domain.NoLine(),
		ObservedAt:  testNow.Add(-30 * time.Second),
	})
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	legID, err := domain.NewLegID(id)
	if err != nil {
		t.Fatalf("leg id: %v", err)
	}
	leg, err := domain.NewLeg(domain.LegParams{
		ID:          legID,
		EventID:     mustEventID(t, event),
		MarketID:    mustMarketID(t, market),
		MarketType:  domain.MarketTypeMoneyline,
		Role:        domain.SelectionRoleHome,
		SelectionID: mustSelectionID(t, selection),
		Price:       price,
	})
	if err != nil {
		t.Fatalf("leg: %v", err)
	}
	return leg
}

// TestQuoteSlipPricesACrossGameParlay, which is the shape the shipped pricer
// DOES price: the product of independent leg prices.
func TestQuoteSlipPricesACrossGameParlay(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedSlip(t, d, 100000)
	ids := seedGames(t, d, 2)

	body := fmt.Sprintf(`{"kind":"parlay","stake_minor":1000,"legs":[
		{"selection_id":%q,"book_id":"bk_sharp","seen_decimal":2.0},
		{"selection_id":%q,"book_id":"bk_sharp","seen_decimal":2.0}]}`, ids[0], ids[1])

	rec := quoteSlip(t, d, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	q := decodeJSONBody[gen.SlipQuote](t, rec)
	if q.DecimalOdds == nil || *q.DecimalOdds != 4.0 {
		t.Fatalf("decimal_odds = %v, want 2.0 x 2.0", q.DecimalOdds)
	}
	if q.PotentialPayoutMinor != 4000 {
		t.Errorf("potential_payout_minor = %d, want 1000 x 4", q.PotentialPayoutMinor)
	}
	if q.IsSameGame == nil || *q.IsSameGame {
		t.Error("is_same_game is true for two different events")
	}
}

// TestGetWagerRendersTheBookedTermsAndNotTheMarket.
//
// The one property a wager detail page must have: every price on it is the price
// AT PLACEMENT. The fixture deliberately leaves a live quote on the same
// selection at a DIFFERENT number, so a handler that had re-resolved the leg
// would render that one instead and this test would say so.
func TestGetWagerRendersTheBookedTermsAndNotTheMarket(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedSlip(t, d, 50000)
	// The board fixture quotes sel_bos at 1.91 and 1.95 right now. The ticket
	// below was booked at 1.60, a price no book is currently offering.
	booked := wagerFromDomain(testStraight(t, "wgr_1", 2500))
	booked.Legs[0].Decimal = mustDecimal(t, 1.60)
	d.wagers.wagers = []Wager{booked}

	rec := callAuthed(t, d, http.MethodGet, "/v1/wagers/wgr_1?odds_format=american", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	w := decodeJSONBody[gen.Wager](t, rec)
	if len(w.Legs) != 1 {
		t.Fatalf("legs = %d, want 1", len(w.Legs))
	}
	if w.Legs[0].DecimalOdds != 1.60 {
		t.Errorf("leg decimal_odds = %v, want the BOOKED 1.60 and never a current quote",
			w.Legs[0].DecimalOdds)
	}
	if w.Legs[0].BookSlug != "sharp" {
		t.Errorf("book_slug = %q, want the booking book's slug", w.Legs[0].BookSlug)
	}
	// A moneyline carries no line, and `null` must not become 0.0 — a pick'em is
	// a different bet from no handicap at all.
	if w.Legs[0].Line != nil || w.Legs[0].GradingLine != nil {
		t.Errorf("a moneyline leg reported a line: %v / %v", w.Legs[0].Line, w.Legs[0].GradingLine)
	}
	if w.Legs[0].GradedAt != nil {
		t.Error("a pending leg reported a grading instant")
	}
	// odds_format adds a rendered string; the canonical decimal always travels.
	if w.Legs[0].Display == nil || *w.Legs[0].Display == "" {
		t.Error("odds_format=american did not add a display string")
	}
	if w.SettledAt != nil || w.ReturnedMinor != nil || w.NetReturnMinor != nil {
		t.Error("a running ticket reported settlement fields")
	}
	assertMinorFieldsAreIntegers(t, rec.Body.Bytes())
}

// TestSettledWagerReportsWhatItReturned.
//
// returned_minor is the ONLY authority on what settlement paid, and net_return
// is signed. A losing ticket must report a negative net return rather than a
// zero, or a P&L page reads every loss as a break-even.
func TestSettledWagerReportsWhatItReturned(t *testing.T) {
	t.Parallel()

	d := newDeps()
	seedSlip(t, d, 50000)
	lost := wagerFromDomain(testStraight(t, "wgr_1", 2500))
	lost.Status = domain.WagerStatusLost
	zero := domain.ZeroMoney
	net := domain.Money(-2500)
	lost.Returned, lost.NetReturn = &zero, &net
	lost.UpdatedAt = testNow
	d.wagers.wagers = []Wager{lost}

	rec := callAuthed(t, d, http.MethodGet, "/v1/wagers/wgr_1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	w := decodeJSONBody[gen.Wager](t, rec)
	if w.ReturnedMinor == nil || *w.ReturnedMinor != 0 {
		t.Errorf("returned_minor = %v, want a present 0", w.ReturnedMinor)
	}
	if w.NetReturnMinor == nil || *w.NetReturnMinor != -2500 {
		t.Errorf("net_return_minor = %v, want -2500", w.NetReturnMinor)
	}
	if w.SettledAt == nil || !w.SettledAt.Equal(testNow) {
		t.Errorf("settled_at = %v, want the transition instant once terminal", w.SettledAt)
	}
}
