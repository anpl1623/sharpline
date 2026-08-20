package betting

import (
	"errors"
	"strings"
	"testing"
)

// TestErrorClasses pins the taxonomy an HTTP layer branches on. A sentinel that
// wrapped the wrong class would map a self-exclusion to 400 or a malformed slip
// to 403, and either is a customer being told to fix the wrong thing.
func TestErrorClasses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		class   error
		members []error
	}{
		{
			class: ErrInvalidSlip,
			members: []error{
				ErrSlipEmpty, ErrTooManyLegs, ErrDuplicateSelection, ErrDuplicateMarket,
				ErrLegCountForKind, ErrStakeNotPositive, ErrTeaserPoints, ErrTeaserMarketType,
				ErrRoundRobinSizes, ErrIdempotencyKeyRequired, ErrIdempotencyKeyInvalid,
				ErrSameGameUnsupported, ErrTeaserUnsupported,
			},
		},
		{
			class:   ErrNotPermitted,
			members: []error{ErrSelfExcluded, ErrAccountNotWagerable, ErrLimitExceeded},
		},
		{
			class:   ErrUnaffordable,
			members: []error{ErrInsufficientFunds},
		},
		{
			class: ErrMarketMoved,
			members: []error{
				ErrPriceMoved, ErrPriceMovedNotAccepted, ErrMarketNotOpen, ErrEventStarted,
				ErrStaleQuote, ErrQuoteUnavailable, ErrCashOutUnavailable,
			},
		},
	}

	classes := []error{ErrInvalidSlip, ErrNotPermitted, ErrUnaffordable, ErrMarketMoved}

	for _, tc := range tests {
		for _, member := range tc.members {
			t.Run(member.Error(), func(t *testing.T) {
				t.Parallel()
				if !errors.Is(member, tc.class) {
					t.Fatalf("%v is not in the %v class", member, tc.class)
				}
				// Exactly one class, so a caller's switch cannot match twice
				// and pick whichever arm happens to come first.
				for _, other := range classes {
					if other == tc.class {
						continue
					}
					if errors.Is(member, other) {
						t.Fatalf("%v is in both the %v and %v classes", member, tc.class, other)
					}
				}
			})
		}
	}
}

// TestErrAlreadyPlacedIsNotARefusal guards the one sentinel that is
// informational: it is the store's signal that a replay collided with
// wagers_pkey, and a caller that classified it as a customer failure would turn
// a correct idempotent replay into a 4xx.
func TestErrAlreadyPlacedIsNotARefusal(t *testing.T) {
	t.Parallel()

	for _, class := range []error{ErrInvalidSlip, ErrNotPermitted, ErrUnaffordable, ErrMarketMoved} {
		if errors.Is(ErrAlreadyPlaced, class) {
			t.Fatalf("ErrAlreadyPlaced is in the %v class; it is not a refusal", class)
		}
	}
}

// TestPriceMovedNotAcceptedImpliesPriceMoved is the wrapping that lets a caller
// match one sentinel and get both spellings of "the price moved".
func TestPriceMovedNotAcceptedImpliesPriceMoved(t *testing.T) {
	t.Parallel()

	if !errors.Is(ErrPriceMovedNotAccepted, ErrPriceMoved) {
		t.Fatal("ErrPriceMovedNotAccepted does not imply ErrPriceMoved")
	}
	if errors.Is(ErrPriceMoved, ErrPriceMovedNotAccepted) {
		t.Fatal("ErrPriceMoved wrongly implies the narrower ErrPriceMovedNotAccepted")
	}
}

// TestStructErrorsRender covers the three customer-facing messages. They are
// struct errors precisely so the NUMBERS survive as values, but the rendered
// text is what ends up in a log line and an audit row, so it has to name them.
func TestStructErrorsRender(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "a price move names both quotes",
			err: &PriceMove{
				SelectionID: "sel-1", BookID: "book-1",
				SeenDecimal: 1.91, SeenLine: "-3.5",
				CurrentDecimal: 1.87, CurrentLine: "-4",
			},
			want: []string{"sel-1", "book-1", "1.91", "-3.5", "1.87", "-4"},
		},
		{
			name: "a limit breach names the limit, the usage and the window",
			err: &LimitBreach{
				Kind: "stake", Period: "day",
				Limit: 20000, Used: 18000, Requested: 2001,
				WindowStart: "2026-08-19T18:30:00Z",
			},
			want: []string{"stake", "day", "20000", "18000", "2001", "2026-08-19T18:30:00Z"},
		},
		{
			name: "a shortfall names both amounts",
			err:  &ShortFall{Available: 999, Required: 1000},
			want: []string{"999", "1000"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.err.Error()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("Error() = %q, missing %q", got, want)
				}
			}
		})
	}
}
