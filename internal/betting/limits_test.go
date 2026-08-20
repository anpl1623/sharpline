package betting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
)

func TestWindowStart(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		period   auth.LimitPeriod
		want     time.Time
		windowed bool
	}{
		{
			name:     "a day is a rolling 24 hours",
			period:   auth.LimitPeriodDay,
			want:     time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC),
			windowed: true,
		},
		{
			name:     "a week is seven days",
			period:   auth.LimitPeriodWeek,
			want:     time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC),
			windowed: true,
		},
		{
			// AddDate normalises an overflowing day: 31 February becomes
			// 3 March. The window is then three days LONGER than a strict
			// month, which is the safe direction — a longer lookback sums more
			// spending and is therefore more restrictive.
			name:     "a month before 31 March normalises forward, making the window longer",
			period:   auth.LimitPeriodMonth,
			want:     time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC),
			windowed: true,
		},
		{
			name:     "a session period is not a money window",
			period:   auth.LimitPeriodSession,
			windowed: false,
		},
		{
			name:     "an unknown period is not a money window",
			period:   auth.LimitPeriodUnknown,
			windowed: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, windowed := windowStart(tc.period, at)
			if windowed != tc.windowed {
				t.Fatalf("windowStart(%s) windowed = %v, want %v", tc.period, windowed, tc.windowed)
			}
			if !tc.windowed {
				return
			}
			if !got.Equal(tc.want) {
				t.Fatalf("windowStart(%s) = %s, want %s", tc.period, got, tc.want)
			}
			if !got.Before(at) {
				t.Fatalf("windowStart(%s) = %s, which is not before %s", tc.period, got, at)
			}
		})
	}
}

func TestWindowStartMonthIsNeverShorterThanTwentyEightDays(t *testing.T) {
	t.Parallel()

	// A month window that came out SHORT would under-count spending and let a
	// customer past their limit, so the direction of the AddDate normalisation
	// is asserted across a whole year rather than at one date.
	for day := 1; day <= 31; day++ {
		for month := time.January; month <= time.December; month++ {
			at := time.Date(2026, month, 1, 12, 0, 0, 0, time.UTC).AddDate(0, 0, day-1)
			start, ok := windowStart(auth.LimitPeriodMonth, at)
			if !ok {
				t.Fatal("a month is a money window")
			}
			if span := at.Sub(start); span < 28*24*time.Hour {
				t.Fatalf("month window ending %s spans %s, shorter than 28 days", at, span)
			}
		}
	}
}

