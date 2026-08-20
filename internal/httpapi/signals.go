package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// The signals surface: the +EV finder, live arbitrage, and steam.
//
// CLAUDE.md §6 calls analytics "the differentiator" and §11 phase 9 builds it in
// Go FIRST, deliberately, so the phase 12 Flink SQL jobs have a reference
// implementation to be validated against — "same inputs, same outputs, or the
// Flink job is wrong" (§3).
//
// # What these three handlers do, and what they refuse to do
//
// They rank, filter, page and render PERSISTED FINDINGS. Not one of them devigs
// a market, sizes a Kelly stake, sums an implied probability or decides what
// counts as steam. `pricer` did all of that and wrote the answer down with the
// thresholds it was configured with; a second implementation here would be a
// second set of semantics for phase 12 to disagree with, which is precisely the
// failure the phase exists to prevent.
//
// # Two thresholds, always, and they are different facts
//
// Every finding carries `threshold_*` — what the detector was configured to emit
// when the row was written — and every read takes its own floor on top. A reader
// asking for less than the detector emitted gets what the detector emitted and
// nothing more; findings that were never written cannot be recovered, and this
// surface does not pretend otherwise. Collapsing the two would make a row
// uninterpretable the first time the detector is retuned.
//
// # Why the time bound is required in substance on all three
//
// `ev_signals` and `steam_signals` are Timescale hypertables with NO retention
// policy, so a read with no lower bound consults an index on every chunk that
// has ever existed. history.go makes the same argument for `prices`. The
// difference here is that these endpoints DO default the bound, because unlike a
// line-movement chart there is an obviously right default — "recently" — and
// there is a ceiling on how far back a caller may push it. The ceiling is not
// arbitrary: it is the RETENTION OF THE MATCHING KAFKA TOPIC, so the window this
// API will serve and the window the bus can replay are the same window.

// Windows and bounds. Every one is a named constant rather than a literal at a
// call site, because each is a product decision somebody will want to change and
// a magic number in a handler is a decision nobody can find.
const (
	// defaultEVSignalWindow is how far back `/signals/ev` looks by default. Six
	// hours matches [quoteFreshness], the horizon beyond which this API stops
	// calling a quote a current line — a +EV finding on a price that is no
	// longer current is history, not an opportunity.
	defaultEVSignalWindow = 6 * time.Hour

	// maxEVSignalLookback is the `signals.ev` topic's retention.
	maxEVSignalLookback = 7 * 24 * time.Hour

	// defaultArbSignalWindow is short because an arbitrage is: the phase-4 gate
	// found the leg-age bound binding almost constantly, and a finding whose
	// oldest leg was observed an hour ago is a historical curiosity.
	defaultArbSignalWindow = 15 * time.Minute

	// defaultSteamSignalWindow is longer than the arbitrage window and shorter
	// than the +EV one. A steam move stays interesting while the follower books
	// are still catching up and for a while after, as evidence of where the
	// money went.
	defaultSteamSignalWindow = 2 * time.Hour

	// maxFindingLookback is the `signals.arb` and `signals.steam` retention.
	maxFindingLookback = 30 * 24 * time.Hour

	// defaultArbMaxLegAge and defaultArbMaxSpread are
	// pricing.DefaultArbitrageConfig()'s MaxLegAge and MaxLegSpread.
	//
	// THE READER'S DEFAULT IS THE DETECTOR'S OWN BOUND, deliberately: a default
	// view narrower than what was detected would hide findings without saying
	// so, and a default view wider than the detector's bound cannot exist,
	// because the database CHECK `arbitrage_signals_within_own_bounds` makes
	// every stored finding satisfy its own recorded bounds already.
	//
	// They are duplicated from internal/pricing rather than imported because
	// this package must not depend on the pricer to serve a read — but they are
	// named here so the duplication is one grep away rather than two literals
	// in a query string.
	defaultArbMaxLegAge = 120 * time.Second
	defaultArbMaxSpread = 30 * time.Second

	// maxMarketTypeFilter is the number of members domain.MarketType has. A
	// filter naming more than every type is a client bug, not a wider query.
	maxMarketTypeFilter = 5

	// maxSignalSeconds bounds every duration parameter on this surface. A day is
	// far past the point at which any of these findings means anything, and the
	// bound is what stops a caller passing a value large enough to overflow the
	// conversion to time.Duration.
	maxSignalSeconds = 86400
)

