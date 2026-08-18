package theoddsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// Adapter is The Odds API as internal/ingest/provider's Adapter interface sees
// it: the HTTP client of client.go, plus the neutral-shape decode of decode.go,
// plus the domain mapping of mapping.go.
//
// It is safe for concurrent use. The scheduler runs a goroutine per league
// window and they share one adapter; the HTTP client, the limiter, the book
// registry and the league registry all guard their own state.
//
// # What each interface method costs
//
//	Name       nothing
//	Catalogue  one FREE request (/v4/sports). ADR 0003 requirement 2: "Refresh
//	           the event and league catalogue aggressively — only price polling
//	           costs anything."
//	Fetch      markets × region-equivalents credits for a sweep; the same,
//	           TIMES the event count, when player props force the per-event
//	           endpoint.
//	Cost       nothing, and reads no clock. It is the number the scheduler's
//	           token bucket charges before it spends, so it must be knowable
//	           without asking anybody.
//	Quota      nothing. Asking a provider how much quota is left would itself
//	           spend quota.
type Adapter struct {
	client *Client
	name   provider.Name
	prov   kafka.Provider

	mapper  *mapper
	metrics *Metrics
	log     *slog.Logger
	tracer  trace.Tracer

	mu      sync.RWMutex
	leagues map[domain.LeagueID]string // derived league id -> provider sport key
}

// Span attributes specific to the adapter layer.
const (
	attrLeagueID    = "sharpline.league_id"
	attrScopeEvents = "sharpline.provider.scope_events"
	attrSnapEvents  = "sharpline.provider.snapshot_events"
	attrSnapPrices  = "sharpline.provider.snapshot_prices"
)

// NewAdapter builds the adapter and the HTTP client underneath it.
//
// The options are the Client's, because everything injectable — the HTTP
// client, the logger, the tracer provider, the metrics, the clock, the shared
// limiter — belongs to the transport. The adapter reads them back off the
// client rather than taking a second, divergent copy.
func NewAdapter(cfg Config, opts ...Option) (*Adapter, error) {
	c, err := New(cfg, opts...)
	if err != nil {
		return nil, err
	}
	return Wrap(c)
}

// Wrap turns an existing Client into an Adapter, so several league pollers can
// share one client (and therefore one budget) without constructing it twice.
func Wrap(c *Client) (*Adapter, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: nil client", ErrInvalidConfig)
	}
	p, err := kafka.NewProvider(ProviderSlug)
	if err != nil {
		return nil, fmt.Errorf("%w: provider slug %q: %w", ErrInvalidConfig, ProviderSlug, err)
	}
	// Fail at construction rather than at the first record: a provider slug
	// that does not fit the identifier budget would produce identifiers the
	// domain refuses, one per market, for ever.
	if err := normalizer.ValidateProviderForIdentity(p); err != nil {
		return nil, fmt.Errorf("%w: provider slug: %w", ErrInvalidConfig, err)
	}
	name, err := provider.NewName(ProviderSlug)
	if err != nil {
		return nil, fmt.Errorf("%w: provider name: %w", ErrInvalidConfig, err)
	}
	if name != provider.NameTheOddsAPI {
		// The two constants are one contract: provider.NameTheOddsAPI is the
		// {provider} in odds.raw.the-odds-api and the `provider` metric label,
		// and ProviderSlug is what this package writes into that label.
		return nil, fmt.Errorf("%w: provider slug %q does not match provider.NameTheOddsAPI %q",
			ErrInvalidConfig, ProviderSlug, provider.NameTheOddsAPI)
	}

	return &Adapter{
		client:  c,
		name:    name,
		prov:    p,
		metrics: c.metrics,
		log:     c.log,
		tracer:  c.tracer,
		leagues: make(map[domain.LeagueID]string),
		mapper: &mapper{
			prov:       p,
			oddsFormat: c.cfg.OddsFormat,
			reference:  c.cfg.ReferenceBook,
			metrics:    c.metrics,
			books:      newBookRegistry(),
		},
	}, nil
}

// Client returns the HTTP client, so a second Adapter can be built over the same
// budget with Wrap.
func (a *Adapter) Client() *Client { return a.client }

// Name implements provider.Adapter.
func (a *Adapter) Name() provider.Name { return a.name }