func TestEvaluateLimits(t *testing.T) {
	t.Parallel()

	limit := func(kind auth.LimitKind, period auth.LimitPeriod, amount domain.Money) Limit {
		return Limit{Kind: kind, Period: period, Amount: amount, EffectiveFrom: testNow.Add(-time.Hour)}
	}

	tests := []struct {
		name   string
		limits []Limit
		sums   map[domain.EntryKind]domain.Money
		stake  domain.Money
		want   error

		// wantUsed asserts the figure surfaced on the LimitBreach, because a
		// caller renders it and a wrong number there disagrees with the number
		// that said no.
		wantUsed int64
	}{
		{
			name:  "no limits admits any stake",
			stake: domain.Money(100_00),
		},
		{
			name:   "a stake well inside a stake limit",
			limits: []Limit{limit(auth.LimitKindStake, auth.LimitPeriodDay, 200_00)},
			sums:   map[domain.EntryKind]domain.Money{domain.EntryKindStake: -50_00},
			stake:  domain.Money(20_00),
		},
		{
			// The boundary a customer will test: a slip that takes them exactly
			// TO the limit is accepted.
			name:   "a stake exactly at the limit is accepted",
			limits: []Limit{limit(auth.LimitKindStake, auth.LimitPeriodDay, 200_00)},
			sums:   map[domain.EntryKind]domain.Money{domain.EntryKindStake: -180_00},
			stake:  domain.Money(20_00),
		},
		{
			name:   "one minor unit past the limit is refused",
			limits: []Limit{limit(auth.LimitKindStake, auth.LimitPeriodDay, 200_00)},
			sums:   map[domain.EntryKind]domain.Money{domain.EntryKindStake: -180_00},
			stake:  domain.Money(20_01),
			want:   ErrLimitExceeded, wantUsed: 180_00,
		},
		{
			// Stake entries DEBIT cash, so the ledger sum is negative and
			// usedForLimit negates it. A sign error here would report a
			// customer who has staked $180 as having used −$180, and the limit
			// would never fire.
			name:   "stake usage is the negated cash debit",
			limits: []Limit{limit(auth.LimitKindStake, auth.LimitPeriodWeek, 100_00)},
			sums:   map[domain.EntryKind]domain.Money{domain.EntryKindStake: -100_00},
			stake:  domain.Money(1),
			want:   ErrLimitExceeded, wantUsed: 100_00,
		},
		{
			// Grant entries CREDIT cash, so the sum is already positive and is
			// used as is. Placing a bet adds nothing to a grant limit.
			name:   "a grant limit is not moved by placing a bet",
			limits: []Limit{limit(auth.LimitKindGrant, auth.LimitPeriodMonth, 500_00)},
			sums:   map[domain.EntryKind]domain.Money{domain.EntryKindGrant: 500_00},
			stake:  domain.Money(50_00),
		},
		{
			name:   "a grant limit already breached refuses anyway",
			limits: []Limit{limit(auth.LimitKindGrant, auth.LimitPeriodMonth, 500_00)},
			sums:   map[domain.EntryKind]domain.Money{domain.EntryKindGrant: 500_01},
			stake:  domain.Money(50_00),
			want:   ErrLimitExceeded, wantUsed: 500_01,
		},
		{
			// loss = −(net of every non-grant cash entry). Staked $100, got
			// $30 back: a $70 net loss.
			name:   "a loss limit nets stakes against returns",
			limits: []Limit{limit(auth.LimitKindLoss, auth.LimitPeriodDay, 100_00)},
			sums: map[domain.EntryKind]domain.Money{
				domain.EntryKindStake:  -100_00,
				domain.EntryKindPayout: 30_00,
			},
			stake: domain.Money(20_00),
		},
		{
			name:   "a loss limit refuses the stake that would cross it",
			limits: []Limit{limit(auth.LimitKindLoss, auth.LimitPeriodDay, 100_00)},
			sums: map[domain.EntryKind]domain.Money{
				domain.EntryKindStake:  -100_00,
				domain.EntryKindPayout: 30_00,
			},
			stake: domain.Money(30_01),
			want:  ErrLimitExceeded, wantUsed: 70_00,
		},
		{
			// A grant must not raise loss headroom: including it would let a
			// customer top up their way past a control that exists precisely
			// for the moment they are tempted to.
			name:   "a grant does not raise loss headroom",
			limits: []Limit{limit(auth.LimitKindLoss, auth.LimitPeriodDay, 100_00)},
			sums: map[domain.EntryKind]domain.Money{
				domain.EntryKindStake: -100_00,
				domain.EntryKindGrant: 1_000_00,
			},
			stake: domain.Money(1),
			want:  ErrLimitExceeded, wantUsed: 100_00,
		},
		{
			// A customer who is up on the period produces a negative "used",
			// which is deliberately not clamped to zero — clamping would hide a
			// sign error behind a plausible number.
			name:   "a customer who is up cannot breach a loss limit",
			limits: []Limit{limit(auth.LimitKindLoss, auth.LimitPeriodDay, 100_00)},
			sums: map[domain.EntryKind]domain.Money{
				domain.EntryKindStake:  -50_00,
				domain.EntryKindPayout: 200_00,
			},
			stake: domain.Money(50_00),
		},
		{
			// The biconditional: a session limit is a duration, not a money
			// sum, and is skipped rather than read as minor units.
			name: "a session limit is skipped, not evaluated as zero",
			limits: []Limit{{
				Kind:          auth.LimitKindSession,
				Period:        auth.LimitPeriodSession,
				EffectiveFrom: testNow.Add(-time.Hour),
			}},
			stake: domain.Money(1_000_00),
		},
		{
			// A money kind on a session period is unstorable
			// (user_limits_session_period), so a row like this is corrupt.
			// Refusing the bet is the only safe reading.
			name:   "a money kind on a session period refuses rather than ignores",
			limits: []Limit{limit(auth.LimitKindStake, auth.LimitPeriodSession, 100_00)},
			stake:  domain.Money(1),
			want:   ErrLimitExceeded,
		},
		{
			// Tx.LimitsInForce promises not to return these; asserting the
			// promise turns a store bug into a refusal instead of into an
			// unlimited customer.
			name: "a not-yet-effective limit refuses rather than admits",
			limits: []Limit{{
				Kind:          auth.LimitKindStake,
				Period:        auth.LimitPeriodDay,
				Amount:        1_000_00,
				EffectiveFrom: testNow.Add(time.Hour),
			}},
			stake: domain.Money(1),
			want:  ErrLimitExceeded,
		},
		{
			name: "the tightest of several limits is the one that fires",
			limits: []Limit{
				limit(auth.LimitKindStake, auth.LimitPeriodMonth, 1_000_00),
				limit(auth.LimitKindStake, auth.LimitPeriodDay, 10_00),
			},
			sums:  map[domain.EntryKind]domain.Money{domain.EntryKindStake: -5_00},
			stake: domain.Money(6_00),
			want:  ErrLimitExceeded, wantUsed: 5_00,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tx := newFakeTx()
			tx.limits = tc.limits
			if tc.sums != nil {
				tx.sums = tc.sums
			}

			err := evaluateLimits(context.Background(), tx, testUser, tc.stake, testNow)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("evaluateLimits() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("evaluateLimits() = %v, want errors.Is(_, %v)", err, tc.want)
			}
			// A limit refusal is about the ACCOUNT, not the slip: it must be in
			// the 403 class and never the 400 one.
			if !errors.Is(err, ErrNotPermitted) {
				t.Fatalf("evaluateLimits() = %v, want it in the ErrNotPermitted class", err)
			}

			if tc.wantUsed == 0 {
				return
			}
			var breach *LimitBreach
			if !errors.As(err, &breach) {
				t.Fatalf("evaluateLimits() = %v, want a *LimitBreach a caller can render", err)
			}
			if breach.Used != tc.wantUsed {
				t.Fatalf("LimitBreach.Used = %d, want %d", breach.Used, tc.wantUsed)
			}
			if breach.WindowStart == "" {
				t.Error("LimitBreach.WindowStart is empty; a customer cannot be told when the headroom returns")
			}
		})
	}
}

