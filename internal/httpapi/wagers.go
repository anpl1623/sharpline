package httpapi

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anpl1623/sharpline/internal/betting"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
	"github.com/anpl1623/sharpline/internal/httpapi/middleware"
)

// Placement, wager history, and cash-out.
//
// THIS FILE MOVES MONEY, AND IT DOES NOT DECIDE ANYTHING ABOUT IT. Every rule
// that governs whether a slip becomes a ticket — the self-exclusion read against
// a locked row, the responsible-gaming limit sums, the balance fold, the price
// re-read, the round-robin expansion, the double-entry stake movement — lives in
// internal/betting and happens inside one transaction there. What is here is
// parsing, one sentinel-to-status table, and mapping onto the wire.
//
// The division matters more than usual because of the shape of the failures.
// internal/betting's ordering is load-bearing (the users row is locked before
// the balance is folded, so two concurrent placements cannot both see enough
// money), and a handler that re-checked any of it would be a second answer able
// to disagree with the one the transaction used. So there is no affordability
// check in this file, no status check, and no limit evaluation.
//
// # The two things this file DOES own
//
// The Idempotency-Key header, which is parsed and validated here because it is
// an HTTP framing concern, and the sentinel-to-status mapping, which is the one
// piece of knowledge that belongs to neither package alone.

// Fixed messages for the betting surface.
//
// Declared here rather than in respond.go's block for the reason that block
// gives: every message on the wire is a CONSTANT chosen at the call site, and
// nothing derived from an error ever reaches it. Adding them beside the handlers
// that use them keeps that property and keeps the reasoning next to the code —
// respond.go's rule is about where messages come FROM, not about which file they
// are typed in.
const (
	msgIdempotencyKey     = "an Idempotency-Key header is required"
	msgSelfExcluded       = "this account is self-excluded and cannot place a wager"
	msgInsufficientFunds  = "the balance does not cover the stake"
	msgLimitExceeded      = "a self-imposed limit would be exceeded"
	msgInvalidGrantAmount = "the grant amount must be positive and within the per-request maximum"
	msgPriceMoved         = "the price moved and was not accepted"
	msgMarketUnavailable  = "the market is not offering this bet right now"
	msgSlipInvalid        = "the slip cannot be placed as written"
	msgCashOutTerminal    = "this wager is already settled and cannot be cashed out"
	msgCashOutUnavailable = "this wager cannot be cashed out right now"
)

// wagerRounding is the rule stake x price is collapsed to whole minor units
// under, for every ticket this API books.
//
// # It is house policy and it is deliberately NOT a request field
//
// money.go refuses to give Rounding a default — "a silent default is how a house
// edge appears in a ledger that nobody meant to put one in" — which means
// somebody has to choose, and the choice must not be the caller's: a client that
// could name the rounding mode could name the one that rounds its way on every
// ticket it places. So the mode is fixed here, applied server-side, and REPORTED
// on every quote and every wager, which is the arrangement under which a policy
// is auditable rather than merely hidden.
//
// # Why half-to-even and not half-away-from-zero
//
// Half-away-from-zero is what a human means by "round to the nearest cent" and
// would be the friendlier choice for a single ticket. It also has a directional
// bias, and money.go says where that matters: "Prefer [half-to-even] wherever
// many roundings accumulate into one aggregate." A book's settlements are
// exactly that aggregate — every payout is one rounding, and a bias of half a
// minor unit per ticket is a systematic transfer in one direction that nobody
// chose and nobody would find. Neutrality over millions of roundings is worth
// more than a hundredth of a cent on any one of them.
const wagerRounding = domain.RoundHalfToEven

// idempotencyHeader is the header a money-moving request carries.
const idempotencyHeader = "Idempotency-Key"

// -----------------------------------------------------------------------------
// Placement
// -----------------------------------------------------------------------------