// Quota implements provider.Adapter. It performs NO I/O.
//
// # Where the number comes from, and when it is trusted
//
// ADR 0003 requirement 3: "Feed the Prometheus quota gauge from
// x-requests-remaining, the provider's own number, not from a local counter.
// […] using the response header makes it authoritative and drift-proof." So the
// provider's header wins whenever one has been seen.
//
// Known is the honest half of that. provider.Quota documents it as "False
// before the first successful request […] reporting a fabricated one would put
// a number on the dashboard that no provider ever said", and
// ProviderQuotaExhausted alerts on `== 0`, so a "not yet measured" zero would
// page for a healthy system during every cold start. Known is therefore true in
// exactly two cases:
//
//   - the provider has reported x-requests-remaining, or
//   - this process has actually spent credits, so the local estimate is a
//     measurement of our own spend rather than a restatement of the configured
//     budget.
//
// Which of the two is live is separately visible on
// sharpline_provider_quota_from_provider, so a gauge running on the estimate is
// distinguishable rather than indistinguishable.
func (a *Adapter) Quota() provider.Quota {
	q := a.client.Quota()
	known := q.FromProvider || (q.Limit > 0 && q.LocalEstimate < q.Limit)
	remaining := q.Remaining
	if !q.FromProvider {
		remaining = q.LocalEstimate
	}
	return provider.Quota{
		Known:      known,
		Remaining:  remaining,
		Limit:      q.Limit,
		LastCost:   q.LastCost,
		ObservedAt: q.ObservedAt,
	}
}

// Cost implements provider.Adapter: the credits one Fetch of scope consumes.
//
// ADR 0003 §Cost model, from the provider's own documentation:
//
//	/v4/sports/{sport}/odds                  markets × regions
//	/v4/sports/{sport}/events/{id}/odds      unique markets returned × regions, PER EVENT
//
// Two consequences the scheduler's token bucket depends on:
//
//   - Cost does NOT scale with slate size for a sweep. "A 16-game NFL slate and
//     a 1-game slate cost the same." Narrowing a scope to a handful of events
//     therefore does not make a sweep cheaper, and this adapter still issues one
//     sweep and filters, because the alternative costs more.
//   - It DOES scale with slate size once a player prop is in scope, because
//     props are only available per event. ADR 0003 scenario E: "One afternoon of
//     NFL player props costs 6,144 credits — 6.1% of the entire 100K monthly
//     tier, spent in four hours."
//
// Ten named bookmakers count as one region-equivalent and take precedence over
// `regions` (ADR 0003 requirement 1), which is why SweepCost is given the
// bookmaker count rather than a region count alone.
//
// It performs no I/O and reads no clock.
func (a *Adapter) Cost(scope provider.Scope) int {
	keys, perEvent := a.marketKeys(scope.Markets)
	markets := len(keys)
	if markets < 1 {
		// The provider defaults `markets` to h2h when it is omitted, and bills
		// for it. Charging zero here would let a malformed scope spend a credit
		// the bucket never reserved.
		markets = 1
	}
	cost := SweepCost(markets, len(a.client.cfg.Regions), len(a.client.cfg.Bookmakers))
	if !perEvent {
		return cost
	}
	events := len(scope.Events)
	if events < 1 {
		events = 1
	}
	return cost * events
}

// marketKeys maps the scope's domain market types onto provider market keys.
//
// The second return value reports whether the per-event endpoint is required,
// which is true exactly when a player prop is in scope: the provider's own
// INVALID_MARKET documentation states that the /odds endpoint "only support[s]
// featured markets" and that everything else must be requested one event at a
// time.
func (a *Adapter) marketKeys(types []domain.MarketType) (keys []string, perEvent bool) {
	seen := make(map[string]bool, len(types)+len(a.client.cfg.PlayerPropMarkets))
	add := func(k string) {
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		keys = append(keys, k)
	}
	for _, t := range types {
		if k, ok := featuredMarketKeyFor(t); ok {
			add(k)
			continue
		}
		if t == domain.MarketTypePlayerProp {
			perEvent = true
			for _, k := range a.client.cfg.PlayerPropMarkets {
				add(k)
			}
		}
	}
	return keys, perEvent
}

// -----------------------------------------------------------------------------
// Catalogue
// -----------------------------------------------------------------------------

