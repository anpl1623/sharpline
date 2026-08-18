package synthetic

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// Catalogue is provider.Catalogue under a local name, so that this package's
// signatures read in its own vocabulary. It is an ALIAS, not a second type:
// there is one catalogue shape in the system and an adapter that invented its
// own would not satisfy provider.Adapter.
type Catalogue = provider.Catalogue

// neutralProviderSlug is this adapter's name as the bus and the normalizer spell
// it. provider.Name and kafka.Provider enforce the identical charset for the
// identical reason (see provider.Name's comment), so the conversion is safe by
// construction and TestNameMatchesKafkaProviderCharset keeps it that way.
const neutralProviderSlug = kafka.Provider(provider.NameSynthetic)

// Adapter is the seeded stochastic market maker.
//
// # It holds no model state
//
// Everything about the simulated universe at an instant is a pure function of
// (Options.Seed, that instant). The struct carries configuration, two
// precomputed kernels, the book table, and a credit bucket — and nothing that
// evolves. That is what makes the determinism contract true rather than
// aspirational: there is no state for a restart to lose, no accumulator for two
// goroutines to race on, and no way for the polling schedule to influence the
// prices, because nothing here remembers that a poll happened.
//
// The one mutable field is the quota bucket, which is a limiter rather than part
// of the model, and it is guarded by its own mutex. Every method is therefore
// safe for the concurrent use provider.Adapter requires.
type Adapter struct {
	opts  Options
	books []bookDef

	// fastW and slowW are the two noise kernels, computed once. They are read
	// concurrently and never written after New returns.
	fastW      []float64
	slowW      []float64
	weightFast float64

	// lags[i] is book i's view lag in steps, and maxLag is the deepest. They are
	// derived from books() so the two orders cannot drift apart.
	lags   []int
	maxLag int

	mu       sync.Mutex
	tokens   float64
	refilled time.Time
	lastCost int64
}

// Compile-time proof that this package satisfies the seam. CLAUDE.md §5's
// ProviderAdapter and provider.Adapter are the same type, so one assertion
// covers both.
var _ provider.Adapter = (*Adapter)(nil)

// New builds the synthetic adapter.
//
// A zero Options is valid and yields the documented defaults, so the offline
// path is `synthetic.New(synthetic.Options{})`. Pass Seed to reproduce a
// specific board and Clock to make a test's instants explicit.
func New(opts Options) (*Adapter, error) {
	opts = opts.withDefaults()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if err := assertRawDecodes(); err != nil {
		return nil, err
	}

	bs := books()
	a := &Adapter{
		opts:       opts,
		books:      bs,
		fastW:      expKernel(fastHalfLifeGrid, fastKernelLen),
		slowW:      expKernel(slowHalfLifeGrid, slowKernelLen),
		weightFast: fastMixWeight(),
		lags:       make([]int, len(bs)),
		maxLag:     maxBookLag(),
		tokens:     float64(opts.QuotaBudget),
		refilled:   opts.Clock().UTC(),
	}
	for i, b := range bs {
		a.lags[i] = b.lagSteps
	}

	// The catalogue is built once here and thrown away. It is the cheapest
	// possible proof that the invented universe satisfies the domain's
	// constructors and provider.Catalogue.Validate — a league naming an absent
	// sport, or two books claiming to be the sharp reference, becomes a startup
	// error instead of a failure on the first poll.
	c, err := a.catalogue()
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("synthetic: %w", err)
	}
	return a, nil
}

// fastMixWeight returns the fast component's weight, which is fixed by
// weightSlow: the two must satisfy w_slow² + w_fast² = 1 for the mixture to have
// the unit variance every downstream scale factor assumes.
func fastMixWeight() float64 {
	return math.Sqrt(1 - weightSlow*weightSlow)
}

// Name implements provider.Adapter.
func (a *Adapter) Name() provider.Name { return provider.NameSynthetic }

// Catalogue implements provider.Adapter. It is pure computation over the
// invented universe, so it consumes no credits — which happens to match the real
// provider, where ADR 0003 notes "/events and /sports are free".
func (a *Adapter) Catalogue(ctx context.Context) (Catalogue, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Catalogue{}, a.contextError("catalogue", err)
	}
	c, err := a.catalogue()
	if err != nil {
		return Catalogue{}, err
	}
	if err := c.Validate(); err != nil {
		return Catalogue{}, fmt.Errorf("synthetic: %w", err)
	}
	return c, nil
}

// Cost implements provider.Adapter.
//
// It mirrors The Odds API's markets × regions billing at one region, which is
// what makes the scheduler's budget arithmetic identical on both paths — an
// offline run exercises the same token bucket, the same backoff and the same
// alert as a run with a key.
//
// The credits are simulated. Generating a price costs nothing and no money
// moves; see Options.QuotaBudget for why the budget exists at all. Returning
// zero instead would be honest about the money and would silently disable every
// consumer of Cost, so the adapter that never spends anything would be the only
// one whose quota path is never tested.
//
// No I/O and no clock, as the interface requires.
func (a *Adapter) Cost(scope provider.Scope) int {
	if len(scope.Markets) == 0 {
		return 1
	}
	return len(scope.Markets)
}