// handlePlaceWager books the slip.
//
// 201 when this request wrote the tickets, 200 when a previous one already had.
// Both carry the same body, which is what lets a client that retried after a
// timeout learn from the status line whether its first attempt landed — a
// question it otherwise cannot answer without comparing timestamps against a
// history page.
func (a *API) handlePlaceWager(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}

	key, ok := a.idempotencyKey(w, r)
	if !ok {
		return
	}

	var body gen.PlaceWagerRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}

	slip, bad := parsePlacement(body)
	if len(bad) > 0 {
		failInvalid(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable, msgUnprocessable, bad)
		return
	}

	books, err := a.catalogue.Books(r.Context())
	if err != nil {
		failWith(w, r, a.log, "place wager: books", err)
		return
	}

	placement, err := a.betting.Place(r.Context(), betting.PlaceRequest{
		UserID:         user,
		IdempotencyKey: key,
		Slip:           slip,
		Audit:          a.auditContext(r).forBetting(),
	})
	if err != nil {
		a.failBetting(w, r, "place wager", err)
		return
	}

	out, err := wirePlacement(placement, bookIndex(books), parseOddsFormat(r.URL.Query(), &badParams{}))
	if err != nil {
		// The tickets are BOOKED. Answering 500 here reports a failure that did
		// not happen, and the customer's money has already moved — so this is
		// logged loudly and the client is told to re-read its history rather
		// than being invited to retry a placement that would be a replay.
		failWith(w, r, a.log, "place wager: render receipt", err)
		return
	}

	// a.record is NOT called here, and that is the correct shape rather than an
	// omission. The audit entry for a placement — `wager.place`, one per booked
	// ticket — is written by internal/betting INSIDE the placement transaction,
	// through betting.Tx.RecordAudit, using the provenance handed to Place
	// above. It commits with the wager and the stake movement or none of them
	// does.
	//
	// Writing it from here instead would be the wrong fix, for the reason
	// [API.record] gives: that path writes after the fact on its own
	// connection, so an entry could commit without its wager — the placement
	// rolls back at COMMIT on the deferred zero-sum trigger and the trail then
	// claims a bet that does not exist — or go missing after a committed one.
	// A trail that is one crash away from wrong is worse than one with a stated
	// gap. handleSetLimit takes the same route through its own store for the
	// same reason.
	//
	// A REPLAY WRITES NO ROW, deliberately: nothing changed, and an entry
	// saying this request placed this wager would double-count the placement
	// for anyone summing the trail.
	status := http.StatusCreated
	if placement.Replayed {
		status = http.StatusOK
	}
	respond(w, status, out)
}

// parsePlacement turns the wire request into internal/betting's slip.
//
// It parses and it does not judge: the arity rules, the teaser correspondence,
// the round-robin sizes and the duplicate-selection check are betting.Slip's own
// (Validate) and are enforced inside the placement path, so re-checking them
// here would be a second implementation that could disagree. What this does is
// the conversion the service cannot do for itself — strings to domain
// identifiers, an int64 to domain.Money, a nullable JSON number to domain.Line —
// and it reports each failure against the field that caused it, so a client
// learns about all of them at once rather than one per round trip.
func parsePlacement(body gen.PlaceWagerRequest) (betting.Slip, []gen.InvalidParam) {
	var bad badParams

	slip := betting.Slip{
		Rounding:          wagerRounding,
		AcceptBetterPrice: body.AcceptBetterPrice != nil && *body.AcceptBetterPrice,
	}

	kind, err := domain.ParseWagerKind(string(body.Kind))
	if err != nil {
		bad.add("kind", "must be one of straight, parlay, round_robin, teaser")
	}
	slip.Kind = kind

	stake, err := domain.FromMinorUnits(body.StakeMinor)
	if err != nil || !stake.IsPositive() {
		bad.add("stake_minor", "must be a positive integer number of minor units")
	}
	slip.Stake = stake

	if body.TeaserPoints != nil {
		slip.TeaserPoints = *body.TeaserPoints
	}
	if body.SeenTicketDecimal != nil {
		slip.SeenTicketDecimal = *body.SeenTicketDecimal
	}
	slip.AcceptTicketDecimal = body.AcceptedTicketDecimal

	if body.RoundRobinSizes != nil {
		slip.Sizes = make([]int, 0, len(*body.RoundRobinSizes))
		for _, size := range *body.RoundRobinSizes {
			slip.Sizes = append(slip.Sizes, int(size))
		}
	}

	slip.Legs = make([]betting.SlipLeg, 0, len(body.Legs))
	for i, leg := range body.Legs {
		parsed, ok := parsePlacementLeg(i, leg, &bad)
		if !ok {
			continue
		}
		slip.Legs = append(slip.Legs, parsed)
	}

	return slip, bad.items
}