// Catalogue implements provider.Adapter.
//
// It costs ZERO credits — guides/v4: "This endpoint does not count against the
// usage quota" — which is the whole reason provider.Adapter keeps it separate
// from Fetch. It still takes a slot in the frequency bucket, because free does
// not mean exempt from the documented 30 requests/second.
//
// # The books come from Fetch, not from here
//
// The Odds API publishes no bookmaker endpoint. A book's display title exists
// only inside an odds payload, so the catalogue's Books list is whatever the
// provider has actually named so far and is EMPTY before the first Fetch.
// Manufacturing "DraftKings" from the key "draftkings" would be inventing
// display data; an empty list with a correct empty state is the honest answer.
func (a *Adapter) Catalogue(ctx context.Context) (provider.Catalogue, error) {
	ctx, span := a.tracer.Start(ctx, "theoddsapi.Catalogue",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String(attrProvider, ProviderSlug)))
	defer span.End()

	sports, err := a.client.Sports(ctx, a.client.cfg.IncludeInactiveSports)
	if err != nil {
		wrapped := a.translate("catalogue", err)
		a.recordSpanError(span, wrapped)
		return provider.Catalogue{}, wrapped
	}

	cat := provider.Catalogue{Books: a.mapper.books.books()}
	seenSports := make(map[domain.SportID]bool, len(sports))
	seenLeagues := make(map[domain.LeagueID]bool, len(sports))
	learned := make(map[domain.LeagueID]string, len(sports))

	for _, s := range sports {
		leagueKey := strings.TrimSpace(s.Key)
		if leagueKey == "" {
			continue
		}
		// "americanfootball_nfl" is a LEAGUE. Its sport is the prefix, which is
		// the provider's own grouping rather than a guess.
		sportKey := normalizer.SportKeyFromLeagueKey(leagueKey)

		sportID, err := normalizer.SportIDFor(a.prov, sportKey)
		if err != nil {
			continue
		}
		if !seenSports[sportID] {
			sportSlug, err := normalizer.SlugFor("", sportKey)
			if err != nil {
				continue
			}
			sport, err := domain.NewSport(domain.SportParams{
				ID:   sportID,
				Slug: sportSlug,
				// `group` is the provider's own display text ("American
				// Football"); the key is the fallback, never a prettified guess.
				Name: firstNonEmpty(strings.TrimSpace(s.Group), sportKey),
			})
			if err != nil {
				continue
			}
			seenSports[sportID] = true
			cat.Sports = append(cat.Sports, sport)
		}

		leagueID, err := normalizer.LeagueIDFor(a.prov, leagueKey)
		if err != nil || seenLeagues[leagueID] {
			continue
		}
		leagueSlug, err := normalizer.SlugFor("", leagueKey)
		if err != nil {
			continue
		}
		league, err := domain.NewLeague(domain.LeagueParams{
			ID:      leagueID,
			SportID: sportID,
			Slug:    leagueSlug,
			Name:    firstNonEmpty(strings.TrimSpace(s.Title), leagueKey),
		})
		if err != nil {
			continue
		}
		seenLeagues[leagueID] = true
		cat.Leagues = append(cat.Leagues, league)
		learned[leagueID] = leagueKey
	}

	if err := cat.Validate(); err != nil {
		wrapped := provider.Newf("catalogue", a.name, provider.DispositionFatal,
			provider.ErrInvalidCatalogue, "%s", a.redact(err.Error()))
		a.recordSpanError(span, wrapped)
		return provider.Catalogue{}, wrapped
	}

	a.mu.Lock()
	for id, key := range learned {
		a.leagues[id] = key
	}
	a.mu.Unlock()

	span.SetAttributes(
		attribute.Int("sharpline.provider.catalogue_sports", len(cat.Sports)),
		attribute.Int("sharpline.provider.catalogue_leagues", len(cat.Leagues)),
		attribute.Int("sharpline.provider.catalogue_books", len(cat.Books)),
	)
	span.SetStatus(codes.Ok, "")
	return cat, nil
}

// -----------------------------------------------------------------------------
// Fetch
// -----------------------------------------------------------------------------

