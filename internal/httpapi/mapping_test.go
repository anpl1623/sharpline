package httpapi

import (
	"testing"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
)

// THE ENUM BOUNDARY.
//
// openapi.yaml declares that each of its enums IS the domain's own String()
// vocabulary — "Exactly `domain.EventStatus`", and so on — and mapping.go relies
// on that by casting the domain string straight into the generated type. That
// cast cannot fail at compile time (both sides are strings), so a status added
// to the domain and not to the spec would serialise as a value the spec's enum
// does not contain, and a generated client would fail to decode it. Silently,
// and only for the one event in that state.
//
// These tests close that gap in BOTH directions: every domain member must be a
// valid generated member, and every generated member must parse back into the
// domain.
//
// oapi-codegen emits `Valid()` on every enum, which is what makes the first
// direction checkable at all.

func TestEventStatusVocabularyMatches(t *testing.T) {
	t.Parallel()

	domainMembers := []domain.EventStatus{
		domain.EventStatusScheduled, domain.EventStatusLive, domain.EventStatusSuspended,
		domain.EventStatusEnded, domain.EventStatusSettled, domain.EventStatusPostponed,
		domain.EventStatusCancelled,
	}
	for _, m := range domainMembers {
		if !gen.EventStatus(m.String()).Valid() {
			t.Errorf("domain.EventStatus %q is not a member of the spec's EventStatus enum", m)
		}
	}

	specMembers := []gen.EventStatus{
		gen.EventStatusScheduled, gen.EventStatusLive, gen.EventStatusSuspended,
		gen.EventStatusEnded, gen.EventStatusSettled, gen.EventStatusPostponed,
		gen.EventStatusCancelled,
	}
	if len(specMembers) != len(domainMembers) {
		t.Errorf("the spec has %d event statuses, the domain has %d", len(specMembers), len(domainMembers))
	}
	for _, m := range specMembers {
		if _, err := domain.ParseEventStatus(string(m)); err != nil {
			t.Errorf("spec EventStatus %q does not parse into the domain: %v", m, err)
		}
	}
}

func TestMarketTypeVocabularyMatches(t *testing.T) {
	t.Parallel()

	domainMembers := []domain.MarketType{
		domain.MarketTypeMoneyline, domain.MarketTypeSpread, domain.MarketTypeTotal,
		domain.MarketTypePlayerProp, domain.MarketTypeFutures,
	}
	for _, m := range domainMembers {
		if !gen.MarketType(m.String()).Valid() {
			t.Errorf("domain.MarketType %q is not a member of the spec's MarketType enum", m)
		}
	}
	for _, m := range []gen.MarketType{
		gen.Moneyline, gen.Spread, gen.Total, gen.PlayerProp, gen.Futures,
	} {
		if _, err := domain.ParseMarketType(string(m)); err != nil {
			t.Errorf("spec MarketType %q does not parse into the domain: %v", m, err)
		}
	}
}

func TestMarketStatusVocabularyMatches(t *testing.T) {
	t.Parallel()

	for _, m := range []domain.MarketStatus{
		domain.MarketStatusOpen, domain.MarketStatusSuspended, domain.MarketStatusClosed,
		domain.MarketStatusSettled, domain.MarketStatusVoided,
	} {
		if !gen.MarketStatus(m.String()).Valid() {
			t.Errorf("domain.MarketStatus %q is not a member of the spec's MarketStatus enum", m)
		}
	}
}

func TestSelectionRoleVocabularyMatches(t *testing.T) {
	t.Parallel()

	for _, m := range []domain.SelectionRole{
		domain.SelectionRoleHome, domain.SelectionRoleAway, domain.SelectionRoleDraw,
		domain.SelectionRoleOver, domain.SelectionRoleUnder, domain.SelectionRoleOutright,
	} {
		if !gen.SelectionRole(m.String()).Valid() {
			t.Errorf("domain.SelectionRole %q is not a member of the spec's SelectionRole enum", m)
		}
	}
}

func TestBookKindVocabularyMatches(t *testing.T) {
	t.Parallel()

	for _, m := range []domain.BookKind{domain.BookKindExternal, domain.BookKindSynthetic} {
		if !gen.BookKind(m.String()).Valid() {
			t.Errorf("domain.BookKind %q is not a member of the spec's BookKind enum", m)
		}
	}
}

func TestEventKindVocabularyMatches(t *testing.T) {
	t.Parallel()

	for _, m := range []domain.EventKind{domain.EventKindMatch, domain.EventKindOutright} {
		if !gen.EventKind(m.String()).Valid() {
			t.Errorf("domain.EventKind %q is not a member of the spec's EventKind enum", m)
		}
	}
}