func parsePlacementLeg(i int, leg gen.PlacementLeg, bad *badParams) (betting.SlipLeg, bool) {
	field := func(name string) string { return fmt.Sprintf("legs[%d].%s", i, name) }

	selection, err := domain.NewSelectionID(leg.SelectionId)
	if err != nil {
		bad.add(field("selection_id"), "is not a valid identifier")
		return betting.SlipLeg{}, false
	}
	book, err := domain.NewBookID(leg.BookId)
	if err != nil {
		bad.add(field("book_id"), "is not a valid identifier")
		return betting.SlipLeg{}, false
	}
	if !finiteOdds(leg.SeenDecimal) {
		bad.add(field("seen_decimal"), "must be a decimal price greater than 1")
		return betting.SlipLeg{}, false
	}

	out := betting.SlipLeg{
		SelectionID: selection,
		BookID:      book,
		SeenDecimal: leg.SeenDecimal,
	}

	line, ok := parseLine(leg.SeenLine)
	if !ok {
		bad.add(field("seen_line"), "must be a finite number or null")
		return betting.SlipLeg{}, false
	}
	out.SeenLine = line

	// An acceptance is the customer's explicit agreement to a re-quote, so it is
	// all-or-nothing: a decimal with no line, on a market that HAS a line, would
	// be consent to a price at an unstated handicap. betting.Acceptance requires
	// both, and the two fields are read together here for the same reason.
	if leg.AcceptedDecimal != nil {
		if !finiteOdds(*leg.AcceptedDecimal) {
			bad.add(field("accepted_decimal"), "must be a decimal price greater than 1")
			return betting.SlipLeg{}, false
		}
		acceptedLine, ok := parseLine(leg.AcceptedLine)
		if !ok {
			bad.add(field("accepted_line"), "must be a finite number or null")
			return betting.SlipLeg{}, false
		}
		out.Accept = &betting.Acceptance{Decimal: *leg.AcceptedDecimal, Line: acceptedLine}
	} else if leg.AcceptedLine != nil {
		bad.add(field("accepted_decimal"), "is required when accepted_line is present")
		return betting.SlipLeg{}, false
	}

	return out, true
}

// parseLine turns the wire's `number | null` into a domain.Line.
//
// The distinction is the whole reason domain.Line exists: `null` is NO LINE (a
// moneyline, a futures market) and `0.0` is a traded pick'em, and a *float64
// that treated the two alike would make a pick'em unbettable through this API.
func parseLine(v *float64) (domain.Line, bool) {
	if v == nil {
		return domain.NoLine(), true
	}
	line, err := domain.NewLine(*v)
	if err != nil {
		return domain.Line{}, false
	}
	return line, true
}

// finiteOdds rejects NaN, the infinities and anything at or below evens before
// the value reaches the domain.
//
// It is a guard against JSON, not a duplicate of the domain's own range check:
// a decimal price arrives as a float64 straight out of a decoder, and NaN
// compares false against every bound, so a bare `d > 1` test would let it
// through to a multiplication that silently produces a NaN payout.
func finiteOdds(d float64) bool {
	return !math.IsNaN(d) && !math.IsInf(d, 0) && d > domain.MinDecimalOdds
}

// -----------------------------------------------------------------------------
// History
// -----------------------------------------------------------------------------

// handleListWagers serves the customer's own wager history, newest first.
func (a *API) handleListWagers(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	bad := &badParams{}
	limit := parseLimit(q, bad)
	format := parseOddsFormat(q, bad)
	statuses := parseWagerStatuses(q, bad)
	if bad.any() {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam, bad.items)
		return
	}

	// The scope fingerprint binds the cursor to the filter it was minted under,
	// so a client that adds a status mid-page gets a clean 400 rather than a
	// consistently-ordered page from a different set. The user is in the
	// fingerprint too: a cursor is not a capability, but a cursor that decoded
	// against somebody else's history would be a confusing 404 rather than an
	// obvious refusal.
	scope := cursorScope("wagers", user.String(), strings.Join(statusStrings(statuses), ","))

	query := WagerPageQuery{UserID: user, Statuses: statuses, Limit: limit}
	if raw := first(q, "cursor"); raw != "" {
		c, err := decodeWagerCursor(raw, scope)
		if err != nil {
			failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidCursor, msgInvalidCursor,
				[]gen.InvalidParam{{Name: "cursor", Reason: "is not valid for this query"}})
			return
		}
		query.After = &c
	}

	page, err := a.wagers.WagerPage(r.Context(), query)
	if err != nil {
		failWith(w, r, a.log, "list wagers", err)
		return
	}

	books, err := a.catalogue.Books(r.Context())
	if err != nil {
		failWith(w, r, a.log, "list wagers: books", err)
		return
	}
	index := bookIndex(books)

	out := gen.WagerPage{Data: make([]gen.Wager, 0, len(page.Wagers))}
	for _, wager := range page.Wagers {
		out.Data = append(out.Data, wireWager(wager, index, format))
	}

	// The cursor is minted from the last row SCANNED, which under a status
	// filter is not the last row returned. Minting from the last returned row
	// would restart the next page before the rows this one filtered out and
	// serve them again.
	var next string
	if page.HasMore && !page.Last.ID.IsZero() {
		next = encodeWagerCursor(page.Last, scope)
	}
	out.Page = wirePage(limit, page.HasMore, next)
	respond(w, http.StatusOK, out)
}