// Fetch implements provider.Adapter: the current state of every market in scope.
//
// It returns a FULL statement, not a delta. Change detection is the ingest
// service's job (CLAUDE.md §5, "hash each normalized market to suppress no-op
// updates") and it cannot be the adapter's, because the provider re-sends its
// whole board on every poll and has no idea what we saw last time.
//
// # Failure, not a stale answer
//
// Every error path here returns nothing rather than a partial or previous
// snapshot. ADR 0003 requirement 5: "The limiter must fail to synthetic, not
// fail to stale. When the budget is exhausted the correct behaviour is a loud
// alert and a visible degraded state — never a board that silently shows
// hour-old prices as if they were live." There is no cache in this adapter for
// the same reason.
func (a *Adapter) Fetch(ctx context.Context, scope provider.Scope) (provider.Snapshot, error) {
	ctx, span := a.tracer.Start(ctx, "theoddsapi.Fetch",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String(attrProvider, ProviderSlug),
			attribute.String(attrLeagueID, scope.League.String()),
			attribute.Int(attrScopeEvents, len(scope.Events)),
		))
	defer span.End()

	if err := scope.Validate(); err != nil {
		wrapped := provider.Newf("fetch", a.name, provider.DispositionFatal,
			provider.ErrInvalidScope, "%s", err)
		a.recordSpanError(span, wrapped)
		return provider.Snapshot{}, wrapped
	}

	sportKey, err := a.sportKeyFor(ctx, scope.League)
	if err != nil {
		a.recordSpanError(span, err)
		return provider.Snapshot{}, err
	}
	span.SetAttributes(attribute.String(attrSportKey, sportKey))

	keys, perEvent := a.marketKeys(scope.Markets)
	if len(keys) == 0 {
		wrapped := provider.Newf("fetch", a.name, provider.DispositionFatal,
			provider.ErrNotSupported,
			"scope %s names no market this provider serves (player props require %s to be configured)",
			scope, envPlayerPropMarkets)
		a.recordSpanError(span, wrapped)
		return provider.Snapshot{}, wrapped
	}

	var sweeps []*OddsSweep
	if perEvent {
		sweeps, err = a.fetchPerEvent(ctx, sportKey, scope, keys)
	} else {
		var sw *OddsSweep
		sw, err = a.client.OddsWithMarkets(ctx, sportKey, keys)
		if sw != nil {
			sweeps = []*OddsSweep{sw}
		}
	}
	if err != nil {
		wrapped := a.translate("fetch", err)
		a.recordSpanError(span, wrapped)
		return provider.Snapshot{}, wrapped
	}
	if len(sweeps) == 0 {
		// Every requested event 404'd. That is a legitimate empty answer, not a
		// failure: an expired event id is documented as 404 and errors.go maps
		// it to "skip this event, keep the sweep going".
		snap := provider.Snapshot{
			Provider:  a.name,
			Scope:     scope,
			FetchedAt: a.client.now(),
			Quota:     a.Quota(),
		}
		span.SetStatus(codes.Ok, "")
		return snap, nil
	}

	fetchedAt := time.Time{}
	events := make([]provider.EventSnapshot, 0, 16)
	for _, sw := range sweeps {
		if sw.FetchedAt.After(fetchedAt) {
			fetchedAt = sw.FetchedAt
		}
		events = append(events, a.mapSweep(sw, scope, sportKey)...)
	}
	if fetchedAt.IsZero() {
		fetchedAt = a.client.now()
	}

	snap := provider.Snapshot{
		Provider:  a.name,
		Scope:     scope,
		FetchedAt: fetchedAt,
		Quota:     a.Quota(),
		Events:    a.keepValid(scope, fetchedAt, events),
	}
	span.SetAttributes(
		attribute.Int(attrSnapEvents, len(snap.Events)),
		attribute.Int(attrSnapPrices, snap.PriceCount()),
	)
	span.SetStatus(codes.Ok, "")
	return snap, nil
}

// fetchPerEvent issues one per-event request for each event in scope.
//
// A 404 skips that event and continues; every other failure aborts, because a
// 401 or an exhausted budget will not improve on the next event and issuing the
// remaining calls would spend credits to learn the same thing several times.
func (a *Adapter) fetchPerEvent(
	ctx context.Context,
	sportKey string,
	scope provider.Scope,
	keys []string,
) ([]*OddsSweep, error) {
	if len(scope.Events) == 0 {
		return nil, fmt.Errorf("%w: player props require an event-narrowed scope — "+
			"the provider serves non-featured markets only from %s", ErrInvalidRequest, EndpointEventOdds)
	}
	out := make([]*OddsSweep, 0, len(scope.Events))
	for _, id := range scope.Events {
		key, ok := a.eventKeyFor(id)
		if !ok {
			a.metrics.observeDropped(DropReasonInvalidEvent, 1)
			continue
		}
		sw, err := a.client.EventOdds(ctx, sportKey, key, keys)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return out, err
		}
		out = append(out, sw)
	}
	return out, nil
}