func TestEvaluateLimitsWrapsStoreFailures(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("connection reset")

	t.Run("the limits read", func(t *testing.T) {
		t.Parallel()
		tx := newFakeTx()
		tx.limitsErr = sentinel
		err := evaluateLimits(context.Background(), tx, testUser, 100, testNow)
		if !errors.Is(err, sentinel) {
			t.Fatalf("evaluateLimits() = %v, want it to wrap %v", err, sentinel)
		}
		// A store failure must NOT read as a limit breach: telling a customer
		// they hit a self-imposed limit when the database was unreachable is a
		// lie about a responsible-gaming control.
		if errors.Is(err, ErrLimitExceeded) {
			t.Fatal("a store failure was reported as a limit breach")
		}
	})

	t.Run("the entry sum", func(t *testing.T) {
		t.Parallel()
		tx := newFakeTx()
		tx.limits = []Limit{{
			Kind:          auth.LimitKindStake,
			Period:        auth.LimitPeriodDay,
			Amount:        100_00,
			EffectiveFrom: testNow.Add(-time.Hour),
		}}
		tx.sumErr = sentinel
		err := evaluateLimits(context.Background(), tx, testUser, 100, testNow)
		if !errors.Is(err, sentinel) {
			t.Fatalf("evaluateLimits() = %v, want it to wrap %v", err, sentinel)
		}
		if errors.Is(err, ErrLimitExceeded) {
			t.Fatal("a store failure was reported as a limit breach")
		}
	})
}

func TestRequestedForLimit(t *testing.T) {
	t.Parallel()

	stake := domain.Money(50_00)
	tests := []struct {
		kind auth.LimitKind
		want domain.Money
	}{
		{kind: auth.LimitKindStake, want: stake},
		// A bet that has not settled is a loss until it is not: the money has
		// left the customer's cash the moment the ticket is booked.
		{kind: auth.LimitKindLoss, want: stake},
		// Placing a bet does not issue play money.
		{kind: auth.LimitKindGrant, want: domain.ZeroMoney},
		{kind: auth.LimitKindSession, want: domain.ZeroMoney},
	}

	for _, tc := range tests {
		t.Run(tc.kind.String(), func(t *testing.T) {
			t.Parallel()
			if got := requestedForLimit(tc.kind, stake); got != tc.want {
				t.Fatalf("requestedForLimit(%s) = %s, want %s", tc.kind, got, tc.want)
			}
		})
	}
}