// handleGetWager serves one ticket.
//
// The ownership rule is in the store, not here: [Wagers.Wager] takes the user
// and answers [ErrNotFound] both for a wager that does not exist and for one
// that belongs to somebody else. There is deliberately no branch in this handler
// that could tell them apart, because a 403 on another customer's wager id would
// confirm the id exists.
func (a *API) handleGetWager(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}
	id, ok := pathWagerID(r)
	if !ok {
		failNotFound(w, r)
		return
	}

	format := parseOddsFormat(r.URL.Query(), &badParams{})

	wager, err := a.wagers.Wager(r.Context(), user, id)
	if err != nil {
		a.notFoundOr(w, r, "get wager", err)
		return
	}
	books, err := a.catalogue.Books(r.Context())
	if err != nil {
		failWith(w, r, a.log, "get wager: books", err)
		return
	}
	respond(w, http.StatusOK, wireWager(wager, bookIndex(books), format))
}

// -----------------------------------------------------------------------------
// Cash-out
// -----------------------------------------------------------------------------

// handleCashOutQuote prices an early close.
//
// THE WAGER IS READ FIRST, THROUGH THE USER-SCOPED PORT, AND THE QUOTE IS ASKED
// FOR SECOND. [CashOutQuotes] does not take a user — pricing a ticket has no
// opinion about who owns it — so the ordering here is what makes the endpoint
// safe: an unauthorised caller gets the 404 the read produced and never reaches
// a pricing call at all. Reversing the two would price somebody else's ticket
// and then discard the answer, which leaks nothing today and leaks a timing
// signal the moment pricing gets slower.
func (a *API) handleCashOutQuote(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}
	id, ok := pathWagerID(r)
	if !ok {
		failNotFound(w, r)
		return
	}

	wager, err := a.wagers.Wager(r.Context(), user, id)
	if err != nil {
		a.notFoundOr(w, r, "cash-out quote: wager", err)
		return
	}
	if wager.Status.IsTerminal() {
		// Answered before pricing rather than by letting the service refuse,
		// because this is the one cash-out refusal with a specific, useful
		// sentence: the ticket is finished, and no amount of re-quoting will
		// change that.
		fail(w, r, http.StatusConflict, gen.ErrorCodeCashOutUnavailable, msgCashOutTerminal)
		return
	}

	quote, err := a.cashOutQuotes.CashOutQuote(r.Context(), id)
	if err != nil {
		a.failBetting(w, r, "cash-out quote", err)
		return
	}

	out, err := wireCashOutQuote(quote, wager, pendingLegs(wager))
	if err != nil {
		failWith(w, r, a.log, "cash-out quote: render", err)
		return
	}
	respond(w, http.StatusOK, out)
}