// mapSweep converts one response into event snapshots.
func (a *Adapter) mapSweep(sw *OddsSweep, scope provider.Scope, sportKey string) []provider.EventSnapshot {
	bodies := splitPayload(sw.Raw, len(sw.Events))
	out := make([]provider.EventSnapshot, 0, len(sw.Events))

	for i, ev := range sw.Events {
		if !strings.EqualFold(strings.TrimSpace(ev.SportKey), sportKey) {
			// The provider answered for a different league than the one asked
			// for. Mapping it would put an event in the wrong league's snapshot,
			// which provider.Snapshot.Validate rejects — better to say so.
			a.metrics.observeDropped(DropReasonWrongLeague, 1)
			continue
		}
		raw, dropped, err := RawEventFrom(ev, sw.OddsFormat, a.mapper.reference)
		if err != nil {
			a.metrics.observeDropped(DropReasonInvalidEvent, 1)
			continue
		}
		a.metrics.observeDropped(DropReasonInvalidOdds, dropped)

		var body []byte
		if i < len(bodies) {
			body = bodies[i]
		}
		snap, ok := a.mapper.mapEvent(raw, scope.League, sw.FetchedAt, body)
		if !ok {
			continue
		}
		if !scope.HasEvent(snap.Event.ID()) {
			// A narrowed scope served from a full sweep: the sweep is the same
			// price as a narrowed request, so the filtering happens here rather
			// than at the provider.
			continue
		}
		out = append(out, snap)
	}
	return out
}

// keepValid drops any event snapshot the provider layer's own invariants
// refuse, rather than failing the whole sweep.
//
// provider.Snapshot.Validate is the contract every adapter's output must
// satisfy, and it catches three bugs that are otherwise silent — a selection
// whose role its market type does not admit, a price whose line has drifted
// from its selection's, an away spread quoted at the home line. Running it per
// event here means one malformed contest costs one contest, not the board, and
// the loss is counted rather than swallowed.
func (a *Adapter) keepValid(
	scope provider.Scope,
	fetchedAt time.Time,
	events []provider.EventSnapshot,
) []provider.EventSnapshot {
	out := events[:0]
	for _, e := range events {
		probe := provider.Snapshot{
			Provider:  a.name,
			Scope:     scope,
			FetchedAt: fetchedAt,
			Events:    []provider.EventSnapshot{e},
		}
		if err := probe.Validate(); err != nil {
			a.metrics.observeDropped(DropReasonInvalidEvent, 1)
			a.log.Debug("dropping event that failed snapshot validation",
				slog.String("provider", ProviderSlug),
				slog.String("event_id", e.Event.ID().String()),
				slog.String("error", a.redact(err.Error())))
			continue
		}
		out = append(out, e)
	}
	return out
}

// -----------------------------------------------------------------------------
// Identifier round trips
// -----------------------------------------------------------------------------

// sportKeyFor resolves a derived league identifier back to the provider's sport
// key.
//
// Three attempts, cheapest first. The registry is populated by Catalogue; a
// miss triggers ONE catalogue refresh, which is free; and the last resort is a
// round trip through the identifier scheme itself — strip the prefix, re-derive,
// and accept the key only if it reproduces the identifier exactly. That last
// check is what makes the reversal safe: normalizer.LeagueIDFor hashes a key it
// cannot embed, and a hash does not round-trip, so a false positive is not
// possible.
func (a *Adapter) sportKeyFor(ctx context.Context, id domain.LeagueID) (string, error) {
	a.mu.RLock()
	key, ok := a.leagues[id]
	a.mu.RUnlock()
	if ok {
		return key, nil
	}
	if k, ok := leagueKeyFromID(a.prov, id); ok {
		return k, nil
	}
	if _, err := a.Catalogue(ctx); err != nil {
		return "", err
	}
	a.mu.RLock()
	key, ok = a.leagues[id]
	a.mu.RUnlock()
	if ok {
		return key, nil
	}
	return "", provider.Newf("fetch", a.name, provider.DispositionFatal, provider.ErrNotFound,
		"league %s is not one this provider offers", id)
}

// leagueKeyFromID reverses normalizer.LeagueIDFor when the key was embedded
// verbatim.
func leagueKeyFromID(p kafka.Provider, id domain.LeagueID) (string, bool) {
	key, ok := trimIdentityPrefix(string(p), "l", string(id))
	if !ok {
		return "", false
	}
	got, err := normalizer.LeagueIDFor(p, key)
	if err != nil || got != id {
		return "", false
	}
	return key, true
}