// Quota implements provider.Adapter.
//
// The reading is always Known: unlike a real adapter, which has no number until
// the provider's first response header, this limiter is the adapter's own and is
// exact from construction. Reporting Known false would leave
// sharpline_provider_quota_remaining absent on the offline path, which is the
// path CI runs, so the panel and the ProviderQuotaLow alert would never be seen
// to work.
func (a *Adapter) Quota() provider.Quota {
	now := a.opts.Clock().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refillLocked(now)
	return a.quotaLocked(now)
}

// -----------------------------------------------------------------------------
// Fetch
// -----------------------------------------------------------------------------

// Fetch implements provider.Adapter.
//
// It returns a FULL statement of the scope's current state, never a delta:
// suppressing unchanged markets is the ingest service's job, and an adapter that
// did it would make the change-detection rate unmeasurable and a cold consumer
// unservable.
func (a *Adapter) Fetch(ctx context.Context, scope provider.Scope) (provider.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, a.opts.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return provider.Snapshot{}, a.contextError("fetch", err)
	}
	if err := scope.Validate(); err != nil {
		// scope.Validate already wraps provider.ErrInvalidScope, so the
		// classification and the errors.Is chain both survive the wrap.
		return provider.Snapshot{}, &provider.Error{
			Op: "fetch", Provider: provider.NameSynthetic,
			Disposition: provider.DispositionFatal, Err: err,
		}
	}
	league, ok := findLeague(scope.League)
	if !ok {
		return provider.Snapshot{}, provider.Newf("fetch", provider.NameSynthetic,
			provider.DispositionFatal, provider.ErrNotFound, "league %s", scope.League)
	}

	now := a.opts.Clock().UTC()
	quota, err := a.charge(now, int64(a.Cost(scope)))
	if err != nil {
		return provider.Snapshot{}, err
	}

	n := a.stepIndex(now)
	at := a.stepTime(n)
	snap := provider.Snapshot{
		Provider:  provider.NameSynthetic,
		Scope:     scope,
		FetchedAt: now,
		Quota:     quota,
	}

	sc := &scratch{}
	for _, ev := range a.buildSlate(league, now) {
		if err := ctx.Err(); err != nil {
			return provider.Snapshot{}, a.contextError("fetch", err)
		}
		if !scope.HasEvent(ev.id) {
			continue
		}
		es, err := a.newEventState(sc, ev, now, at, n)
		if err != nil {
			return provider.Snapshot{}, a.internal("fetch", err)
		}
		evSnap, ok, err := a.eventSnapshot(sc, es, scope)
		if err != nil {
			return provider.Snapshot{}, a.internal("fetch", err)
		}
		if ok {
			snap.Events = append(snap.Events, evSnap)
		}
	}

	// The snapshot is validated before it leaves. provider.Snapshot.Validate is
	// O(prices) and catches exactly the class of bug a generator produces: a
	// selection whose role its market type does not admit, or a price quoted at
	// a line its selection does not trade at. Both are plausible wrong numbers
	// rather than errors, and both would settle a wager at a handicap nobody
	// took.
	if err := snap.Validate(); err != nil {
		return provider.Snapshot{}, a.internal("fetch", err)
	}
	return snap, nil
}

// eventSnapshot builds one event's markets, prices and raw payload. The bool
// reports whether the event carries any market in scope; an event with none is
// dropped rather than published empty, because a scope that asked only for
// futures should not be told about every contest in the league.
func (a *Adapter) eventSnapshot(sc *scratch, es *eventState, scope provider.Scope) (provider.EventSnapshot, bool, error) {
	plans, err := a.planFor(es, scope)
	if err != nil {
		return provider.EventSnapshot{}, false, err
	}
	if len(plans) == 0 {
		return provider.EventSnapshot{}, false, nil
	}

	event, err := a.domainEvent(es)
	if err != nil {
		return provider.EventSnapshot{}, false, err
	}

	out := provider.EventSnapshot{Event: event, Markets: make([]provider.MarketSnapshot, 0, len(plans))}
	raws := make([][]normalizer.RawMarket, 0, len(plans))
	for _, p := range plans {
		ms, raw, err := a.assemble(es, p, sc)
		if err != nil {
			return provider.EventSnapshot{}, false, err
		}
		out.Markets = append(out.Markets, ms)
		raws = append(raws, raw)
	}

	payload, err := marshalRaw(a.rawEventFor(es, raws), es.at)
	if err != nil {
		return provider.EventSnapshot{}, false, err
	}
	out.Raw = payload
	return out, true, nil
}