// -----------------------------------------------------------------------------
// GET /signals/ev
// -----------------------------------------------------------------------------

func (a *API) handleEVSignals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bad := &badParams{}

	limit := parseLimit(q, bad)
	minEV := parseFloatParam(q, "min_ev_percent", 0, 0, 10000, bad)
	marketTypes := parseMarketTypes(q, bad)

	now := a.now().UTC()
	observedAfter := parseLowerBound(q, "observed_after", now, defaultEVSignalWindow, maxEVSignalLookback, bad)

	books, err := a.catalogue.Books(r.Context())
	if err != nil {
		failWith(w, r, a.log, "ev signals: books", err)
		return
	}
	bookIDs := sortedBookIDs(parseBookFilter(q, books, bad))

	league, err := a.leagueFilter(r.Context(), q, bad)
	if err != nil {
		failWith(w, r, a.log, "ev signals: league", err)
		return
	}

	if bad.any() {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam, bad.items)
		return
	}

	// THE SCOPE FINGERPRINT, and it is WIDER than the board's on purpose.
	//
	// board.go leaves `book` out of its fingerprint because there the filter
	// changes how a page is RENDERED — the same events come back either way.
	// Here every one of these five changes WHICH ROWS ARE IN THE SET and
	// therefore where the keyset boundary falls, so a client that changed one
	// mid-listing would silently receive a page from a different set, ordered
	// consistently, with nothing anywhere reporting it. With them in, that is a
	// 400 naming the cursor.
	scope := cursorScope("signals.ev",
		league.ID.String(),
		strconv.FormatInt(observedAfter.UnixNano(), 10),
		strconv.FormatFloat(minEV, 'g', -1, 64),
		strings.Join(stringsOfBooks(bookIDs), ","),
		strings.Join(stringsOfMarketTypes(marketTypes), ","),
	)

	var after *EVSignalKey
	if raw := first(q, "cursor"); raw != "" {
		key, err := decodeEVCursor(raw, scope)
		if err != nil {
			fail(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidCursor, msgInvalidCursor)
			return
		}
		after = &key
	}

	page, err := a.signals.EVSignals(r.Context(), EVSignalQuery{
		LeagueID:      league.ID,
		ObservedAfter: observedAfter,
		MinEVPercent:  minEV,
		Books:         bookIDs,
		MarketTypes:   marketTypes,
		After:         after,
		Limit:         limit,
	})
	if err != nil {
		failWith(w, r, a.log, "ev signals", err)
		return
	}

	out := gen.EVSignalPage{
		Data: make([]gen.EVSignal, 0, len(page.Signals)),
		AsOf: now,
	}
	for _, s := range page.Signals {
		out.Data = append(out.Data, wireEVSignal(s))
	}
	out.Page = wirePage(limit, page.HasMore, nextEVCursor(page.Signals, page.HasMore, scope))

	respond(w, http.StatusOK, out)
}

// nextEVCursor mints the continuation token from the LAST ROW of the page, and
// returns "" when there is no next page — so the envelope carries
// `next_cursor: null` rather than a token that would return nothing.
func nextEVCursor(rows []EVSignal, hasMore bool, scope uint64) string {
	if !hasMore || len(rows) == 0 {
		return ""
	}
	last := rows[len(rows)-1]
	return encodeSignalCursor(scope,
		strconv.FormatFloat(last.ExpectedValuePercent, 'g', -1, 64),
		strconv.FormatInt(last.QuoteObservedAt.UTC().UnixNano(), 10),
		last.SelectionID.String(),
		last.BookID.String(),
	)
}

func decodeEVCursor(encoded string, scope uint64) (EVSignalKey, error) {
	parts, err := decodeSignalCursor(encoded, scope, 4)
	if err != nil {
		return EVSignalKey{}, err
	}
	ev, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return EVSignalKey{}, fmt.Errorf("%w: unparseable expected value", ErrBadCursor)
	}
	nanos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return EVSignalKey{}, fmt.Errorf("%w: unparseable key instant", ErrBadCursor)
	}
	// Both identifiers go through their domain constructors rather than being
	// cast, so a cursor cannot smuggle a value the rest of the system considers
	// impossible into a query parameter.
	selection, err := domain.NewSelectionID(parts[2])
	if err != nil {
		return EVSignalKey{}, fmt.Errorf("%w: unparseable selection", ErrBadCursor)
	}
	book, err := domain.NewBookID(parts[3])
	if err != nil {
		return EVSignalKey{}, fmt.Errorf("%w: unparseable book", ErrBadCursor)
	}
	return EVSignalKey{
		ExpectedValuePercent: ev,
		QuoteObservedAt:      time.Unix(0, nanos).UTC(),
		SelectionID:          selection,
		BookID:               book,
	}, nil
}