// eventKeyFor reverses normalizer.EventIDFor. The Odds API's event identifiers
// are 32 hex characters, comfortably inside the 35-byte embedding budget, so
// the reversal succeeds for every real event id.
func (a *Adapter) eventKeyFor(id domain.EventID) (string, bool) {
	key, ok := trimIdentityPrefix(string(a.prov), "e", string(id))
	if !ok {
		return "", false
	}
	got, err := normalizer.EventIDFor(a.prov, key)
	if err != nil || got != id {
		return "", false
	}
	return key, true
}

// trimIdentityPrefix strips `{provider}.{tag}.` and rejects a component that is
// a HASH rather than an embedded key.
//
// The rejection is the whole reason this is a function and not a string slice.
// normalizer.identity replaces any component it cannot embed — too long, or
// carrying a byte outside [A-Za-z0-9_-] — with `h` plus 48 bits of SHA-256, and
// a hash is not reversible. But it IS embeddable, so re-deriving from the hash
// reproduces the identifier exactly and the round-trip check passes: the
// reversal would hand back "h1a2b3c4d5e6f" as though it were the provider's own
// sport key, and the adapter would spend a credit asking for a sport that does
// not exist.
//
// So a component of exactly the hash's shape is refused. The cost of being
// wrong in this direction is nil — a genuine provider key of that shape falls
// through to the catalogue, which resolves it correctly and for free — while
// the cost of being wrong in the other direction is a wasted request per poll.
//
// hashShape below is a copy of normalizer's private encoding.
// TestIdentityHashShapeMatchesTheNormalizer keeps the two in step.
func trimIdentityPrefix(prov, tag, id string) (string, bool) {
	prefix := prov + "." + tag + "."
	if !strings.HasPrefix(id, prefix) {
		return "", false
	}
	rest := id[len(prefix):]
	if rest == "" || looksHashed(rest) {
		return "", false
	}
	return rest, true
}

// identityHashHexLen is how many hex characters normalizer.identity keeps of a
// hashed component, after its `h` prefix.
const identityHashHexLen = 12

