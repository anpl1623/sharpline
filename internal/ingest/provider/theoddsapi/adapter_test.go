package theoddsapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// -----------------------------------------------------------------------------
// Cost
// -----------------------------------------------------------------------------

// TestAdapterCostImplementsTheRealBillingModel.
//
// The scheduler's token bucket charges this number BEFORE it spends, so an
// under-estimate does not fail — it burns the monthly quota early and the board
// goes dark in the third week. ADR 0003's entire arithmetic rests on two facts
// this table pins:
//
//   - a sweep costs markets × region-equivalents and does NOT scale with slate
//     size ("a 16-game NFL slate and a 1-game slate cost the same");
//   - a player prop is served only per event, so it DOES scale with slate size.
func TestAdapterCostImplementsTheRealBillingModel(t *testing.T) {
	nfl := mustLeagueID(t, "americanfootball_nfl")
	events := func(n int) []domain.EventID {
		out := make([]domain.EventID, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, mustEventID(t, fmt.Sprintf("%032x", i)))
		}
		return out
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		scope  provider.Scope
		want   int
	}{
		{
			name: "ADR 0003 recommended sweep: 3 markets, 1 region",
			scope: provider.Scope{League: nfl, Markets: []domain.MarketType{
				domain.MarketTypeMoneyline, domain.MarketTypeSpread, domain.MarketTypeTotal}},
			want: 3,
		},
		{
			name:   "cost is multiplicative in regions",
			mutate: func(c *Config) { c.Regions = []string{"us", "us2"} },
			scope: provider.Scope{League: nfl, Markets: []domain.MarketType{
				domain.MarketTypeMoneyline, domain.MarketTypeSpread, domain.MarketTypeTotal}},
			want: 6,
		},
		{
			name: "ten named bookmakers are one region-equivalent, which is why ADR 0003 prefers them",
			mutate: func(c *Config) {
				c.Regions = nil
				c.Bookmakers = []string{
					"draftkings", "fanduel", "betmgm", "caesars", "pointsbetus",
					"betrivers", "bovada", "barstool", "unibet", "williamhill_us",
				}
			},
			scope: provider.Scope{League: nfl, Markets: []domain.MarketType{
				domain.MarketTypeMoneyline, domain.MarketTypeSpread, domain.MarketTypeTotal}},
			want: 3,
		},
		{
			name: "a sweep does NOT scale with the number of events in scope",
			scope: provider.Scope{
				League:  nfl,
				Markets: []domain.MarketType{domain.MarketTypeMoneyline},
				Events:  events(16),
			},
			want: 1,
		},
		{
			name:   "player props scale with the slate, because they are per-event",
			mutate: func(c *Config) { c.PlayerPropMarkets = []string{"player_pass_tds", "player_rush_yds"} },
			scope: provider.Scope{
				League:  nfl,
				Markets: []domain.MarketType{domain.MarketTypePlayerProp},
				Events:  events(16),
			},
			want: 32,
		},
		{
			name:   "a mixed scope is billed per event on the per-event endpoint",
			mutate: func(c *Config) { c.PlayerPropMarkets = []string{"player_pass_tds"} },
			scope: provider.Scope{
				League:  nfl,
				Markets: []domain.MarketType{domain.MarketTypeMoneyline, domain.MarketTypePlayerProp},
				Events:  events(3),
			},
			want: 6,
		},
		{
			name:  "futures are the outrights market",
			scope: provider.Scope{League: nfl, Markets: []domain.MarketType{domain.MarketTypeFutures}},
			want:  1,
		},
		{
			name: "an unserveable scope still charges the provider's own floor",
			// The provider defaults `markets` to h2h when omitted, and bills for
			// it. Returning 0 would let a malformed scope spend a credit the
			// bucket never reserved.
			scope: provider.Scope{League: nfl, Markets: []domain.MarketType{domain.MarketTypePlayerProp}},
			want:  1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newProviderStub(t), tc.mutate)
			if got := h.Adapter.Cost(tc.scope); got != tc.want {
				t.Errorf("Cost = %d, want %d", got, tc.want)
			}
			// Cost must be pure: no request may leave the process.
			if n := len(h.Stub.seen()); n != 0 {
				t.Errorf("Cost issued %d HTTP requests; provider.Adapter says it performs no I/O", n)
			}
		})
	}
}

