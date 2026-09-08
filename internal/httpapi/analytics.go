package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// Closing line value, and the public leaderboard.
//
// # Why these two live together
//
// They are the same measurement seen twice. `/account/clv` is one customer's
// evidence and `/leaderboard` is the ranking that evidence produces, and the
// exclusion rules have to be IDENTICAL across them or a customer will compute a
// mean from their own rows that does not match the one the board ranked them on.
// Keeping both in one file is what makes "voided and line-moved rows are shown
// and not counted" a single decision rather than two that drift.
//
// # Neither handler computes a CLV
//
// `settle` wrote every row, using odds.EvaluateCLV, against a closing snapshot
// defined precisely enough for the phase-12 SQL job to reproduce. This package
// pages, aggregates through the store's own SQL form of odds.AggregateCLV, and
// renders. There is no arithmetic here beyond a beat RATE, which is a division
// of two counts that are both on the wire so a reader can check it.

const (
	// defaultCLVWindow is how far back `/account/clv` looks when the caller does
	// not say. A quarter is long enough that a customer's CLV means something
	// and short enough that the default page is about their current form.
	defaultCLVWindow = 90 * 24 * time.Hour

	// defaultLeaderboardWindow matches it, so a customer comparing their own
	// aggregate against their leaderboard row is comparing the same window by
	// default. A board over "all time" would ossify: the same names would sit at
	// the top forever because nobody's early sample can ever be diluted.
	defaultLeaderboardWindow = 90 * 24 * time.Hour

	// defaultMinSettledWagers and defaultMinCLVSamples are the sample floors the
	// board applies when the caller does not choose.
	//
	// Twenty is a product decision and it is stated here rather than buried in a
	// query string so it is one edit. The reasoning is the one CLAUDE.md §6
	// gives for refusing a profit ranking: the failure mode is one lucky
	// maximum-stake bet at the top of the board, and a floor is what makes that
	// unrepresentable rather than merely unlikely. Twenty is not enough for CLV
	// to be statistically strong — nothing at this scale is — which is exactly
	// why the response reports the floor and the per-row sample counts instead
	// of implying a confidence it does not have.
	defaultMinSettledWagers = 20
	defaultMinCLVSamples    = 20

	// maxSampleFloor bounds the two floors. A floor above this admits nobody and
	// is more likely a client bug than an intention.
	maxSampleFloor = 1000000
)

// -----------------------------------------------------------------------------
// GET /account/clv
// -----------------------------------------------------------------------------

// handleAccountCLV serves the authenticated customer's closing line value.
//
// # The three parts of the response, and why all three are needed
//
// `data` is the evidence, `aggregate` is the number, and `by_league` is where
// the number came from. A page showing only the mean cannot be checked; a page
// showing only the rows makes the reader do the arithmetic the leaderboard
// already did, differently.
//
// # The exclusions, stated once
//
// `data` INCLUDES line-moved and voided rows. `aggregate` and `by_league`
// exclude both. odds/clv.go is explicit about the first: of a line-moved result
// it says "Show it next to the two lines in a user interface; never rank anyone
// by it" — this is the user interface, so the row travels with `line_moved: true`
// and both lines on it. A PUSH is neither excluded nor flagged, because a push
// is a settlement outcome rather than a data problem and excluding it would make
// CLV depend on the scoreboard.
func (a *API) handleAccountCLV(w http.ResponseWriter, r *http.Request) {
	user, ok := a.caller(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	bad := &badParams{}

	limit := parseLimit(q, bad)
	now := a.now().UTC()
	from, to := parseWindow(q, "graded_from", "graded_to", now, defaultCLVWindow, bad)

	if bad.any() {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam, bad.items)
		return
	}
	if !from.Before(to) {
		// 422 and not 400: the pair is syntactically valid and semantically
		// impossible, which is the distinction respond.go keeps. An empty page
		// would be worse than either, because it is indistinguishable from a
		// customer who has never had a leg graded.
		failInvalid(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable, msgUnprocessable,
			[]gen.InvalidParam{{Name: "graded_from", Reason: "must be before graded_to"}})
		return
	}

	// The user is in the fingerprint. A cursor is not a capability — the store
	// scopes every read by the caller's own id, so a cursor from another account
	// could not return that account's rows — but a cursor that decoded cleanly
	// against a DIFFERENT customer's listing would produce a page starting at a
	// row that customer never saw, which is a confusing outcome with no upside.
	scope := cursorScope("account.clv",
		user.String(),
		strconv.FormatInt(from.UnixNano(), 10),
	)

	var after *CLVKey
	if raw := first(q, "cursor"); raw != "" {
		key, err := decodeCLVCursor(raw, scope)
		if err != nil {
			fail(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidCursor, msgInvalidCursor)
			return
		}
		after = &key
	}

	page, err := a.clv.UserCLV(r.Context(), CLVQuery{
		UserID:     user,
		GradedFrom: from,
		After:      after,
		Limit:      limit,
	})
	if err != nil {
		failWith(w, r, a.log, "account clv: rows", err)
		return
	}

	window := CLVWindowQuery{UserID: user, From: from, To: to}

	aggregate, err := a.clv.UserCLVAggregate(r.Context(), window)
	if err != nil {
		failWith(w, r, a.log, "account clv: aggregate", err)
		return
	}
	byLeague, err := a.clv.UserCLVByLeague(r.Context(), window)
	if err != nil {
		failWith(w, r, a.log, "account clv: by league", err)
		return
	}

	out := gen.CLVResponse{
		Data:      make([]gen.CLVEntry, 0, len(page.Entries)),
		Aggregate: wireCLVAggregate(aggregate),
		ByLeague:  make([]gen.CLVLeagueSummary, 0, len(byLeague)),
		Window:    gen.AnalyticsWindow{From: from, To: to},
		AsOf:      now,
	}
	for _, e := range page.Entries {
		out.Data = append(out.Data, wireCLVEntry(e))
	}
	for _, s := range byLeague {
		out.ByLeague = append(out.ByLeague, wireCLVLeagueSummary(s))
	}
	out.Page = wirePage(limit, page.HasMore, nextCLVCursor(page.Entries, page.HasMore, scope))

	respond(w, http.StatusOK, out)
}