// -----------------------------------------------------------------------------
// GET /signals/arbitrage
// -----------------------------------------------------------------------------

// handleArbitrageSignals serves the live arbitrage set.
//
// # Why this endpoint is deliberately conservative
//
// The phase-4 gate measured 68 apparent arbitrages across 1,065 records with the
// leg-age bound binding almost constantly: most cross-book "arbitrage" is not an
// opportunity, it is one book that has not moved yet. A firehose of stale-price
// findings is worse than no arbitrage feed at all, because it trains a reader to
// ignore the one that is real.
//
// So the three bounds below are REQUIRED IN SUBSTANCE, are named parameters with
// declared defaults rather than literals, and are ECHOED IN THE RESPONSE. The
// echo is the part that matters: a reader looking at three findings needs to
// know what was filtered out to reach three, and `bounds` is how they find out
// without reading this file.
//
// # Why there is no cursor
//
// The bounds make the live set small by construction and it turns over in
// seconds. A cursor would page through a list that no longer exists — the second
// page would be minted against rows the first page's keyset boundary no longer
// separates, and the client would render a mixture of two sets. If the set ever
// outgrows a page the answer is a tighter bound, not a cursor.
func (a *API) handleArbitrageSignals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bad := &badParams{}

	limit := parseLimit(q, bad)
	marketTypes := parseMarketTypes(q, bad)
	minReturnPercent := parseFloatParam(q, "min_return_percent", 0, 0, 10000, bad)
	maxLegAge := parseSecondsParam(q, "max_leg_age_seconds", defaultArbMaxLegAge, bad)
	maxSpread := parseSecondsParam(q, "max_spread_seconds", defaultArbMaxSpread, bad)
	minBooks := parseInt32Param(q, "min_distinct_books", 1, 1, 64, bad)

	now := a.now().UTC()
	observedAfter := parseLowerBound(q, "observed_after", now, defaultArbSignalWindow, maxFindingLookback, bad)

	league, err := a.leagueFilter(r.Context(), q, bad)
	if err != nil {
		failWith(w, r, a.log, "arbitrage signals: league", err)
		return
	}

	if bad.any() {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam, bad.items)
		return
	}

	findings, err := a.signals.ArbitrageSignals(r.Context(), ArbitrageQuery{
		LeagueID:          league.ID,
		MarketTypes:       marketTypes,
		ObservedAfter:     observedAfter,
		MaxLegAge:         maxLegAge,
		MaxObservedSpread: maxSpread,
		// The wire speaks percent because that is what a reader reads; the store
		// speaks the fraction the column holds. Converting here rather than
		// storing both is the same call migration 00009 makes about the margin
		// family: one stored number, every derivation computed from it.
		MinReturnFraction: minReturnPercent / 100,
		MinDistinctBooks:  minBooks,
		Limit:             limit,
	})
	if err != nil {
		failWith(w, r, a.log, "arbitrage signals", err)
		return
	}

	out := gen.ArbitrageSignalList{
		Data: make([]gen.ArbitrageSignal, 0, len(findings)),
		AsOf: now,
		Bounds: gen.ArbitrageBounds{
			MaxLegAgeSeconds: maxLegAge.Seconds(),
			MaxSpreadSeconds: maxSpread.Seconds(),
			MinReturnPercent: minReturnPercent,
			MinDistinctBooks: minBooks,
			ObservedAfter:    observedAfter,
		},
		// has_more false and next_cursor null, always. The envelope is shared
		// with every other list so a client has one shape to handle; the
		// contract that this list is not paged is stated in the spec and is not
		// something a client should discover by following a cursor into nothing.
		Page: wirePage(limit, false, ""),
	}
	for _, s := range findings {
		out.Data = append(out.Data, wireArbitrageSignal(s))
	}

	respond(w, http.StatusOK, out)
}

// -----------------------------------------------------------------------------
// GET /signals/steam
// -----------------------------------------------------------------------------