// TestAdapterCostReadsNoClock asserts the other half of the interface's promise
// about Cost by calling it across a clock that does not move and one that does.
func TestAdapterCostReadsNoClock(t *testing.T) {
	h := newHarness(t, newProviderStub(t), nil)
	scope := provider.Scope{
		League:  mustLeagueID(t, "americanfootball_nfl"),
		Markets: []domain.MarketType{domain.MarketTypeMoneyline, domain.MarketTypeSpread},
	}
	first := h.Adapter.Cost(scope)
	for i := 0; i < 100; i++ {
		if got := h.Adapter.Cost(scope); got != first {
			t.Fatalf("Cost changed from %d to %d across repeated calls", first, got)
		}
	}
}

// -----------------------------------------------------------------------------
// Quota
// -----------------------------------------------------------------------------

func TestAdapterQuotaPrefersTheProvidersOwnHeader(t *testing.T) {
	stub := newProviderStub(t)
	stub.route("/v4/sports/", json200(readGolden(t, goldenSports)))
	stub.remaining, stub.used, stub.last = 42_000, 58_000, 3
	h := newHarness(t, stub, nil)

	// Before the first request there is no reading, and reporting one would put
	// a number on the dashboard that no provider ever said.
	if q := h.Adapter.Quota(); q.Known {
		t.Errorf("quota is Known before any request: %s", q)
	}

	if _, err := h.Adapter.Catalogue(context.Background()); err != nil {
		t.Fatalf("Catalogue: %v", err)
	}

	q := h.Adapter.Quota()
	if !q.Known {
		t.Fatalf("quota is not Known after a response carrying %s", HeaderRequestsRemaining)
	}
	if q.Remaining != 42_000 {
		t.Errorf("Remaining = %d, want 42000 (the provider's own %s)", q.Remaining, HeaderRequestsRemaining)
	}
	if q.Limit != 100_000 {
		t.Errorf("Limit = %d, want the configured 100000", q.Limit)
	}
	if q.LastCost != 3 {
		t.Errorf("LastCost = %d, want 3 (%s)", q.LastCost, HeaderRequestsLast)
	}
	if frac, ok := q.Fraction(); !ok || frac != 0.42 {
		t.Errorf("Fraction = %v (ok=%v), want 0.42", frac, ok)
	}

	// The two contract series must carry the provider's number, and the
	// from_provider flag must say the header is what is live.
	if got := testutil.ToFloat64(h.Metrics.quotaRemaining.WithLabelValues(ProviderSlug)); got != 42_000 {
		t.Errorf("sharpline_provider_quota_remaining = %v, want 42000", got)
	}
	if got := testutil.ToFloat64(h.Metrics.quotaLimit.WithLabelValues(ProviderSlug)); got != 100_000 {
		t.Errorf("sharpline_provider_quota_limit = %v, want 100000", got)
	}
	if got := testutil.ToFloat64(h.Metrics.quotaFromProvider.WithLabelValues(ProviderSlug)); got != 1 {
		t.Errorf("sharpline_provider_quota_from_provider = %v, want 1", got)
	}
}