// handleTakeCashOut closes a ticket early at a quoted value.
func (a *API) handleTakeCashOut(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}
	id, ok := pathWagerID(r)
	if !ok {
		failNotFound(w, r)
		return
	}
	key, ok := a.idempotencyKey(w, r)
	if !ok {
		return
	}

	var body gen.CashOutRequest
	if err := decodeJSON(r, &body); err != nil {
		a.badBody(w, r, err)
		return
	}
	accepted, err := domain.FromMinorUnits(body.AcceptedValueMinor)
	if err != nil || !accepted.IsPositive() {
		failInvalid(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable, msgUnprocessable,
			[]gen.InvalidParam{{
				Name:   "accepted_value_minor",
				Reason: "must be a positive integer number of minor units",
			}})
		return
	}

	// The ownership read comes first here for the same reason it does on the
	// quote, and it is load-bearing rather than defensive: the take port is
	// keyed by wager id, so without this a caller could settle a ticket that is
	// not theirs.
	if _, err := a.wagers.Wager(r.Context(), user, id); err != nil {
		a.notFoundOr(w, r, "take cash-out: wager", err)
		return
	}

	settled, err := a.cashOuts.TakeCashOut(r.Context(), TakeCashOut{
		UserID:         user,
		WagerID:        id,
		IdempotencyKey: key,
		AcceptedValue:  accepted,
		Audit:          a.auditContext(r),
	})
	if err != nil {
		a.failBetting(w, r, "take cash-out", err)
		return
	}

	books, err := a.catalogue.Books(r.Context())
	if err != nil {
		failWith(w, r, a.log, "take cash-out: books", err)
		return
	}
	respond(w, http.StatusOK, wireWager(wagerFromDomain(settled), bookIndex(books),
		parseOddsFormat(r.URL.Query(), &badParams{})))
}

// pendingLegs counts the legs still waiting on a game.
func pendingLegs(w Wager) int {
	n := 0
	for _, leg := range w.Legs {
		if leg.Status == domain.LegStatusPending {
			n++
		}
	}
	return n
}

// -----------------------------------------------------------------------------
// Shared plumbing
// -----------------------------------------------------------------------------

// idempotencyKey reads and validates the header, answering 400 when it cannot.
//
// # Why a missing key is refused rather than defaulted
//
// The resource identifier is derived from `(user, key)`, so without a key there
// is nothing to derive from and a retried submit — which the network produces
// eventually whether the client meant it or not — books a second bet. An
// endpoint that generated a key server-side would be worse than one that
// refused: it would look idempotent and be at-least-once, because every retry
// would generate a different one.
//
// It is a 400 and not a 422 because the fault is in the request's FRAMING rather
// than in what it asks for. The distinction is respond.go's and it tells a
// client whether to fix its transport or its logic.
func (a *API) idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get(idempotencyHeader))
	if key == "" || len(key) > betting.MaxIdempotencyKeyLen {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeBadRequest, msgIdempotencyKey,
			[]gen.InvalidParam{{
				Name: idempotencyHeader,
				Reason: "is required and must be between 1 and " +
					strconv.Itoa(betting.MaxIdempotencyKeyLen) + " bytes",
			}})
		return "", false
	}
	return key, true
}

func pathWagerID(r *http.Request) (domain.WagerID, bool) {
	id, err := domain.NewWagerID(r.PathValue("wagerId"))
	return id, err == nil
}

func bookIndex(books []Book) map[domain.BookID]Book {
	index := make(map[domain.BookID]Book, len(books))
	for _, b := range books {
		index[b.ID] = b
	}
	return index
}

// parseWagerStatuses reads the repeatable `status` filter.
//
// An unknown status is a 400 rather than an empty page, for the reason
// parseBookFilter gives about an unknown book slug: a typo that quietly returns
// nothing is indistinguishable from "you have no wagers", which is the one
// answer a customer must never be given wrongly.
func parseWagerStatuses(q map[string][]string, bad *badParams) []domain.WagerStatus {
	raw := q["status"]
	if len(raw) == 0 {
		return nil
	}
	out := make([]domain.WagerStatus, 0, len(raw))
	seen := make(map[domain.WagerStatus]struct{}, len(raw))
	for _, s := range raw {
		status, err := domain.ParseWagerStatus(s)
		if err != nil {
			bad.add("status", "must be one of placed, open, won, lost, void, push, cashed_out")
			continue
		}
		if _, dup := seen[status]; dup {
			continue
		}
		seen[status] = struct{}{}
		out = append(out, status)
	}
	return out
}

func statusStrings(statuses []domain.WagerStatus) []string {
	out := make([]string, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, s.String())
	}
	return out
}

// -----------------------------------------------------------------------------
// The wager cursor
// -----------------------------------------------------------------------------