// handleSteamSignals serves the steam feed, most recent window first.
//
// RANKED BY RECENCY, NOT BY MAGNITUDE, and that is the one thing about this
// endpoint worth knowing. A steam alert is actionable only while the follower
// books are still catching up — that lag IS the opportunity — so an hour-old
// larger move is worth less than a fresh smaller one. `min_magnitude` is a
// filter and never the sort.
//
// There is no per-market cut on this path. A market-scoped view is a WINDOW read
// rather than a ranked page, and serving two different response contracts from
// one operation is worse than not serving the second one yet; when the event
// page wants steam it gets its own operation with its own envelope.
func (a *API) handleSteamSignals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bad := &badParams{}

	limit := parseLimit(q, bad)
	marketTypes := parseMarketTypes(q, bad)
	minMagnitude := parseFloatParam(q, "min_magnitude", 0, 0, 1, bad)
	minBooks := parseInt32Param(q, "min_participating_books", 2, 2, 256, bad)

	now := a.now().UTC()
	windowEndAfter := parseLowerBound(q, "window_end_after", now, defaultSteamSignalWindow, maxFindingLookback, bad)

	if bad.any() {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam, bad.items)
		return
	}

	scope := cursorScope("signals.steam",
		strconv.FormatInt(windowEndAfter.UnixNano(), 10),
		strconv.FormatFloat(minMagnitude, 'g', -1, 64),
		strconv.FormatInt(int64(minBooks), 10),
		strings.Join(stringsOfMarketTypes(marketTypes), ","),
	)

	var after *SteamSignalKey
	if raw := first(q, "cursor"); raw != "" {
		key, err := decodeSteamCursor(raw, scope)
		if err != nil {
			fail(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidCursor, msgInvalidCursor)
			return
		}
		after = &key
	}

	page, err := a.signals.SteamSignals(r.Context(), SteamQuery{
		WindowEndAfter:        windowEndAfter,
		MinMagnitude:          minMagnitude,
		MinParticipatingBooks: minBooks,
		MarketTypes:           marketTypes,
		After:                 after,
		Limit:                 limit,
	})
	if err != nil {
		failWith(w, r, a.log, "steam signals", err)
		return
	}

	out := gen.SteamSignalPage{
		Data: make([]gen.SteamSignal, 0, len(page.Signals)),
		AsOf: now,
	}
	for _, s := range page.Signals {
		out.Data = append(out.Data, wireSteamSignal(s))
	}
	out.Page = wirePage(limit, page.HasMore, nextSteamCursor(page.Signals, page.HasMore, scope))

	respond(w, http.StatusOK, out)
}

func nextSteamCursor(rows []SteamSignal, hasMore bool, scope uint64) string {
	if !hasMore || len(rows) == 0 {
		return ""
	}
	last := rows[len(rows)-1]
	return encodeSignalCursor(scope,
		strconv.FormatInt(last.WindowEnd.UTC().UnixNano(), 10),
		last.MarketID.String(),
		last.SelectionID.String(),
	)
}

func decodeSteamCursor(encoded string, scope uint64) (SteamSignalKey, error) {
	parts, err := decodeSignalCursor(encoded, scope, 3)
	if err != nil {
		return SteamSignalKey{}, err
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return SteamSignalKey{}, fmt.Errorf("%w: unparseable key instant", ErrBadCursor)
	}
	market, err := domain.NewMarketID(parts[1])
	if err != nil {
		return SteamSignalKey{}, fmt.Errorf("%w: unparseable market", ErrBadCursor)
	}
	selection, err := domain.NewSelectionID(parts[2])
	if err != nil {
		return SteamSignalKey{}, fmt.Errorf("%w: unparseable selection", ErrBadCursor)
	}
	return SteamSignalKey{WindowEnd: time.Unix(0, nanos).UTC(), MarketID: market, SelectionID: selection}, nil
}

// -----------------------------------------------------------------------------
// The analytics cursor codec
// -----------------------------------------------------------------------------