// TestAdapterQuotaFallsBackToLocalEstimate covers the case ADR 0003 calls out:
// the header is authoritative WHEN PRESENT, and the fallback must be visible
// rather than silent.
func TestAdapterQuotaFallsBackToLocalEstimate(t *testing.T) {
	body := stripDocsElision(t, readGolden(t, goldenOdds))
	stub := newProviderStub(t)
	stub.omitQuota = true
	stub.route("/v4/sports/americanfootball_nfl/odds/", json200(body))
	h := newHarness(t, stub, nil)

	if q := h.Adapter.Quota(); q.Known {
		t.Errorf("quota is Known before any spend: %s", q)
	}

	if _, err := h.Adapter.Fetch(context.Background(), provider.Scope{
		League:  mustLeagueID(t, "americanfootball_nfl"),
		Markets: []domain.MarketType{domain.MarketTypeMoneyline, domain.MarketTypeSpread},
	}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// The sweep asked for two markets in one region, so it cost two credits.
	q := h.Adapter.Quota()
	if !q.Known {
		t.Fatalf("quota is not Known after spending credits; the local estimate is a real measurement " +
			"of this process's own spend, not a restatement of the configured budget")
	}
	if q.Remaining != 99_998 {
		t.Errorf("Remaining = %d, want 99998 (100000 budget minus a 2-credit sweep)", q.Remaining)
	}

	// The absence of the header must be COUNTED, or a provider change that
	// stopped sending it would silently move the gauge onto an estimate.
	if got := testutil.ToFloat64(h.Metrics.quotaHeaderMissing.WithLabelValues(ProviderSlug)); got != 1 {
		t.Errorf("sharpline_provider_quota_header_missing_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.Metrics.quotaFromProvider.WithLabelValues(ProviderSlug)); got != 0 {
		t.Errorf("sharpline_provider_quota_from_provider = %v, want 0 — a gauge running on an estimate "+
			"must be distinguishable from one running on the provider's number", got)
	}
}

// TestAdapterQuotaPerformsNoIO is the interface's own words: "It performs NO I/O
// — asking a provider how much quota is left would itself spend quota."
func TestAdapterQuotaPerformsNoIO(t *testing.T) {
	h := newHarness(t, newProviderStub(t), nil)
	for i := 0; i < 50; i++ {
		_ = h.Adapter.Quota()
	}
	if n := len(h.Stub.seen()); n != 0 {
		t.Errorf("Quota issued %d HTTP requests", n)
	}
}

// -----------------------------------------------------------------------------
// Failure modes
// -----------------------------------------------------------------------------

func statusHandler(status int, body string, headers map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// TestAdapterDistinguishesFailureModes.
//
// Each row maps to a DIFFERENT caller decision, which is the entire reason the
// error vocabulary has seven sentinels instead of one. Collapsing any two of
// them produces a specific, expensive misbehaviour, named in the row.
func TestAdapterDistinguishesFailureModes(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		headers     map[string]string
		remaining   int64
		omitQuota   bool
		want        provider.Disposition
		sentinel    error
		code        string
		retryAfter  time.Duration
		wantRequest int
		why         string
	}{
		{
			name: "401 with a key code is a deployment error a human must fix", status: 401,
			body: `{"message":"INVALID_KEY"}`, remaining: 99_000,
			want: provider.DispositionFatal, sentinel: provider.ErrUnauthorized, code: CodeInvalidKey,
			wantRequest: 1,
			why:         "retrying a wrong key for ever produces 401s until someone looks at a dashboard",
		},
		{
			name: "401 with OUT_OF_USAGE_CREDITS is quota exhaustion, not a bad key", status: 401,
			body: `{"message":"OUT_OF_USAGE_CREDITS"}`, remaining: 0,
			want: provider.DispositionQuotaExhausted, sentinel: provider.ErrQuotaExhausted,
			code: CodeOutOfUsageCredits, wantRequest: 1,
			why: "ADR 0003 requires this state fail loudly to a visible degraded mode, with its own alert",
		},
		{
			name: "401 with no code but a zero quota header is still quota exhaustion", status: 401,
			body: `unauthorized`, remaining: 0,
			want: provider.DispositionQuotaExhausted, sentinel: provider.ErrQuotaExhausted,
			wantRequest: 1,
			why:         "a key that is invalid has no quota to report, so a reported zero means the credits went",
		},
		{
			name: "401 with neither signal falls back to the safer bad-key reading", status: 401,
			body: `unauthorized`, omitQuota: true,
			want: provider.DispositionFatal, sentinel: provider.ErrUnauthorized, wantRequest: 1,
			why: "mislabelling a bad key as quota would have ingest wait for a monthly reset that changes nothing",
		},
		{
			name: "422 is a request this package built wrongly", status: 422,
			body: `{"message":"INVALID_MARKET"}`, remaining: 99_000,
			want: provider.DispositionFatal, sentinel: provider.ErrProviderRejected, code: "INVALID_MARKET",
			wantRequest: 1,
			why:         "a client error that repeats is a client error; retrying it spends credits to learn nothing",
		},
		{
			name: "404 is one event, not the league", status: 404,
			body: `{"message":"EVENT_NOT_FOUND"}`, remaining: 99_000,
			want: provider.DispositionFatal, sentinel: provider.ErrNotFound, code: CodeEventNotFound,
			wantRequest: 1,
		},
		{
			name: "429 is retryable and carries the provider's own Retry-After", status: 429,
			body: `{"message":"EXCEEDED_FREQ_LIMIT"}`, remaining: 99_000,
			headers: map[string]string{"Retry-After": "7"},
			want:    provider.DispositionRetryable, sentinel: provider.ErrRateLimited,
			code: CodeExceededFreqLimit, retryAfter: 7 * time.Second, wantRequest: DefaultMaxAttempts,
			why: "the provider is the only party that knows when its own throttle window closes",
		},
		{
			name: "5xx is their fault and probably transient", status: 500,
			body: `internal error`, remaining: 99_000,
			want: provider.DispositionRetryable, sentinel: provider.ErrUnavailable,
			wantRequest: DefaultMaxAttempts,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newProviderStub(t)
			stub.omitQuota = tc.omitQuota
			stub.remaining = tc.remaining
			stub.fallback = statusHandler(tc.status, tc.body, tc.headers)
			h := newHarness(t, stub, nil)

			_, err := h.Adapter.Fetch(context.Background(), provider.Scope{
				League:  mustLeagueID(t, "americanfootball_nfl"),
				Markets: []domain.MarketType{domain.MarketTypeMoneyline},
			})
			if err == nil {
				t.Fatalf("Fetch succeeded against http %d", tc.status)
			}
			if got := provider.Classify(err); got != tc.want {
				t.Errorf("disposition = %s, want %s. %s", got, tc.want, tc.why)
			}
			if !errors.Is(err, tc.sentinel) {
				t.Errorf("error does not unwrap to %v: %v", tc.sentinel, err)
			}
			if tc.code != "" {
				if got := ErrorCode(err); got != tc.code {
					t.Errorf("provider error code = %q, want %q", got, tc.code)
				}
			}
			if tc.retryAfter > 0 {
				got, ok := RetryAfter(err)
				if !ok || got != tc.retryAfter {
					t.Errorf("Retry-After = %s (ok=%v), want %s", got, ok, tc.retryAfter)
				}
			}
			if got := len(h.Stub.seen()); got != tc.wantRequest {
				t.Errorf("issued %d requests, want %d", got, tc.wantRequest)
			}
			assertNoKey(t, "the error", err.Error())
		})
	}
}

// TestAdapterNeverRetriesQuotaExhaustion.
//
// The credits return at the monthly reset, not after a backoff. A poller that
// retried into an exhausted quota would generate 401s for the rest of the
// month, each one a request against the frequency limit.
func TestAdapterNeverRetriesQuotaExhaustion(t *testing.T) {
	stub := newProviderStub(t)
	stub.remaining = 0
	stub.fallback = statusHandler(401, `{"message":"OUT_OF_USAGE_CREDITS"}`, nil)
	h := newHarness(t, stub, nil)

	_, err := h.Adapter.Fetch(context.Background(), provider.Scope{
		League:  mustLeagueID(t, "americanfootball_nfl"),
		Markets: []domain.MarketType{domain.MarketTypeMoneyline},
	})
	if !provider.IsQuotaExhausted(err) {
		t.Fatalf("error is not classified as quota exhausted: %v", err)
	}
	if got := len(h.Stub.seen()); got != 1 {
		t.Errorf("issued %d requests into an exhausted quota, want exactly 1", got)
	}
	if got := testutil.ToFloat64(h.Metrics.retries.WithLabelValues(ProviderSlug, EndpointOdds, "rate_limited")); got != 0 {
		t.Errorf("quota exhaustion was retried %v times", got)
	}
	// The provider's own zero must reach the gauge the exhaustion alert reads.
	if got := testutil.ToFloat64(h.Metrics.quotaRemaining.WithLabelValues(ProviderSlug)); got != 0 {
		t.Errorf("sharpline_provider_quota_remaining = %v, want 0 — ProviderQuotaExhausted alerts on == 0", got)
	}
}

// TestAdapterFailsToQuotaRatherThanCallingWhenTheLocalBudgetIsGone.
//
// ADR 0003 requirement 5: "The limiter must fail to synthetic, not fail to
// stale. When the budget is exhausted the correct behaviour is a loud alert and
// a visible degraded state — never a board that silently shows hour-old prices
// as if they were live." Two properties follow, and both are asserted: no
// request is issued, and no snapshot comes back.
func TestAdapterFailsToQuotaRatherThanCallingWhenTheLocalBudgetIsGone(t *testing.T) {
	stub := newProviderStub(t)
	stub.route("/v4/sports/americanfootball_nfl/odds/",
		json200(stripDocsElision(t, readGolden(t, goldenOdds))))
	h := newHarness(t, stub, func(c *Config) {
		// A budget too small to ever afford a two-market sweep.
		c.Limiter.MonthlyCredits = 1
		c.Limiter.CreditBurst = 1
	})

	snap, err := h.Adapter.Fetch(context.Background(), provider.Scope{
		League:  mustLeagueID(t, "americanfootball_nfl"),
		Markets: []domain.MarketType{domain.MarketTypeMoneyline, domain.MarketTypeSpread},
	})
	if err == nil {
		t.Fatalf("Fetch spent credits it did not have")
	}
	if !provider.IsQuotaExhausted(err) {
		t.Errorf("disposition = %s, want quota_exhausted: %v", provider.Classify(err), err)
	}
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("error does not unwrap to ErrBudgetExhausted: %v", err)
	}
	if len(snap.Events) != 0 {
		t.Errorf("a refused fetch returned %d events; it must return NOTHING rather than something stale",
			len(snap.Events))
	}
	if got := len(h.Stub.seen()); got != 0 {
		t.Errorf("issued %d requests after the local budget refused; no credit should have been at risk", got)
	}
	if got := testutil.ToFloat64(h.Metrics.throttled.WithLabelValues(ProviderSlug, "credits")); got != 1 {
		t.Errorf("sharpline_provider_throttled_total{limiter=\"credits\"} = %v, want 1", got)
	}
}