// The keyset cursor for wager history.
//
// It is the SAME SCHEME as cursor.go's — same version byte, same field
// separator, same base64url encoding, same scope fingerprint, and the same
// argument for every one of those choices, which is not repeated here. What
// differs is the key it carries: (placed_at, wager_id) descending rather than
// (scheduled_start, event_id) ascending.
//
// It is a separate codec rather than a generalised one because the two decode
// into DIFFERENT domain identifiers, and the constructor each runs is the thing
// that stops a cursor smuggling a value the rest of the system considers
// impossible. A codec parameterised over the id type would either lose that
// check or reintroduce it at every call site.
//
// A cursor from one endpoint presented to the other decodes structurally and
// then fails the scope check, which is precisely what the fingerprint is for.

func encodeWagerCursor(key WagerKey, scope uint64) string {
	raw := strings.Join([]string{
		cursorVersion,
		// UnixNano rather than RFC 3339, for cursor.go's reason: the database
		// orders by the full-precision timestamptz, and a cursor rounded to
		// microseconds could re-emit or skip a ticket placed inside the rounded
		// interval — which for a round robin, whose tickets share an instant, is
		// the ordinary case rather than a corner one.
		strconv.FormatInt(key.PlacedAt.UTC().UnixNano(), 10),
		strconv.FormatUint(scope, 36),
		key.ID.String(),
	}, "\x1f")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeWagerCursor(encoded string, scope uint64) (WagerKey, error) {
	if len(encoded) > maxCursorLen {
		return WagerKey{}, fmt.Errorf("%w: too long", ErrBadCursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return WagerKey{}, fmt.Errorf("%w: not base64url", ErrBadCursor)
	}
	parts := strings.Split(string(raw), "\x1f")
	if len(parts) != 4 {
		return WagerKey{}, fmt.Errorf("%w: wrong field count", ErrBadCursor)
	}
	if parts[0] != cursorVersion {
		return WagerKey{}, fmt.Errorf("%w: unsupported version", ErrBadCursor)
	}
	nanos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return WagerKey{}, fmt.Errorf("%w: unparseable key instant", ErrBadCursor)
	}
	got, err := strconv.ParseUint(parts[2], 36, 64)
	if err != nil {
		return WagerKey{}, fmt.Errorf("%w: unparseable scope", ErrBadCursor)
	}
	if got != scope {
		return WagerKey{}, fmt.Errorf("%w: cursor belongs to a different query", ErrBadCursor)
	}
	// Through the domain constructor, never a cast: it is what stops a cursor
	// smuggling an identifier the rest of the system considers impossible into a
	// query parameter.
	id, err := domain.NewWagerID(parts[3])
	if err != nil {
		return WagerKey{}, fmt.Errorf("%w: unparseable key identifier", ErrBadCursor)
	}
	return WagerKey{PlacedAt: time.Unix(0, nanos).UTC(), ID: id}, nil
}

// -----------------------------------------------------------------------------
// Errors
// -----------------------------------------------------------------------------

// failBetting maps internal/betting's sentinels onto the wire.
//
// # This table is the one piece of knowledge that belongs to neither package
//
// internal/betting/errors.go proposes a mapping in its own header —
// ErrInvalidSlip to 400, ErrNotPermitted to 403, ErrUnaffordable to 422,
// ErrMarketMoved to 409 — and this function DELIBERATELY DIVERGES FROM IT IN TWO
// PLACES. Both divergences are about this API's own status vocabulary, which
// respond.go defines and which internal/betting cannot see:
//
//   - ErrInvalidSlip becomes 422, not 400. respond.go reserves 400 for a request
//     that could not be UNDERSTOOD — an unparseable body, a parameter of the
//     wrong type — and 422 for one that was understood and cannot be satisfied.
//     "Two legs on one market" is valid JSON expressing an impossible ticket, so
//     it is 422. The distinction is the difference between "fix your serialiser"
//     and "fix your logic", and collapsing it is the single most common thing an
//     API does to make its errors useless.
//
//   - ErrLimitExceeded becomes 422, not 403, even though it wraps
//     ErrNotPermitted. 403 in this API means a STANDING CONDITION ON THE ACCOUNT:
//     the credential is good and the actor is refused, which describes a
//     suspension and a self-exclusion. A responsible-gaming limit is a ceiling
//     on THIS SLIP's size — the account is in good standing, and the same slip
//     at a smaller stake, or after the period rolls, is accepted. Reporting it as
//     403 would tell a customer their account is blocked when it is not, and
//     errors.go's own argument for splitting the classes is exactly that a
//     customer told the wrong thing retries the wrong fix.
//
// ErrSelfExcluded keeps its own code rather than folding into
// account_not_active, because it is the one status the customer chose. A
// suspended account is told to contact support; a self-excluded one is told how
// their exclusion is managed. Collapsing the two makes the responsible-gaming
// path read as a punishment.
//
// Anything not matched here becomes a 500, which is the correct default: a
// sentinel this function has never heard of must not fall through to a status
// chosen by accident.
func (a *API) failBetting(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	// A price move is the only refusal that carries numbers, and it must, so
	// this arm comes before the generic ErrMarketMoved one below.
	case errors.Is(err, betting.ErrPriceMoved):
		a.failPriceMoved(w, r, err)

	case errors.Is(err, betting.ErrSelfExcluded):
		fail(w, r, http.StatusForbidden, gen.ErrorCodeSelfExcluded, msgSelfExcluded)

	case errors.Is(err, betting.ErrAccountNotWagerable):
		fail(w, r, http.StatusForbidden, gen.ErrorCodeAccountNotActive, msgAccountBlocked)

	case errors.Is(err, betting.ErrLimitExceeded):
		failInvalid(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeLimitExceeded,
			msgLimitExceeded, limitParams(err))

	case errors.Is(err, betting.ErrInvalidGrantAmount):
		fail(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeInvalidGrantAmount, msgInvalidGrantAmount)

	case errors.Is(err, betting.ErrInsufficientFunds):
		fail(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeInsufficientFunds, msgInsufficientFunds)

	case errors.Is(err, betting.ErrCashOutUnavailable):
		fail(w, r, http.StatusConflict, gen.ErrorCodeCashOutUnavailable, msgCashOutUnavailable)

	case errors.Is(err, betting.ErrMarketMoved):
		// ErrMarketNotOpen, ErrEventStarted, ErrStaleQuote and
		// ErrQuoteUnavailable collapse into one code on purpose. They are four
		// descriptions of one situation — the book is not laying this bet right
		// now — and a client's only useful response to any of them is the same:
		// re-quote the slip. Four codes would be four branches that all did
		// that.
		fail(w, r, http.StatusConflict, gen.ErrorCodeMarketUnavailable, msgMarketUnavailable)

	case errors.Is(err, betting.ErrWagerNotFound), errors.Is(err, ErrNotFound):
		failNotFound(w, r)

	case errors.Is(err, betting.ErrInvalidSlip), errors.Is(err, domain.ErrInvalid):
		failInvalid(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable,
			msgSlipInvalid, slipParams(err))

	default:
		failWith(w, r, a.log, op, err)
	}
}