// The four value sets migration 00005 defines with no domain counterpart. The
// migration is explicit that phase 5 "MUST define matching Go constants with
// String() / ParseX() pairs producing exactly these lowercase spellings"; those
// live in internal/auth, and these assert the API's enums agree with them.
func TestAccountStatusVocabularyMatches(t *testing.T) {
	t.Parallel()

	for _, m := range []auth.UserStatus{
		auth.UserStatusActive, auth.UserStatusSuspended,
		auth.UserStatusSelfExcluded, auth.UserStatusClosed,
	} {
		if !gen.AccountStatus(m.String()).Valid() {
			t.Errorf("auth.UserStatus %q is not a member of the spec's AccountStatus enum", m)
		}
	}
	for _, m := range []gen.AccountStatus{
		gen.AccountStatusActive, gen.AccountStatusSuspended,
		gen.AccountStatusSelfExcluded, gen.AccountStatusClosed,
	} {
		if _, err := auth.ParseUserStatus(string(m)); err != nil {
			t.Errorf("spec AccountStatus %q does not parse into internal/auth: %v", m, err)
		}
	}
}

func TestLimitVocabularyMatches(t *testing.T) {
	t.Parallel()

	for _, m := range []auth.LimitKind{
		auth.LimitKindGrant, auth.LimitKindStake, auth.LimitKindLoss, auth.LimitKindSession,
	} {
		if !gen.LimitKind(m.String()).Valid() {
			t.Errorf("auth.LimitKind %q is not a member of the spec's LimitKind enum", m)
		}
	}
	for _, m := range []auth.LimitPeriod{
		auth.LimitPeriodDay, auth.LimitPeriodWeek, auth.LimitPeriodMonth, auth.LimitPeriodSession,
	} {
		if !gen.LimitPeriod(m.String()).Valid() {
			t.Errorf("auth.LimitPeriod %q is not a member of the spec's LimitPeriod enum", m)
		}
	}

	// THERE IS NO `deposit`. This system has no deposits (CLAUDE.md §0), and a
	// limit that can never fire is an invitation to build the deposit flow §0
	// forbids. Migration 00005 makes the same argument at length.
	if gen.LimitKind("deposit").Valid() {
		t.Error("the spec declares a `deposit` limit kind; this system has no deposits")
	}
}

// TestOutrightEventHasNoCompetitorFields.
//
// An outright event has no home or away side AT ALL, which is a different fact
// from two empty ones. domain/event.go makes the same point: optional
// competitors on every event is how "the home team is empty" becomes a runtime
// surprise in the middle of a board render.
func TestOutrightEventHasNoCompetitorFields(t *testing.T) {
	t.Parallel()

	out := wireEvent(Event{
		ID:             "evt_futures_2027",
		LeagueID:       "nba",
		Kind:           domain.EventKindOutright,
		Name:           "2027 NBA Champion",
		ScheduledStart: testNow,
		Status:         domain.EventStatusScheduled,
		ObservedAt:     testNow,
	})
	if out.HomeCompetitor != nil || out.AwayCompetitor != nil {
		t.Error("an outright event carries competitor objects")
	}
	if out.Clock != nil {
		t.Error("an event that has not started carries a clock")
	}
	if out.Score != nil {
		t.Error("an event that has not started carries a score")
	}
}