// domainEvent builds the immutable Event value, with its score and clock.
//
// UpdatedAt is the MODEL instant rather than the fetch instant, for the same
// reason provider.go gives about prices: it is the provider's own observation
// time, and stamping our receipt time here would report a system with zero
// provider staleness for ever.
func (a *Adapter) domainEvent(es *eventState) (domain.Event, error) {
	params := domain.EventParams{
		ID:             es.ev.id,
		LeagueID:       es.ev.league.leagueID(),
		Kind:           es.ev.kind,
		Name:           es.ev.name,
		ScheduledStart: es.ev.start,
		Status:         es.status,
		UpdatedAt:      es.at,
	}
	if es.ev.kind == domain.EventKindMatch {
		params.Home = es.ev.home
		params.Away = es.ev.away
	}
	event, err := domain.NewEvent(params)
	if err != nil {
		return domain.Event{}, fmt.Errorf("synthetic event: %w", err)
	}
	if es.hasScore {
		event, err = event.WithScore(es.score, es.at)
		if err != nil {
			return domain.Event{}, fmt.Errorf("synthetic event %s score: %w", es.ev.id, err)
		}
	}
	if es.hasClock {
		event, err = event.WithClock(es.clock, es.at)
		if err != nil {
			return domain.Event{}, fmt.Errorf("synthetic event %s clock: %w", es.ev.id, err)
		}
	}
	return event, nil
}

// -----------------------------------------------------------------------------
// Model time
// -----------------------------------------------------------------------------

// stepIndex is the model step containing an instant.
//
// It is measured from the Unix epoch rather than from the adapter's construction
// instant, which is the whole determinism contract in one line: the step index
// for a given instant is the same in every process and after every restart, so
// the state at that instant is too.
func (a *Adapter) stepIndex(t time.Time) int64 {
	return floorDiv(t.UTC().UnixNano(), int64(a.opts.Step))
}

// stepTime is the instant a step begins, which is the observation time every
// value in that step carries.
func (a *Adapter) stepTime(n int64) time.Time {
	return time.Unix(0, n*int64(a.opts.Step)).UTC()
}

// -----------------------------------------------------------------------------
// The credit bucket
// -----------------------------------------------------------------------------

// charge takes cost credits, or reports exhaustion.
//
// ADR 0003's rule is followed exactly: when the budget is gone the adapter fails
// LOUDLY rather than serving a stale or partial answer. There is no "well, it is
// only synthetic" exception here, because the point of the bucket is that the
// exhaustion path behaves identically on both adapters.
func (a *Adapter) charge(now time.Time, cost int64) (provider.Quota, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refillLocked(now)
	if a.tokens < float64(cost) {
		return provider.Quota{}, provider.QuotaExhausted("fetch", provider.NameSynthetic, a.quotaLocked(now))
	}
	a.tokens -= float64(cost)
	a.lastCost = cost
	return a.quotaLocked(now), nil
}

// refillLocked adds the credits that have accrued since the last reading.
//
// A continuous refill rather than a period boundary, because a boundary makes
// the budget arrive in one lump and the gauge a sawtooth that no alert threshold
// reads sensibly. Time running backwards — a clock injected by a test, or an NTP
// step — adds nothing rather than removing credits.
func (a *Adapter) refillLocked(now time.Time) {
	if a.refilled.IsZero() {
		a.refilled = now
		return
	}
	elapsed := now.Sub(a.refilled)
	if elapsed <= 0 {
		return
	}
	a.refilled = now
	rate := float64(a.opts.QuotaBudget) / a.opts.QuotaPeriod.Seconds()
	a.tokens += rate * elapsed.Seconds()
	if a.tokens > float64(a.opts.QuotaBudget) {
		a.tokens = float64(a.opts.QuotaBudget)
	}
}

func (a *Adapter) quotaLocked(now time.Time) provider.Quota {
	remaining := int64(a.tokens)
	if remaining < 0 {
		remaining = 0
	}
	return provider.Quota{
		Known:      true,
		Remaining:  remaining,
		Limit:      a.opts.QuotaBudget,
		LastCost:   a.lastCost,
		ObservedAt: now,
	}
}

// -----------------------------------------------------------------------------
// Errors
// -----------------------------------------------------------------------------

// contextError classifies a cancelled or expired context. provider.Classify
// already maps context.Canceled to fatal and DeadlineExceeded to retryable; this
// wraps them so the scheduler also learns which adapter and which operation.
func (a *Adapter) contextError(op string, err error) error {
	d := provider.DispositionRetryable
	if provider.IsFatal(err) {
		d = provider.DispositionFatal
	}
	return &provider.Error{Op: op, Provider: provider.NameSynthetic, Disposition: d, Err: err}
}

// internal reports a failure inside the generator.
//
// It is FATAL, and deliberately so. Every error reachable from here is a
// violation the domain refused — an impossible price, a role its market does not
// admit — and no amount of retrying makes a generator's bug intermittent. The
// default classification is retryable (provider/errors.go explains why), which
// would turn this into an infinite loop over the same broken market.
func (a *Adapter) internal(op string, err error) error {
	return &provider.Error{
		Op:          op,
		Provider:    provider.NameSynthetic,
		Disposition: provider.DispositionFatal,
		Err:         fmt.Errorf("%w: %w", provider.ErrInvalidSnapshot, err),
	}
}