// failPriceMoved answers 409 with the numbers that changed.
//
// # Why this writes an error body respond.go's [fail] cannot
//
// respond.go's rule is that NOTHING DERIVED FROM AN ERROR VALUE reaches the
// wire, and this function keeps it: the `message` is a constant declared in this
// file, and every number in `price_moves` is one this SERVICE computed or one
// the client itself sent, extracted through a typed field on a struct error —
// never a formatted message, never err.Error(). betting.PriceMove is a struct
// error rather than a formatted one precisely so that this is possible; parsing
// two prices back out of an error string is the kind of thing that works until
// a number contains a comma.
//
// The alternative — 409 with only a code — was rejected because it makes the
// client re-quote to discover what moved, which races the next move and shows
// the customer a third number that was never the reason their bet was refused.
func (a *API) failPriceMoved(w http.ResponseWriter, r *http.Request, err error) {
	body := gen.Error{
		Code:      gen.ErrorCodePriceMoved,
		Message:   msgPriceMoved,
		RequestId: middleware.RequestIDFrom(r.Context()),
	}
	if moves := priceMoves(err); len(moves) > 0 {
		body.PriceMoves = &moves
	}
	respond(w, http.StatusConflict, body)
}

// priceMoves extracts the typed detail behind betting.ErrPriceMoved.
//
// An error that wraps the sentinel without carrying a *betting.PriceMove yields
// nothing rather than a fabricated pair. A move reported with invented numbers
// would be worse than one reported with none: the client would render it, and
// the customer would be shown a price the book never quoted.
func priceMoves(err error) []gen.PriceMove {
	var move *betting.PriceMove
	if !errors.As(err, &move) {
		return nil
	}
	out := gen.PriceMove{
		Scope:          gen.Leg,
		Movement:       priceMovement(move.SeenDecimal, move.CurrentDecimal),
		SeenDecimal:    &move.SeenDecimal,
		CurrentDecimal: &move.CurrentDecimal,
		Improved:       &move.Improved,
		Accepted:       &move.Accepted,
	}
	if move.SelectionID != "" {
		id := move.SelectionID
		out.SelectionId = &id
	} else {
		// The service reports a whole-ticket re-quote with no selection: the
		// number that moved is the ticket's price, not any one leg's.
		out.Scope = gen.Ticket
	}
	if move.BookID != "" {
		id := move.BookID
		out.BookId = &id
	}
	out.SeenLine = renderedLine(move.SeenLine)
	out.CurrentLine = renderedLine(move.CurrentLine)
	return []gen.PriceMove{out}
}