func nextCLVCursor(rows []CLVEntry, hasMore bool, scope uint64) string {
	if !hasMore || len(rows) == 0 {
		return ""
	}
	last := rows[len(rows)-1]
	return encodeSignalCursor(scope,
		strconv.FormatInt(last.GradedAt.UTC().UnixNano(), 10),
		last.LegID.String(),
	)
}

func decodeCLVCursor(encoded string, scope uint64) (CLVKey, error) {
	parts, err := decodeSignalCursor(encoded, scope, 2)
	if err != nil {
		return CLVKey{}, err
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return CLVKey{}, fmt.Errorf("%w: unparseable key instant", ErrBadCursor)
	}
	// Through the domain constructor, never a cast: it is what stops a cursor
	// smuggling a value the rest of the system considers impossible into a query.
	leg, err := domain.NewLegID(parts[1])
	if err != nil {
		return CLVKey{}, fmt.Errorf("%w: unparseable leg identifier", ErrBadCursor)
	}
	return CLVKey{GradedAt: time.Unix(0, nanos).UTC(), LegID: leg}, nil
}

// -----------------------------------------------------------------------------
// GET /leaderboard
// -----------------------------------------------------------------------------

// handleLeaderboard serves the public board.
//
// # It does not rank on profit, and that is the decision
//
// CLAUDE.md §6: "a public leaderboard on ROI and CLV, not raw profit". Raw
// profit ranks stake size and variance — the top of a profit board is whoever
// staked the most and got lucky, which is the opposite of the signal a
// leaderboard is supposed to carry. ROI is stake-normalised, so a customer who
// staked ten thousand and lost cannot outrank one who staked ten and won at any
// sample size, and that property holds structurally rather than by convention.
// CLV is scored against the market's own closing estimate rather than against
// the scoreboard, which makes it the better predictor of the two over the short
// histories this system will ever have.
//
// `basis` chooses the sort. BOTH measures are on every row either way, because
// the interesting rows are the ones where they disagree.
//
// # The sample floors are parameters and are echoed
//
// `minimum_samples` travels beside the rows. A ranking without its sample floor
// is not interpretable, and a reader cannot tell that one lucky bet has been
// excluded unless the number that excludes it is on screen.
//
// # No customer identity reaches the wire
//
// `users` holds an email address and nothing else — there is no display name in
// this system — so the store returns account identifiers and [publicHandle]
// derives a stable pseudonym at the rendering boundary. An empty board is a
// correct answer and renders as one: it means nobody has met the floor yet.
func (a *API) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bad := &badParams{}

	limit := parseLimit(q, bad)
	basis := parseLeaderboardBasis(q, bad)
	minWagers := parseInt64Param(q, "min_settled_wagers", defaultMinSettledWagers, 1, maxSampleFloor, bad)
	minCLV := parseInt64Param(q, "min_clv_samples", defaultMinCLVSamples, 1, maxSampleFloor, bad)

	now := a.now().UTC()
	from, to := parseWindow(q, "from", "to", now, defaultLeaderboardWindow, bad)

	if bad.any() {
		failInvalid(w, r, http.StatusBadRequest, gen.ErrorCodeInvalidParameter, msgInvalidParam, bad.items)
		return
	}
	if !from.Before(to) {
		failInvalid(w, r, http.StatusUnprocessableEntity, gen.ErrorCodeUnprocessable, msgUnprocessable,
			[]gen.InvalidParam{{Name: "from", Reason: "must be before to"}})
		return
	}

	rows, err := a.leaderboard.Leaderboard(r.Context(), LeaderboardQuery{
		Basis:            basis,
		MinSettledWagers: minWagers,
		MinCLVSamples:    minCLV,
		From:             from,
		To:               to,
		Limit:            limit,
	})
	if err != nil {
		failWith(w, r, a.log, "leaderboard", err)
		return
	}

	out := gen.LeaderboardPage{
		Data:  make([]gen.LeaderboardEntry, 0, len(rows)),
		Basis: gen.LeaderboardBasis(basis),
		MinimumSamples: gen.LeaderboardMinimums{
			SettledWagers: minWagers,
			ClvSamples:    minCLV,
		},
		Window: gen.AnalyticsWindow{From: from, To: to},
		AsOf:   now,
		// A leaderboard is a top-N by construction; paging to rank 400 is not a
		// thing anyone wants, and the tie-breaks that make the order total exist
		// to keep the visible top stable, not to support a walk to the bottom.
		Page: wirePage(limit, false, ""),
	}
	for i, row := range rows {
		// Rank is DENSE and 1-based over the returned order. It is computed here
		// rather than stored or returned by SQL because it is a position under
		// one basis, one window and one pair of floors — storing it would make
		// it wrong the moment any of those changed.
		out.Data = append(out.Data, wireLeaderboardEntry(row, int32(i)+1))
	}

	respond(w, http.StatusOK, out)
}