// TestAdapterFrequencyRefusalIsRateLimitedNotQuotaExhausted.
//
// Both refusals come from the same limiter and both mean "not now", but the
// waits differ by orders of magnitude: the frequency bucket refills in
// milliseconds, the credit bucket over the budget window. Collapsing them would
// fire ProviderQuotaExhausted for a burst of sweeps arriving together.
func TestAdapterFrequencyRefusalIsRateLimitedNotQuotaExhausted(t *testing.T) {
	stub := newProviderStub(t)
	stub.route("/v4/sports/", json200(readGolden(t, goldenSports)))
	h := newHarness(t, stub, func(c *Config) {
		c.Limiter.RequestsPerSecond = 0.001
		c.Limiter.RequestBurst = 1
	})

	if _, err := h.Adapter.Catalogue(context.Background()); err != nil {
		t.Fatalf("first Catalogue: %v", err)
	}
	// The clock is frozen, so the frequency bucket cannot refill.
	_, err := h.Adapter.Catalogue(context.Background())
	if err == nil {
		t.Fatalf("second Catalogue was not throttled")
	}
	if got, want := provider.Classify(err), provider.DispositionRetryable; got != want {
		t.Errorf("disposition = %s, want %s", got, want)
	}
	if !errors.Is(err, provider.ErrRateLimited) {
		t.Errorf("error does not unwrap to provider.ErrRateLimited: %v", err)
	}
	if provider.IsQuotaExhausted(err) {
		t.Errorf("a frequency refusal was reported as quota exhaustion, which would fire " +
			"ProviderQuotaExhausted for a burst of sweeps")
	}
	if _, ok := provider.RetryAfter(err); !ok {
		t.Errorf("a frequency refusal carries no wait; the scheduler has nothing to back off by")
	}
}