// renderedLine converts domain.Line's canonical rendering back into a wire
// number.
//
// betting.PriceMove carries its lines already rendered, because its own
// Error() method needs strings. The round trip is exact rather than lossy:
// domain.Line.String is strconv.FormatFloat(v, 'f', -1, 64), the shortest form
// that parses back to the identical float64, and an absent line renders as the
// sentinel "none" which no float parse accepts — so the `number | null`
// distinction survives without a second field.
func renderedLine(s string) *float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

// limitParams names the breached limit.
//
// The `name` is the field the customer can act on — the stake — and the `reason`
// names the limit's kind and period. Both halves of that reason come from
// SERVER-SIDE ENUMS (auth.LimitKind, auth.LimitPeriod) and never from request
// input, so respond.go's rule that nothing client-controlled is reflected into
// an error body is kept: there is no path by which a caller's bytes reach this
// string.
//
// The amounts are deliberately NOT included. "You have used $180 of your $200
// daily limit" is a better sentence and it belongs on the account screen, which
// reads the limit and the ledger directly; putting the numbers in a rejection
// would make the rejection the second place they are computed.
func limitParams(err error) []gen.InvalidParam {
	var breach *betting.LimitBreach
	if !errors.As(err, &breach) {
		return nil
	}
	return []gen.InvalidParam{{
		Name:   "stake_minor",
		Reason: "would exceed the " + breach.Kind + " limit in force for this " + breach.Period,
	}}
}

// slipParams attributes a slip refusal to a field where the sentinel identifies
// one unambiguously.
//
// The mapping is deliberately partial. A sentinel that does not name a single
// field yields no attribution rather than a guess, because a form that
// highlights the wrong input is worse than one that highlights nothing: the
// customer edits the field that was fine and resubmits the one that was not.
func slipParams(err error) []gen.InvalidParam {
	switch {
	case errors.Is(err, betting.ErrStakeNotPositive):
		return []gen.InvalidParam{{Name: "stake_minor", Reason: "must be greater than zero"}}
	case errors.Is(err, betting.ErrTeaserPoints):
		return []gen.InvalidParam{{Name: "teaser_points", Reason: "are required on a teaser and on nothing else"}}
	case errors.Is(err, betting.ErrTeaserMarketType):
		return []gen.InvalidParam{{Name: "legs", Reason: "only a spread or total selection can be teased"}}
	case errors.Is(err, betting.ErrRoundRobinSizes):
		return []gen.InvalidParam{{Name: "round_robin_sizes", Reason: "each size is at least 2 and at most the number of legs"}}
	case errors.Is(err, betting.ErrLegCountForKind):
		return []gen.InvalidParam{{Name: "legs", Reason: "this wager kind does not admit that number of selections"}}
	case errors.Is(err, betting.ErrDuplicateSelection):
		return []gen.InvalidParam{{Name: "legs", Reason: "the same selection appears twice"}}
	case errors.Is(err, betting.ErrDuplicateMarket):
		return []gen.InvalidParam{{Name: "legs", Reason: "two selections answer the same market and cannot both win"}}
	case errors.Is(err, betting.ErrTooManyLegs):
		return []gen.InvalidParam{{Name: "legs", Reason: "the slip has more legs than a ticket may carry"}}
	case errors.Is(err, betting.ErrSameGameUnsupported):
		return []gen.InvalidParam{{Name: "legs", Reason: "this book does not price a same-game parlay"}}
	case errors.Is(err, betting.ErrTeaserUnsupported):
		return []gen.InvalidParam{{Name: "kind", Reason: "this book does not price a teaser"}}
	default:
		return nil
	}
}