// looksHashed reports whether s has the shape normalizer.identity gives a
// component it could not embed.
func looksHashed(s string) bool {
	if len(s) != identityHashHexLen+1 || s[0] != 'h' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// splitPayload cuts a sweep's bytes into one payload per event, preserving each
// event's EXACT bytes.
//
// provider.RawPayload requires "the provider's bytes, unmodified", because
// odds.raw.{provider} is the replayable record a golden file is recorded from
// and the only artefact that survives a parsing bug. Re-marshalling the decoded
// struct would silently normalise key order, number formatting and every field
// this build does not model — which is precisely the drift a raw topic exists to
// detect.
//
// A payload that is a single object (the per-event endpoint) is one element. A
// mismatch between the element count and the decoded event count returns
// nothing rather than a misaligned mapping: an event carrying another event's
// bytes is worse than an event carrying none.
func splitPayload(raw json.RawMessage, want int) [][]byte {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	if trimmed[0] == '{' {
		if want != 1 {
			return nil
		}
		return [][]byte{[]byte(raw)}
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) != want {
		return nil
	}
	out := make([][]byte, len(parts))
	for i := range parts {
		out[i] = parts[i]
	}
	return out
}

// -----------------------------------------------------------------------------
// Error translation
// -----------------------------------------------------------------------------

// translate maps this package's error vocabulary onto the provider layer's.
//
// The two vocabularies exist because they answer different questions: errors.go
// says WHAT the provider did, provider/errors.go says what the SCHEDULER should
// do about it. The mapping is where those meet, and getting it wrong is
// expensive in both directions — retrying a bad key for ever, or abandoning a
// league over a five-second blip.
//
//	local credit budget empty  -> quota exhausted. ADR 0003 requirement 5 calls
//	                              this the state that must "fail to synthetic",
//	                              and it is not a few seconds of backoff: the
//	                              bucket refills over the budget window.
//	local frequency bucket      -> rate limited. Sub-second, and retryable.
//	                              Collapsing it into quota exhaustion would fire
//	                              ProviderQuotaExhausted for a burst of sweeps.
//	provider OUT_OF_USAGE_CREDITS -> quota exhausted.
//	401 bad/missing/deactivated key -> fatal, unauthorized.
//	404                          -> fatal for the scope; the caller skips it.
//	422                          -> fatal. This package built a request the
//	                              provider rejects, which is a config bug.
//	429                          -> retryable, carrying Retry-After.
//	5xx / transport              -> retryable.
//	undecodable 200              -> fatal, malformed payload. Loud, because a
//	                              silently dropped payload leaves the board
//	                              frozen with no failure visible anywhere.
//
// Every message is passed through the redactor a second time. The key travels
// in a query parameter, so an error that formats a URL leaks it, and belt and
// braces is the correct amount of care for a credential that is only
// recoverable by rotation.
func (a *Adapter) translate(op string, err error) error {
	if err == nil {
		return nil
	}

	e := &provider.Error{Op: op, Provider: a.name, Err: err}
	if after, ok := RetryAfter(err); ok {
		e.RetryAfter = after
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		e.Status = apiErr.StatusCode
	}

	var sentinel error
	switch {
	case errors.Is(err, context.Canceled):
		// A cancelled parent context is a decision, not a provider failure.
		return err
	case errors.Is(err, ErrBudgetExhausted):
		var budget *BudgetError
		if errors.As(err, &budget) && budget.Limiter == "frequency" {
			e.Disposition, sentinel = provider.DispositionRetryable, provider.ErrRateLimited
			break
		}
		e.Disposition, sentinel = provider.DispositionQuotaExhausted, provider.ErrQuotaExhausted
	case errors.Is(err, ErrQuotaExhausted):
		e.Disposition, sentinel = provider.DispositionQuotaExhausted, provider.ErrQuotaExhausted
	case errors.Is(err, ErrUnauthenticated):
		e.Disposition, sentinel = provider.DispositionFatal, provider.ErrUnauthorized
	case errors.Is(err, ErrNotFound):
		e.Disposition, sentinel = provider.DispositionFatal, provider.ErrNotFound
	case errors.Is(err, ErrInvalidRequest), errors.Is(err, ErrInvalidConfig):
		e.Disposition, sentinel = provider.DispositionFatal, provider.ErrProviderRejected
	case errors.Is(err, ErrMalformedResponse):
		e.Disposition, sentinel = provider.DispositionFatal, provider.ErrMalformedPayload
	case errors.Is(err, ErrRateLimited):
		e.Disposition, sentinel = provider.DispositionRetryable, provider.ErrRateLimited
	case errors.Is(err, ErrProviderFailure), errors.Is(err, ErrTransport),
		errors.Is(err, context.DeadlineExceeded):
		e.Disposition, sentinel = provider.DispositionRetryable, provider.ErrUnavailable
	default:
		// provider/errors.go's documented default, and its reasoning: a board
		// frozen at the last observation is the worst outcome this system has,
		// so an unclassified error keeps polling and stays visible in
		// sharpline_provider_requests_total{outcome="retryable"}.
		e.Disposition, sentinel = provider.DispositionRetryable, provider.ErrUnavailable
	}

	// BOTH chains are preserved: the provider-layer sentinel the scheduler
	// matches on, and this package's own error underneath it. Flattening the
	// cause into a formatted string would make errors.Is(err, ErrRateLimited)
	// and errors.As(err, &*url.Error) fail on the returned value, which breaks
	// timeout and DNS detection upstream — CLAUDE.md §12 requires errors wrap
	// with %w for exactly this reason.
	//
	// The redaction pass then replaces the MESSAGE without touching the chain.
	// Everything reaching here has already been sanitised by the client; this
	// is the second pass doc.go promises, for the code path nobody has written
	// yet.
	cause := fmt.Errorf("%w: %w", sentinel, err)
	if msg := a.redact(cause.Error()); msg != cause.Error() {
		cause = &redactedError{msg: msg, err: cause}
	}
	e.Err = cause
	return e
}

// redact is the adapter's last pass over any string that might have touched a
// URL. The client's redactor already sanitised every error it returns; this is
// the second pass doc.go promises.
func (a *Adapter) redact(s string) string { return a.client.redact.String(s) }

func (a *Adapter) recordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, a.redact(err.Error()))
}

// Compile-time proof that the adapter is what CLAUDE.md §5 calls a
// ProviderAdapter. Both names are asserted because they are one type and a
// future edit that split them would otherwise go unnoticed here.
var (
	_ provider.Adapter         = (*Adapter)(nil)
	_ provider.ProviderAdapter = (*Adapter)(nil)
)