// encodeSignalCursor renders a keyset cursor over an arbitrary ordering tuple.
//
// # Why this is a second codec and not a generalisation of cursor.go's
//
// cursor.go encodes exactly one key shape — (scheduled_start, event id) — and
// wagers.go already declares a third for (placed_at, wager id). This one serves
// the three analytics feeds, whose keys have two, three and four components and
// whose leading component is a FLOAT on one of them. Rewriting the event codec
// to be generic would change the payload of a cursor that is currently in
// clients' hands for no benefit to them.
//
// The layout is deliberately DIFFERENT from cursor.go's — scope second rather
// than third — so the two cannot be confused by accident. They share the version
// byte, the separator and the base64url encoding, so there is one thing to
// version and one thing to change.
//
// A cursor from one feed presented to another decodes structurally and then
// fails the scope check, which is the fingerprint doing its job; every failure
// is the same [ErrBadCursor] and the caller learns nothing about which.
//
// # The float component
//
// strconv.FormatFloat(v, 'g', -1, 64) emits the shortest decimal that parses
// back to the SAME float64, so the boundary the cursor names is bit-identical to
// the one the previous page ended on. A rounded value would re-emit or skip
// every row whose expected value falls inside the rounding interval — and on a
// list ranked by expected value, ties at the boundary are the normal case, not a
// rare one.
func encodeSignalCursor(scope uint64, fields ...string) string {
	parts := make([]string, 0, len(fields)+2)
	parts = append(parts, cursorVersion, strconv.FormatUint(scope, 36))
	parts = append(parts, fields...)
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, "\x1f")))
}

// decodeSignalCursor parses a cursor and checks it against the scope of the
// query it was presented with, returning exactly `want` payload fields.
//
// The field count is checked BEFORE anything is parsed, so a caller indexes the
// result without a bounds check and a malformed cursor is a 400 rather than a
// panic.
func decodeSignalCursor(encoded string, scope uint64, want int) ([]string, error) {
	if len(encoded) > maxCursorLen {
		return nil, fmt.Errorf("%w: too long", ErrBadCursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: not base64url", ErrBadCursor)
	}

	parts := strings.Split(string(raw), "\x1f")
	if len(parts) != want+2 {
		return nil, fmt.Errorf("%w: wrong field count", ErrBadCursor)
	}
	if parts[0] != cursorVersion {
		return nil, fmt.Errorf("%w: unsupported version", ErrBadCursor)
	}
	got, err := strconv.ParseUint(parts[1], 36, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: unparseable scope", ErrBadCursor)
	}
	if got != scope {
		// The client changed a filter mid-listing. Reporting this rather than
		// serving the page is the entire point of the fingerprint.
		return nil, fmt.Errorf("%w: cursor belongs to a different query", ErrBadCursor)
	}
	return parts[2:], nil
}

// -----------------------------------------------------------------------------
// Shared parameter plumbing
// -----------------------------------------------------------------------------

// leagueFilter resolves the optional `league` slug.
//
// AN UNKNOWN SLUG IS A 400, NOT AN EMPTY RESULT, for parseBookFilter's reason
// and more sharply here: a typo that quietly returns no findings is
// indistinguishable from "there is nothing to report", and "there is nothing to
// report" is a real and frequent answer on this surface. An analytics endpoint
// that can fake it is an analytics endpoint nobody can trust.
//
// It returns a store error separately from a parameter error, so a database
// outage becomes a 500 rather than "no such league" — the same distinction
// notFoundOr draws, made here because the not-found half is a 400 rather than a
// 404: the league is a FILTER on this path, not the resource being addressed.
func (a *API) leagueFilter(ctx context.Context, q map[string][]string, bad *badParams) (League, error) {
	raw := first(q, "league")
	if raw == "" {
		return League{}, nil
	}
	slug, err := domain.NewSlug(raw)
	if err != nil {
		bad.add("league", "is not a valid slug")
		return League{}, nil
	}
	league, err := a.catalogue.LeagueBySlug(ctx, slug)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The slug is echoed only as a REASON about the parameter, never as
			// the parameter's value, so nothing client-controlled is reflected
			// verbatim into a field a client is likely to render as markup.
			bad.add("league", "names a league that does not exist")
			return League{}, nil
		}
		return League{}, fmt.Errorf("resolve league filter: %w", err)
	}
	return league, nil
}

// sortedBookIDs turns the book filter's set into the deterministic slice both
// the query and the cursor fingerprint need.
//
// SORTED, and that is load-bearing rather than tidy: the fingerprint is a hash
// over the filter, so `?book=a&book=b` and `?book=b&book=a` must produce the
// same scope or a client reordering its own parameters between pages would be
// told its cursor belongs to a different query.
func sortedBookIDs(filter map[domain.BookID]struct{}) []domain.BookID {
	if len(filter) == 0 {
		return nil
	}
	out := make([]domain.BookID, 0, len(filter))
	for id := range filter {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func stringsOfBooks(ids []domain.BookID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

func stringsOfMarketTypes(types []domain.MarketType) []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, t.String())
	}
	return out
}