// TestAdapterHonoursCallerDeadline covers CLAUDE.md §12's "every external call
// has a timeout" from the caller's side.
func TestAdapterHonoursCallerDeadline(t *testing.T) {
	release := make(chan struct{})
	stub := newProviderStub(t)
	stub.fallback = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	t.Cleanup(func() { close(release) })
	h := newHarness(t, stub, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := h.Adapter.Catalogue(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Catalogue returned after the deadline passed")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Catalogue took %s to honour a 50ms deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error does not unwrap to context.DeadlineExceeded: %v", err)
	}
	// A deadline on one poll says nothing about the next one.
	if got, want := provider.Classify(err), provider.DispositionRetryable; got != want {
		t.Errorf("disposition = %s, want %s", got, want)
	}
	assertNoKey(t, "the timeout error", err.Error())
}

// TestAdapterCancellationIsNotAProviderFailure.
func TestAdapterCancellationIsNotAProviderFailure(t *testing.T) {
	release := make(chan struct{})
	stub := newProviderStub(t)
	stub.fallback = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	t.Cleanup(func() { close(release) })
	h := newHarness(t, stub, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := h.Adapter.Catalogue(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not unwrap to context.Canceled: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Snapshot shape
// -----------------------------------------------------------------------------

// TestAdapterEmptySlateIsNotAFailure.
//
// An out-of-season league returns `[]`, which the provider documents as not
// billable. The correct answer is an empty snapshot, not an error: an empty
// board with a correct empty state is right, and a spurious failure would drive
// the scheduler's error backoff for a league that is simply not playing.
func TestAdapterEmptySlateIsNotAFailure(t *testing.T) {
	stub := newProviderStub(t)
	stub.route("/v4/sports/americanfootball_nfl/odds/", json200([]byte(`[]`)))
	stub.last = 0
	h := newHarness(t, stub, nil)

	snap, err := h.Adapter.Fetch(context.Background(), provider.Scope{
		League:  mustLeagueID(t, "americanfootball_nfl"),
		Markets: []domain.MarketType{domain.MarketTypeMoneyline},
	})
	if err != nil {
		t.Fatalf("an empty slate was reported as a failure: %v", err)
	}
	if err := snap.Validate(); err != nil {
		t.Fatalf("empty snapshot does not validate: %v", err)
	}
	if len(snap.Events) != 0 {
		t.Errorf("empty slate produced %d events", len(snap.Events))
	}
	if snap.FetchedAt.IsZero() {
		t.Errorf("empty snapshot carries no FetchedAt; provider.Snapshot.Validate requires one")
	}
	if got, want := snap.Provider, provider.NameTheOddsAPI; got != want {
		t.Errorf("snapshot provider = %q, want %q", got, want)
	}

	// A response with no events is documented as not charged. The reservation
	// must therefore be refunded, or an out-of-season league would drain the
	// month polling nothing.
	if got := h.Adapter.client.Quota().LocalEstimate; got != 100_000 {
		t.Errorf("local estimate = %d, want 100000 — the provider does not bill a response with no "+
			"events, so the reservation must come back", got)
	}
}

// TestAdapterNarrowsToTheScopesEvents.
//
// A sweep costs the same whatever the slate size, so narrowing happens here
// rather than at the provider — but provider.Snapshot.Validate rejects an event
// outside the scope, so the filtering has to actually happen.
func TestAdapterNarrowsToTheScopesEvents(t *testing.T) {
	body := stripDocsElision(t, readGolden(t, goldenOdds))
	stub := newProviderStub(t)
	stub.route("/v4/sports/americanfootball_nfl/odds/", json200(body))
	h := newHarness(t, stub, nil)

	other := mustEventID(t, "00000000000000000000000000000000")
	snap, err := h.Adapter.Fetch(context.Background(), provider.Scope{
		League:  mustLeagueID(t, "americanfootball_nfl"),
		Markets: []domain.MarketType{domain.MarketTypeMoneyline},
		Events:  []domain.EventID{other},
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := snap.Validate(); err != nil {
		t.Fatalf("narrowed snapshot does not validate: %v", err)
	}
	if len(snap.Events) != 0 {
		t.Errorf("snapshot carries %d events outside the requested scope", len(snap.Events))
	}
	// Narrowing must not cost extra: one sweep, not one call per event.
	if got := len(h.Stub.seen()); got != 1 {
		t.Errorf("issued %d requests for a narrowed featured-market scope, want 1 sweep", got)
	}
}

// TestAdapterRejectsAnInvalidScopeWithoutSpendingACredit.
func TestAdapterRejectsAnInvalidScopeWithoutSpendingACredit(t *testing.T) {
	h := newHarness(t, newProviderStub(t), nil)

	_, err := h.Adapter.Fetch(context.Background(), provider.Scope{
		League: mustLeagueID(t, "americanfootball_nfl"),
		// No markets: Scope.Validate refuses it.
	})
	if err == nil {
		t.Fatalf("an empty market list was accepted")
	}
	if !errors.Is(err, provider.ErrInvalidScope) {
		t.Errorf("error does not unwrap to provider.ErrInvalidScope: %v", err)
	}
	if got, want := provider.Classify(err), provider.DispositionFatal; got != want {
		t.Errorf("disposition = %s, want %s", got, want)
	}
	if n := len(h.Stub.seen()); n != 0 {
		t.Errorf("a malformed scope issued %d requests", n)
	}
}

// TestAdapterRefusesPlayerPropsWithoutConfiguration.
//
// ADR 0003 scenario E's verdict is that props are a 5M-tier feature. Silently
// serving them would drain the recommended tier in an afternoon, so the refusal
// is explicit and names the variable that enables them.
func TestAdapterRefusesPlayerPropsWithoutConfiguration(t *testing.T) {
	h := newHarness(t, newProviderStub(t), nil)

	_, err := h.Adapter.Fetch(context.Background(), provider.Scope{
		League:  mustLeagueID(t, "americanfootball_nfl"),
		Markets: []domain.MarketType{domain.MarketTypePlayerProp},
		Events:  []domain.EventID{mustEventID(t, "a512a48a58c4329048174217b2cc7ce0")},
	})
	if err == nil {
		t.Fatalf("a player-prop scope was served with no prop markets configured")
	}
	if !errors.Is(err, provider.ErrNotSupported) {
		t.Errorf("error does not unwrap to provider.ErrNotSupported: %v", err)
	}
	if !strings.Contains(err.Error(), envPlayerPropMarkets) {
		t.Errorf("the refusal does not name %s, so an operator cannot act on it: %v",
			envPlayerPropMarkets, err)
	}
	if n := len(h.Stub.seen()); n != 0 {
		t.Errorf("a refused prop scope issued %d requests", n)
	}
}

// TestAdapterIdentity pins the three names that are one contract: the interface
// constant, the metric label, and the Kafka topic suffix.
func TestAdapterIdentity(t *testing.T) {
	h := newHarness(t, newProviderStub(t), nil)
	if got, want := h.Adapter.Name(), provider.NameTheOddsAPI; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
	if got, want := h.Adapter.Name().String(), ProviderSlug; got != want {
		t.Errorf("Name = %q, want the metric label value %q", got, want)
	}
	if _, err := provider.NewName(ProviderSlug); err != nil {
		t.Errorf("ProviderSlug %q is not a legal provider name: %v", ProviderSlug, err)
	}
}

// TestAdapterMetricNamesAreTheContract.
//
// deploy/observability was written in phase 0 against services that did not yet
// exist, so for this package the names are a SPECIFICATION to satisfy. Two of
// them are divided by an alert with no on()/ignoring(), so they must also carry
// an identical label set or the division matches nothing and the alert silently
// never fires.
func TestAdapterMetricNamesAreTheContract(t *testing.T) {
	stub := newProviderStub(t)
	stub.route("/v4/sports/americanfootball_nfl/odds/",
		json200(stripDocsElision(t, readGolden(t, goldenOdds))))
	h := newHarness(t, stub, nil)

	if _, err := h.Adapter.Fetch(context.Background(), provider.Scope{
		League:  mustLeagueID(t, "americanfootball_nfl"),
		Markets: []domain.MarketType{domain.MarketTypeMoneyline, domain.MarketTypeSpread},
	}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	families, err := h.Reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	labels := map[string][]string{}
	present := map[string]bool{}
	for _, f := range families {
		present[f.GetName()] = true
		for _, m := range f.GetMetric() {
			var names []string
			for _, l := range m.GetLabel() {
				names = append(names, l.GetName())
			}
			labels[f.GetName()] = names
			break
		}
	}

	for _, name := range []string{
		"sharpline_provider_quota_remaining",
		"sharpline_provider_quota_limit",
		"sharpline_provider_requests_total",
		"sharpline_provider_credits_used_total",
		"sharpline_provider_mapping_dropped_total",
	} {
		if !present[name] {
			t.Errorf("%s is not exported; the dashboard and alerts reference it by name", name)
		}
	}

	// ProviderQuotaLow divides one by the other with no on()/ignoring().
	if a, b := labels["sharpline_provider_quota_remaining"], labels["sharpline_provider_quota_limit"]; !equalStrings(a, b) {
		t.Errorf("quota_remaining labels %v and quota_limit labels %v differ; ProviderQuotaLow divides "+
			"one by the other with no on()/ignoring(), so a mismatch makes the alert never fire", a, b)
	}

	// The drop counter's label values must stay inside the closed set.
	allowed := map[string]bool{}
	for _, r := range DropReasons() {
		allowed[r] = true
	}
	for _, f := range families {
		if f.GetName() != "sharpline_provider_mapping_dropped_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && !allowed[l.GetValue()] {
					t.Errorf("mapping_dropped_total carries reason=%q, which is outside DropReasons()",
						l.GetValue())
				}
			}
		}
	}
}

// TestAdapterIsSafeForConcurrentUse.
//
// provider.Adapter states it as a contract: "the scheduler runs one goroutine
// per league window (CLAUDE.md §2's 'a goroutine per poller'), and they share
// one adapter." The adapter holds two shared maps — the league registry and the
// book registry — and both are written from Fetch, so a data race here is not
// hypothetical. Run under -race, this is the test that finds it.
func TestAdapterIsSafeForConcurrentUse(t *testing.T) {
	body := stripDocsElision(t, readGolden(t, goldenOdds))
	stub := newProviderStub(t)
	stub.route("/v4/sports/", json200(readGolden(t, goldenSports)))
	stub.route("/v4/sports/americanfootball_nfl/odds/", json200(body))
	h := newHarness(t, stub, func(c *Config) {
		// Wide enough that the limiter is not the thing under test. The rate
		// stays at the provider's documented ceiling because Validate refuses
		// anything above it; the BURST is what has to absorb the fan-in, and
		// the harness clock is frozen so nothing refills.
		c.Limiter.RequestsPerSecond = DocumentedRequestsPerSecond
		c.Limiter.RequestBurst = 1000
	})

	scope := provider.Scope{
		League:  mustLeagueID(t, "americanfootball_nfl"),
		Markets: []domain.MarketType{domain.MarketTypeMoneyline, domain.MarketTypeSpread},
	}

	const goroutines = 8
	errs := make(chan error, goroutines*3)
	done := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := h.Adapter.Fetch(context.Background(), scope); err != nil {
				errs <- err
			}
			if _, err := h.Adapter.Catalogue(context.Background()); err != nil {
				errs <- err
			}
			_ = h.Adapter.Quota()
			_ = h.Adapter.Cost(scope)
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	close(errs)
	for err := range errs {
		t.Errorf("concurrent call failed: %v", err)
	}

	// Every goroutine saw the same twelve books, and the registry did not
	// duplicate them under contention.
	if got := len(h.Adapter.mapper.books.books()); got != 12 {
		t.Errorf("book registry holds %d books after %d concurrent sweeps, want 12", got, goroutines)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