// TestWagerVocabularyMatches closes the same gap over phase 8's four enums.
//
// The stakes are higher here than on the catalogue's. A wager status that
// serialised outside the spec's enum would break a generated client on exactly
// the tickets that had just changed state — the settled ones — which is the
// subset a customer is most likely to be looking at and the subset whose numbers
// are money.
func TestWagerVocabularyMatches(t *testing.T) {
	t.Parallel()

	t.Run("wager kind", func(t *testing.T) {
		t.Parallel()
		domainMembers := []domain.WagerKind{
			domain.WagerKindStraight, domain.WagerKindParlay,
			domain.WagerKindRoundRobin, domain.WagerKindTeaser,
		}
		specMembers := []gen.WagerKind{gen.Straight, gen.Parlay, gen.RoundRobin, gen.Teaser}
		for _, m := range domainMembers {
			if !gen.WagerKind(m.String()).Valid() {
				t.Errorf("domain.WagerKind %q is not a member of the spec's enum", m)
			}
		}
		if len(specMembers) != len(domainMembers) {
			t.Errorf("the spec has %d wager kinds, the domain has %d", len(specMembers), len(domainMembers))
		}
		for _, m := range specMembers {
			if _, err := domain.ParseWagerKind(string(m)); err != nil {
				t.Errorf("spec WagerKind %q does not parse into the domain: %v", m, err)
			}
		}
	})

	t.Run("wager status", func(t *testing.T) {
		t.Parallel()
		domainMembers := []domain.WagerStatus{
			domain.WagerStatusPlaced, domain.WagerStatusOpen, domain.WagerStatusWon,
			domain.WagerStatusLost, domain.WagerStatusVoid, domain.WagerStatusPush,
			domain.WagerStatusCashedOut,
		}
		specMembers := []gen.WagerStatus{
			gen.WagerStatusPlaced, gen.WagerStatusOpen, gen.WagerStatusWon,
			gen.WagerStatusLost, gen.WagerStatusVoid, gen.WagerStatusPush,
			gen.WagerStatusCashedOut,
		}
		for _, m := range domainMembers {
			if !gen.WagerStatus(m.String()).Valid() {
				t.Errorf("domain.WagerStatus %q is not a member of the spec's enum", m)
			}
		}
		if len(specMembers) != len(domainMembers) {
			t.Errorf("the spec has %d wager statuses, the domain has %d", len(specMembers), len(domainMembers))
		}
		for _, m := range specMembers {
			if _, err := domain.ParseWagerStatus(string(m)); err != nil {
				t.Errorf("spec WagerStatus %q does not parse into the domain: %v", m, err)
			}
		}
	})

	t.Run("leg status", func(t *testing.T) {
		t.Parallel()
		domainMembers := []domain.LegStatus{
			domain.LegStatusPending, domain.LegStatusWon, domain.LegStatusLost,
			domain.LegStatusVoid, domain.LegStatusPush,
		}
		specMembers := []gen.LegStatus{
			gen.LegStatusPending, gen.LegStatusWon, gen.LegStatusLost,
			gen.LegStatusVoid, gen.LegStatusPush,
		}
		for _, m := range domainMembers {
			if !gen.LegStatus(m.String()).Valid() {
				t.Errorf("domain.LegStatus %q is not a member of the spec's enum", m)
			}
		}
		if len(specMembers) != len(domainMembers) {
			t.Errorf("the spec has %d leg statuses, the domain has %d", len(specMembers), len(domainMembers))
		}
		for _, m := range specMembers {
			if _, err := domain.ParseLegStatus(string(m)); err != nil {
				t.Errorf("spec LegStatus %q does not parse into the domain: %v", m, err)
			}
		}
	})

	// Rounding is on the wire because a payout must be reproducible from the
	// stake and the price, and because a later repricing has to use the rule the
	// ticket was written under. A spelling that did not parse back would make a
	// stored ticket unreadable by its own settlement path.
	t.Run("rounding", func(t *testing.T) {
		t.Parallel()
		domainMembers := []domain.Rounding{
			domain.RoundHalfAwayFromZero, domain.RoundHalfToEven, domain.RoundTowardZero,
		}
		specMembers := []gen.Rounding{gen.HalfAwayFromZero, gen.HalfToEven, gen.TowardZero}
		for _, m := range domainMembers {
			if !gen.Rounding(m.String()).Valid() {
				t.Errorf("domain.Rounding %q is not a member of the spec's enum", m)
			}
		}
		if len(specMembers) != len(domainMembers) {
			t.Errorf("the spec has %d rounding modes, the domain has %d", len(specMembers), len(domainMembers))
		}
		for _, m := range specMembers {
			if _, err := domain.ParseRounding(string(m)); err != nil {
				t.Errorf("spec Rounding %q does not parse into the domain: %v", m, err)
			}
		}
		// The mode this API actually applies must itself be on the wire, or
		// every wager would report a rounding the spec does not admit.
		if !gen.Rounding(wagerRounding.String()).Valid() {
			t.Errorf("the house rounding policy %q is not a spec member", wagerRounding)
		}
	})
}

// TestErrorCodeVocabularyIsClosed.
//
// Every code [API.failBetting] can emit must be a member of the spec's enum. A
// code that is not would decode as an unknown string in a generated client and
// would land in whatever branch that client wrote for "something else" — which
// for a 403 self-exclusion is precisely the wrong branch.
func TestErrorCodeVocabularyIsClosed(t *testing.T) {
	t.Parallel()

	emitted := []gen.ErrorCode{
		gen.ErrorCodeSelfExcluded,
		gen.ErrorCodeAccountNotActive,
		gen.ErrorCodeInsufficientFunds,
		gen.ErrorCodeLimitExceeded,
		gen.ErrorCodePriceMoved,
		gen.ErrorCodeMarketUnavailable,
		gen.ErrorCodeCashOutUnavailable,
		gen.ErrorCodeUnprocessable,
		gen.ErrorCodeNotFound,
		gen.ErrorCodeBadRequest,
		gen.ErrorCodeInvalidCursor,
		gen.ErrorCodeInternal,
	}
	for _, code := range emitted {
		if !code.Valid() {
			t.Errorf("error code %q is emitted by the betting surface but is not a spec member", code)
		}
	}
}
