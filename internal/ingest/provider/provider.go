// Package provider is the seam between the ingest service and an odds source.
//
// CLAUDE.md §5: "Each provider gets an adapter behind one interface." This file
// is that interface, the value types it exchanges, and the Prometheus
// instrumentation that wraps any implementation of it. errors.go is its error
// vocabulary.
//
// # Two implementations, one interface, chosen at startup
//
// The contract ledger fixes the selection rule: `ingest` constructs the real The
// Odds API adapter when ODDS_API_KEY is set and the synthetic stochastic market
// maker when it is not. Both satisfy Adapter, so nothing downstream of this
// package — the scheduler, the normalizer, the change detector, the bus
// producer — knows which one is running. That is the whole point of the seam,
// and it is what lets the repository be cloned and run with no API key at all.
//
// The synthetic adapter is not mock data. It is a live stochastic market maker
// computing prices from a seeded model on every call; see
// internal/ingest/provider/synthetic.
//
// # Why the interface lives here rather than beside the consumer
//
// CLAUDE.md §12 says interfaces are declared by the consumer. The consumer here
// is the ingest scheduler, and this interface is written from its point of view
// — it asks for exactly the five things the scheduler needs and nothing an
// adapter might find convenient to expose. It is declared in its own package
// only because there are two adapters plus the scheduler plus the normalizer,
// and a shared vocabulary of Scope / Snapshot / Quota in the consumer's package
// would make every adapter import the scheduler. The methods are still the
// consumer's list, not the producer's.
//
// # The observation instant is load-bearing
//
// Every domain.Price an adapter returns carries the PROVIDER'S OWN observation
// instant in Price.ObservedAt — The Odds API's `last_update` for the market, the
// generator's model instant for the synthetic. It is never the moment the
// adapter received the payload; that is Snapshot.FetchedAt, a different clock
// answering a different question.
//
// This is not bookkeeping. deploy/observability/rules/sharpline-alerts.yml
// defines the headline SLO as (fanout instant − observed_at) and
// migrations/00003 calls observed_at "the subtrahend in every staleness
// measurement". An adapter that stamps its own receipt time into ObservedAt does
// not produce a slightly-wrong metric; it produces a metric that reports zero
// provider staleness for ever, which is the exact failure the two-SLO split
// exists to prevent.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/domain"
)

// -----------------------------------------------------------------------------
// Provider identity
// -----------------------------------------------------------------------------

// MaxNameLen bounds a provider name. It is far below Kafka's 249-byte topic
// name limit, which the name has to fit inside once odds.raw. is prepended.
const MaxNameLen = 64

// Name identifies one odds source.
//
// It is the {provider} in the odds.raw.{provider} topic name and the value of
// the `provider` Prometheus label that
// deploy/observability/grafana/dashboards/sharpline-overview.json templates on,
// so it is constrained rather than free text.
//
// The charset is deliberately NARROWER than domain.Slug and identical to
// internal/platform/kafka's Provider: lowercase alphanumerics with internal
// hyphens, no underscores and no dots. kafka/topics.go explains why — Terraform's
// kafka-topics module rejects the wider set, and mixing '.' and '_' in one Kafka
// topic name collides in Kafka's own JMX metric names. A name this package
// accepted and that package rejected would produce a service publishing to a
// topic that cannot be created, with auto-creation disabled. The two validations
// are one contract; TestNameMatchesKafkaProviderCharset in the synthetic package
// asserts they still agree.
type Name string

// The two provider names in use. Terraform declares an odds.raw.* topic for each
// in every environment, so setting ODDS_API_KEY does not require a terraform
// apply first.
const (
	// NameSynthetic is the seeded stochastic market maker: the no-API-key path.
	NameSynthetic Name = "synthetic"

	// NameTheOddsAPI is the real adapter chosen in ADR 0003.
	NameTheOddsAPI Name = "the-odds-api"
)

// NewName validates a provider name.
func NewName(s string) (Name, error) {
	if s == "" {
		return "", fmt.Errorf("provider name: %w: empty", ErrInvalidName)
	}
	if len(s) > MaxNameLen {
		return "", fmt.Errorf("provider name %q is %d bytes, limit %d: %w",
			s, len(s), MaxNameLen, ErrInvalidName)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		switch {
		case alnum:
		case c == '-' && i > 0 && i < len(s)-1:
			// Internal hyphens only: "odds.raw.-x" and "odds.raw.x-" are both
			// rejected by Terraform's raw_providers validation.
		default:
			return "", fmt.Errorf("provider name %q: %w: want lowercase alphanumeric "+
				"with internal hyphens", s, ErrInvalidName)
		}
	}
	return Name(s), nil
}

// String returns the name as a bare string.
func (n Name) String() string { return string(n) }

// IsZero reports whether the name is unset.
func (n Name) IsZero() bool { return n == "" }

// -----------------------------------------------------------------------------
// Scope: what one fetch asks for
// -----------------------------------------------------------------------------

// Scope names the markets one Fetch covers.
//
// The unit is a league plus a market set, because that is the unit the real
// provider bills. ADR 0003: "One /odds request returns every upcoming and live
// event for that sport. Cost does not scale with the number of events" and "cost
// is multiplicative in markets × regions". A scope narrower than a league would
// cost the same and return less; a scope wider than a league is not a request
// the provider offers.
type Scope struct {
	// League is the league to fetch. Required.
	//
	// For The Odds API this is the sport key ("americanfootball_nfl"), which is
	// a legal domain.LeagueID — internal/domain/ids.go sized MaxIDLen against
	// exactly these values.
	League domain.LeagueID

	// Markets are the market types to price. Required, non-empty, no duplicates.
	// Cost is multiplicative in the count, so the scheduler chooses this
	// deliberately rather than always asking for everything.
	Markets []domain.MarketType

	// Events optionally narrows the fetch to specific events within the league.
	// Empty — the normal case — means every event the provider currently offers
	// for that league.
	//
	// It exists because The Odds API's per-event endpoint is the only way to
	// reach player props, and because a scheduler with a live-events-only window
	// wants to spend credits on those events alone. An adapter that cannot
	// narrow may ignore it, but must then still return only the named events.
	Events []domain.EventID
}

// Validate reports whether the scope is a request an adapter can serve.
func (s Scope) Validate() error {
	if err := validateLeagueID(s.League); err != nil {
		return err
	}
	if len(s.Markets) == 0 {
		return fmt.Errorf("scope for league %s: %w: no market types requested", s.League, ErrInvalidScope)
	}
	seen := make(map[domain.MarketType]bool, len(s.Markets))
	for _, m := range s.Markets {
		if !m.Valid() {
			return fmt.Errorf("scope for league %s: %w: market type %d is not defined",
				s.League, ErrInvalidScope, uint8(m))
		}
		if seen[m] {
			return fmt.Errorf("scope for league %s: %w: market type %s requested twice",
				s.League, ErrInvalidScope, m)
		}
		seen[m] = true
	}
	for _, id := range s.Events {
		if id.IsZero() {
			return fmt.Errorf("scope for league %s: %w: empty event id", s.League, ErrInvalidScope)
		}
	}
	return nil
}

// HasMarket reports whether the scope asks for the given market type.
func (s Scope) HasMarket(t domain.MarketType) bool {
	for _, m := range s.Markets {
		if m == t {
			return true
		}
	}
	return false
}

// HasEvent reports whether the scope covers the given event. An empty Events
// list covers every event in the league.
func (s Scope) HasEvent(id domain.EventID) bool {
	if len(s.Events) == 0 {
		return true
	}
	for _, e := range s.Events {
		if e == id {
			return true
		}
	}
	return false
}

// String implements fmt.Stringer.
func (s Scope) String() string {
	names := make([]string, 0, len(s.Markets))
	for _, m := range s.Markets {
		names = append(names, m.String())
	}
	if len(s.Events) == 0 {
		return fmt.Sprintf("scope(%s [%s])", s.League, strings.Join(names, ","))
	}
	return fmt.Sprintf("scope(%s [%s] %d event(s))", s.League, strings.Join(names, ","), len(s.Events))
}

// validateLeagueID rejects a league identifier the domain would refuse.
func validateLeagueID(id domain.LeagueID) error {
	if id.IsZero() {
		return fmt.Errorf("%w: empty league id", ErrInvalidScope)
	}
	if _, err := domain.NewLeagueID(string(id)); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidScope, err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Quota
// -----------------------------------------------------------------------------

// Quota is the request budget an adapter has left.
//
// CLAUDE.md §5 requires the remaining budget to be "a Prometheus gauge", and ADR
// 0003 is specific about where the number comes from: "Feed the Prometheus quota
// gauge from x-requests-remaining, the provider's own number, not from a local
// counter. §5 requires the gauge; using the response header makes it
// authoritative and drift-proof."
//
// So this type carries a READING, not an estimate, and Known says whether one
// exists yet. A real adapter that has not issued a request has no reading, and
// reporting a fabricated one would put a number on the dashboard that no
// provider ever said.
type Quota struct {
	// Known reports whether Remaining and Limit hold a real reading. False
	// before the first successful request. The metrics layer emits no gauge
	// while it is false rather than emitting a zero, because
	// ProviderQuotaExhausted alerts on `== 0` and a "not yet measured" zero
	// would page for a healthy system.
	Known bool

	// Remaining is the credits left in the budget.
	Remaining int64

	// Limit is the budget's size. It is the denominator of
	// ProviderQuotaLow's ratio, so it must be the same unit as Remaining.
	Limit int64

	// LastCost is what the most recent request cost, in credits — The Odds API's
	// x-requests-last. Zero when unknown.
	LastCost int64

	// ObservedAt is when the reading was taken.
	ObservedAt time.Time
}

// Fraction returns Remaining/Limit and whether it could be computed.
func (q Quota) Fraction() (float64, bool) {
	if !q.Known || q.Limit <= 0 {
		return 0, false
	}
	return float64(q.Remaining) / float64(q.Limit), true
}

// Exhausted reports whether a known budget has run out.
func (q Quota) Exhausted() bool { return q.Known && q.Remaining <= 0 }

// String implements fmt.Stringer.
func (q Quota) String() string {
	if !q.Known {
		return "quota(unknown)"
	}
	return fmt.Sprintf("quota(%d/%d, last cost %d)", q.Remaining, q.Limit, q.LastCost)
}

// -----------------------------------------------------------------------------
// Catalogue
// -----------------------------------------------------------------------------

// Catalogue is what an adapter can serve: the sports and leagues it offers and
// the books it quotes.
//
// It is separate from Fetch because for the real provider it is free — ADR 0003:
// "/events and /sports are free. The event and league catalogue can be refreshed
// as often as we like at zero cost. Credits are spent only on prices." Folding
// it into Fetch would make the catalogue cost credits it does not have to.
type Catalogue struct {
	Sports  []domain.Sport
	Leagues []domain.League
	Books   []domain.Book
}

// Validate checks the catalogue's internal consistency: every league names a
// sport that is present, and at most one book is the sharp reference.
//
// domain/book.go leaves the "exactly one reference book" rule to "where books
// are stored" rather than to any single Book, and this is the first place a
// whole book set exists, so it is checked here.
func (c Catalogue) Validate() error {
	sports := make(map[domain.SportID]bool, len(c.Sports))
	for _, s := range c.Sports {
		if s.IsZero() {
			return fmt.Errorf("catalogue: %w: zero sport", ErrInvalidCatalogue)
		}
		if sports[s.ID()] {
			return fmt.Errorf("catalogue: %w: duplicate sport %s", ErrInvalidCatalogue, s.ID())
		}
		sports[s.ID()] = true
	}
	leagues := make(map[domain.LeagueID]bool, len(c.Leagues))
	for _, l := range c.Leagues {
		if l.IsZero() {
			return fmt.Errorf("catalogue: %w: zero league", ErrInvalidCatalogue)
		}
		if leagues[l.ID()] {
			return fmt.Errorf("catalogue: %w: duplicate league %s", ErrInvalidCatalogue, l.ID())
		}
		leagues[l.ID()] = true
		if !sports[l.SportID()] {
			return fmt.Errorf("catalogue: %w: league %s names absent sport %s",
				ErrInvalidCatalogue, l.ID(), l.SportID())
		}
	}
	books := make(map[domain.BookID]bool, len(c.Books))
	references := 0
	for _, b := range c.Books {
		if b.IsZero() {
			return fmt.Errorf("catalogue: %w: zero book", ErrInvalidCatalogue)
		}
		if books[b.ID()] {
			return fmt.Errorf("catalogue: %w: duplicate book %s", ErrInvalidCatalogue, b.ID())
		}
		books[b.ID()] = true
		if b.IsReference() {
			references++
		}
	}
	if references > 1 {
		return fmt.Errorf("catalogue: %w: %d books claim to be the sharp reference, at most one may",
			ErrInvalidCatalogue, references)
	}
	return nil
}

// League returns the league with the given identifier.
func (c Catalogue) League(id domain.LeagueID) (domain.League, bool) {
	for _, l := range c.Leagues {
		if l.ID() == id {
			return l, true
		}
	}
	return domain.League{}, false
}

// Book returns the book with the given identifier.
func (c Catalogue) Book(id domain.BookID) (domain.Book, bool) {
	for _, b := range c.Books {
		if b.ID() == id {
			return b, true
		}
	}
	return domain.Book{}, false
}

// ReferenceBook returns the sharp reference book CLAUDE.md §6's positive-EV
// finder prices against, if the catalogue names one.
func (c Catalogue) ReferenceBook() (domain.Book, bool) {
	for _, b := range c.Books {
		if b.IsReference() {
			return b, true
		}
	}
	return domain.Book{}, false
}

// -----------------------------------------------------------------------------
// Snapshot: what one fetch returns
// -----------------------------------------------------------------------------

// RawPayload is the provider's own bytes for one event, kept verbatim.
//
// CLAUDE.md §3 routes provider → ingest → odds.raw.{provider} → normalizer, and
// this is what goes on that topic. kafka.OddsRaw keys those records by EventID
// because "The Odds API returns one payload per EVENT carrying every market on
// it", which is why a raw payload is scoped to an event here and not to a fetch.
//
// Keeping the bytes alongside the parsed form is deliberate. The raw topic is
// the replayable record of what the provider actually said: it is what a golden
// file is recorded from, what a normalizer regression is reproduced against, and
// the only artefact that survives a parsing bug. The parsed form beside it means
// ingest does not have to decode a payload the adapter has already decoded.
type RawPayload struct {
	// ContentType is the payload's media type, e.g. "application/json".
	ContentType string

	// Body is the provider's bytes, unmodified. It MUST NOT contain a
	// credential: The Odds API passes its key as a query parameter, so a
	// request URL is not part of a payload and must never be stored here.
	Body []byte

	// ObservedAt is the provider's own instant for this payload, matching the
	// prices inside it.
	ObservedAt time.Time
}

// IsZero reports whether no raw payload was captured.
func (r RawPayload) IsZero() bool { return len(r.Body) == 0 }

// MarketSnapshot is one market, its selections, and every book's current price
// for each of them.
type MarketSnapshot struct {
	Market     domain.Market
	Selections []domain.Selection

	// Prices holds at most one price per (selection, book) pair. It is empty
	// for a market that is not currently priced — a closed market on a finished
	// event still belongs in the snapshot so the normalizer can close it.
	Prices []domain.Price
}

// EventSnapshot is everything one fetch observed about one event.
type EventSnapshot struct {
	Event   domain.Event
	Markets []MarketSnapshot
	Raw     RawPayload
}

// Snapshot is one observation of a Scope.
//
// It is a full statement of the current state, not a delta. Change detection is
// the ingest service's job (CLAUDE.md §5: "Hash each normalized market to
// suppress no-op updates"), and it cannot be the adapter's: the real provider
// re-sends its whole board on every poll and has no idea what we saw last time.
type Snapshot struct {
	// Provider is the adapter that produced this snapshot.
	Provider Name

	// Scope is what was asked for.
	Scope Scope

	// FetchedAt is when the adapter received the payload. It becomes
	// prices.ingested_at, and (FetchedAt − price.ObservedAt) is the
	// provider-attributable share of staleness that
	// sharpline_odds_staleness_seconds{stage="received"} measures.
	//
	// It is NEVER written into a price's ObservedAt. See the package comment.
	FetchedAt time.Time

	// Quota is the budget reading after this fetch.
	Quota Quota

	Events []EventSnapshot
}

// PriceCount returns how many prices the snapshot carries.
func (s Snapshot) PriceCount() int {
	n := 0
	for _, e := range s.Events {
		for _, m := range e.Markets {
			n += len(m.Prices)
		}
	}
	return n
}

// MarketCount returns how many markets the snapshot carries.
func (s Snapshot) MarketCount() int {
	n := 0
	for _, e := range s.Events {
		n += len(e.Markets)
	}
	return n
}

// Validate asserts the invariants every adapter's output must satisfy.
//
// It is not defensive noise. Three of these checks catch bugs that are otherwise
// silent and produce a plausible wrong number rather than an error:
//
//   - a selection whose role its market type does not admit (an "over" on a
//     moneyline) prices and grades against machinery that was never written for
//     it;
//   - a price whose line has drifted from its selection's effective line settles
//     a wager at a handicap the customer never took — domain.ValidatePriceForSelection
//     calls this out by name;
//   - a spread's away price quoted at the home line, which is the exact bug the
//     home-perspective convention in domain/market.go exists to prevent.
//
// The ingest service is free to call this on every snapshot in dev and test; it
// is O(prices) and allocation-light.
func (s Snapshot) Validate() error {
	if s.Provider.IsZero() {
		return fmt.Errorf("snapshot: %w: no provider name", ErrInvalidSnapshot)
	}
	if err := s.Scope.Validate(); err != nil {
		return fmt.Errorf("snapshot from %s: %w", s.Provider, err)
	}
	if s.FetchedAt.IsZero() {
		return fmt.Errorf("snapshot from %s: %w: zero FetchedAt", s.Provider, ErrInvalidSnapshot)
	}
	events := make(map[domain.EventID]bool, len(s.Events))
	for _, e := range s.Events {
		if e.Event.IsZero() {
			return fmt.Errorf("snapshot from %s: %w: zero event", s.Provider, ErrInvalidSnapshot)
		}
		id := e.Event.ID()
		if events[id] {
			return fmt.Errorf("snapshot from %s: %w: event %s appears twice", s.Provider, ErrInvalidSnapshot, id)
		}
		events[id] = true
		if e.Event.LeagueID() != s.Scope.League {
			return fmt.Errorf("snapshot from %s: %w: event %s is in league %s, scope asked for %s",
				s.Provider, ErrInvalidSnapshot, id, e.Event.LeagueID(), s.Scope.League)
		}
		if !s.Scope.HasEvent(id) {
			return fmt.Errorf("snapshot from %s: %w: event %s is outside the requested scope",
				s.Provider, ErrInvalidSnapshot, id)
		}
		if err := validateEventSnapshot(e); err != nil {
			return fmt.Errorf("snapshot from %s: event %s: %w", s.Provider, id, err)
		}
	}
	return nil
}

func validateEventSnapshot(e EventSnapshot) error {
	markets := make(map[domain.MarketID]bool, len(e.Markets))
	for _, m := range e.Markets {
		if m.Market.IsZero() {
			return fmt.Errorf("%w: zero market", ErrInvalidSnapshot)
		}
		id := m.Market.ID()
		if markets[id] {
			return fmt.Errorf("%w: market %s appears twice", ErrInvalidSnapshot, id)
		}
		markets[id] = true
		if !m.Market.BelongsTo(e.Event) {
			return fmt.Errorf("%w: market %s names event %s, not %s",
				ErrInvalidSnapshot, id, m.Market.EventID(), e.Event.ID())
		}
		if err := validateMarketSnapshot(m); err != nil {
			return fmt.Errorf("market %s: %w", id, err)
		}
	}
	return nil
}

func validateMarketSnapshot(m MarketSnapshot) error {
	byID := make(map[domain.SelectionID]domain.Selection, len(m.Selections))
	for _, sel := range m.Selections {
		if sel.IsZero() {
			return fmt.Errorf("%w: zero selection", ErrInvalidSnapshot)
		}
		if _, dup := byID[sel.ID()]; dup {
			return fmt.Errorf("%w: selection %s appears twice", ErrInvalidSnapshot, sel.ID())
		}
		if err := domain.ValidateSelectionForMarket(m.Market, sel); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSnapshot, err)
		}
		byID[sel.ID()] = sel
	}
	seen := make(map[[2]string]bool, len(m.Prices))
	for _, p := range m.Prices {
		if p.IsZero() {
			return fmt.Errorf("%w: zero price", ErrInvalidSnapshot)
		}
		sel, ok := byID[p.SelectionID()]
		if !ok {
			return fmt.Errorf("%w: price quotes selection %s, which the market does not list",
				ErrInvalidSnapshot, p.SelectionID())
		}
		key := [2]string{string(p.SelectionID()), string(p.BookID())}
		if seen[key] {
			return fmt.Errorf("%w: two prices for selection %s at book %s",
				ErrInvalidSnapshot, p.SelectionID(), p.BookID())
		}
		seen[key] = true
		if p.ObservedAt().IsZero() {
			return fmt.Errorf("%w: price for selection %s carries no observation instant",
				ErrInvalidSnapshot, p.SelectionID())
		}
		if err := domain.ValidatePriceForSelection(m.Market, sel, p); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSnapshot, err)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// The interface
// -----------------------------------------------------------------------------

// Adapter is one odds source, as the ingest scheduler needs to see it.
//
// Every method is safe for concurrent use: the scheduler runs one goroutine per
// league window (CLAUDE.md §2's "a goroutine per poller"), and they share one
// adapter.
//
// # Contract
//
//   - Fetch and Catalogue take a context and MUST honour its deadline. The
//     caller sets one (CLAUDE.md §12: "every external call has a timeout"); an
//     adapter must also apply its own default so that a caller passing
//     context.Background() cannot hang the poller for ever.
//   - Every returned error must be classifiable by Classify. Wrap a sentinel
//     from errors.go, or return an *Error, so the scheduler can tell "retry in a
//     moment" from "your API key is wrong" from "the budget is gone". A bare
//     unclassifiable error is treated as retryable and will therefore be retried
//     for ever.
//   - No method may log, wrap, or otherwise reveal a credential. See errors.go.
//   - Fetch returns a FULL statement of the scope's current state. Suppressing
//     unchanged markets is the ingest service's job, not the adapter's.
//
// ProviderAdapter is the name CLAUDE.md §5 gives this interface; the two names
// are the same type.
type Adapter interface {
	// Name identifies the adapter. It is the {provider} in
	// odds.raw.{provider} and the `provider` Prometheus label. It never
	// changes over an adapter's lifetime.
	Name() Name

	// Catalogue returns the sports, leagues and books this adapter can serve.
	// It is expected to be cheap — free, for The Odds API — and the scheduler
	// refreshes it far more often than it fetches prices.
	Catalogue(ctx context.Context) (Catalogue, error)

	// Fetch returns the current state of every market in scope.
	//
	// It reports ErrQuotaExhausted rather than a partial or stale answer when
	// the budget is gone. ADR 0003: "The limiter must fail to synthetic, not
	// fail to stale. When the budget is exhausted the correct behaviour is a
	// loud alert and a visible degraded state — never a board that silently
	// shows hour-old prices as if they were live."
	Fetch(ctx context.Context, scope Scope) (Snapshot, error)

	// Cost reports the credits one Fetch of scope will consume, so the
	// scheduler's token bucket can charge the right amount before spending it.
	// ADR 0003 makes this provider-specific: The Odds API bills
	// markets × regions, so it is not derivable from the scope alone.
	//
	// It performs no I/O and reads no clock.
	Cost(scope Scope) int

	// Quota reports the budget reading the adapter currently holds.
	//
	// It performs NO I/O — asking a provider how much quota is left would
	// itself spend quota. A real adapter returns what the last response's
	// headers said, with Known false until the first successful request.
	Quota() Quota
}

// ProviderAdapter is CLAUDE.md §5's name for Adapter. It is an alias, not a
// second interface: there is exactly one seam here.
type ProviderAdapter = Adapter

// -----------------------------------------------------------------------------
// Instrumentation
// -----------------------------------------------------------------------------

// Metric namespace. deploy/observability/prometheus.yml states the rule: every
// application series is prefixed `sharpline_`.
const metricNamespace = "sharpline"

// Pipeline stage labels for sharpline_odds_staleness_seconds.
//
// The histogram is emitted at four points by three services, and the alert rules
// select single stages by exact string ("fanout" is the SLO, "received" is the
// provider's share). Four spellings of one vocabulary would break a rule
// silently — Prometheus answers a query for a series that does not exist with no
// data rather than with an error — so the strings are declared once here.
const (
	// StageReceived is measured by ingest, at the adapter boundary:
	// FetchedAt − observed_at. migrations/00003 defines it as exactly
	// (ingested_at − observed_at), the provider-attributable share.
	StageReceived = "received"

	// StageNormalized is measured by ingest after normalization.
	StageNormalized = "normalized"

	// StagePriced is measured by pricer.
	StagePriced = "priced"

	// StageFanout is measured by stream at the client socket. THIS ONE IS THE
	// HEADLINE SLO; the recording rules read only this stage.
	StageFanout = "fanout"
)

// Request outcome labels. A closed set.
const (
	outcomeOK              = "ok"
	outcomeRetryable       = "retryable"
	outcomeFatal           = "fatal"
	outcomeQuotaExhausted  = "quota_exhausted"
	outcomeInvalidResponse = "invalid_response"
)

// StalenessBuckets returns the histogram boundaries for
// sharpline_odds_staleness_seconds.
//
// THEY ARE PART OF THE CONTRACT, not a tuning choice.
// deploy/observability/rules/sharpline-alerts.yml says so explicitly: several
// rules select a single bucket by an exact `le` literal, and "if the emitted
// histogram has no boundary at that value the selector matches NOTHING, the rule
// silently evaluates to empty, and the SLI reads as absent rather than as
// broken."
//
// le="120" is the SLO-1 compliance bucket and MUST be present. The range reaches
// 300s because ADR 0003 buys a 90-second live cadence, so a legitimate
// observation is routinely far above the sub-second values a pipeline histogram
// would use.
//
// It is exported because pricer and stream emit the same histogram at their own
// stages, and three copies of this slice would eventually differ.
func StalenessBuckets() []float64 {
	return []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 45, 60, 90, 120, 180, 300}
}

// Metrics is the provider layer's Prometheus instrumentation. It wraps any
// Adapter, so the two adapters cannot disagree about a series name.
//
// # These four series are read from outside this package
//
//	sharpline_provider_quota_remaining{provider}    dashboard panels 12 & 21; alerts ProviderQuotaLow, ProviderQuotaExhausted; dashboard `provider` template variable
//	sharpline_provider_quota_limit{provider}        alert ProviderQuotaLow (the denominator)
//	sharpline_provider_requests_total{provider,outcome}   dashboard panel 21, summed by provider
//	sharpline_odds_staleness_seconds{stage,league,provider}  the headline SLO histogram, at stage="received"
//	sharpline_odds_clock_skew_total{provider,stage}  dashboard panel 24; alert ProviderClockSkewDetected
//
// Two more series the dashboard reads belong to the SCHEDULER, not here, and are
// named so nobody emits them twice:
// sharpline_ingest_polls_total{provider,result} and
// sharpline_ingest_poll_interval_seconds{provider,window}. The second is not
// optional — OddsPollCadenceUnknown fires when it is absent while prices are
// flowing, because without it the headline SLO has no threshold to compare
// against, and `window="live"` must be one of its label values.
//
// # One value per process
//
// Registration happens once, in NewMetrics, and the value is injected into
// whatever wraps the adapter. Passing a nil Registerer builds the collectors
// WITHOUT registering them, which is right for a unit test and for a job that
// serves no /metrics endpoint: the observe calls stay live and cost a few
// nanoseconds, so no call site needs a nil check. This mirrors
// internal/platform/kafka's NewMetrics exactly.
type Metrics struct {
	quotaRemaining *prometheus.GaugeVec
	quotaLimit     *prometheus.GaugeVec
	requests       *prometheus.CounterVec
	staleness      *prometheus.HistogramVec
	clockSkew      *prometheus.CounterVec
}

// NewMetrics builds the provider collectors and registers them with reg. A nil
// reg builds them unregistered.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		quotaRemaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      "provider_quota_remaining",
			Help:      "Provider request credits remaining, as reported by the provider itself.",
		}, []string{"provider"}),

		quotaLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      "provider_quota_limit",
			Help:      "Size of the provider request budget the remaining credits are measured against.",
		}, []string{"provider"}),

		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "provider_requests_total",
			Help:      "Requests issued to an odds provider, by outcome.",
		}, []string{"provider", "outcome"}),

		staleness: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "odds_staleness_seconds",
			Help:      "Age of a price, measured from the provider's own observation instant. stage=received is the provider-attributable share.",
			Buckets:   StalenessBuckets(),
		}, []string{"stage", "league", "provider"}),

		clockSkew: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "odds_clock_skew_total",
			Help:      "Prices whose observation instant was in the future, so the staleness observation was clamped to zero.",
		}, []string{"provider", "stage"}),
	}
	if reg == nil {
		return m, nil
	}
	// The three sharpline_provider_* series are owned here and nowhere else, so a
	// second registration of one of them is a genuine bug and stays fatal.
	for _, c := range []prometheus.Collector{
		m.quotaRemaining, m.quotaLimit, m.requests,
	} {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("provider metrics: %w", err)
		}
	}
	// sharpline_odds_staleness_seconds and sharpline_odds_clock_skew_total are
	// SHARED contract series: this package emits stage="received" and
	// internal/ingest/normalizer emits stage="normalized" for the same series in
	// the same process. Whichever constructor runs second must adopt the existing
	// collector rather than fail.
	//
	// This block exists because the order used to be load-bearing and untyped:
	// cmd/ingest happens to build the provider set before the normalizer's, and
	// reversing those two lines killed the process at startup with "duplicate
	// metrics collector registration attempted". Adoption makes the order
	// irrelevant, which is what it always should have been.
	//
	// AlreadyRegisteredError is returned only for an IDENTICAL descriptor, so a
	// disagreement about help text, bucket boundaries or label names still fails
	// loudly — that check is the reason to route through the registry at all.
	var err error
	if m.staleness, err = sharedProviderHistogramVec(reg, m.staleness); err != nil {
		return nil, err
	}
	if m.clockSkew, err = sharedProviderCounterVec(reg, m.clockSkew); err != nil {
		return nil, err
	}
	return m, nil
}

// sharedProviderHistogramVec registers a contract histogram another package in
// this process may already own, and adopts the existing collector if so.
func sharedProviderHistogramVec(reg prometheus.Registerer, c *prometheus.HistogramVec) (*prometheus.HistogramVec, error) {
	existing, err := sharedProviderCollector(reg, c)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return c, nil
	}
	v, ok := existing.(*prometheus.HistogramVec)
	if !ok {
		return nil, fmt.Errorf("provider metrics: a collector of type %T is already registered "+
			"where a *prometheus.HistogramVec was expected", existing)
	}
	return v, nil
}

// sharedProviderCounterVec is sharedProviderHistogramVec for a counter.
func sharedProviderCounterVec(reg prometheus.Registerer, c *prometheus.CounterVec) (*prometheus.CounterVec, error) {
	existing, err := sharedProviderCollector(reg, c)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return c, nil
	}
	v, ok := existing.(*prometheus.CounterVec)
	if !ok {
		return nil, fmt.Errorf("provider metrics: a collector of type %T is already registered "+
			"where a *prometheus.CounterVec was expected", existing)
	}
	return v, nil
}

// sharedProviderCollector returns the already-registered collector, or nil when
// c was the one registered.
func sharedProviderCollector(reg prometheus.Registerer, c prometheus.Collector) (prometheus.Collector, error) {
	err := reg.Register(c)
	if err == nil {
		return nil, nil
	}
	var already prometheus.AlreadyRegisteredError
	if errors.As(err, &already) {
		return already.ExistingCollector, nil
	}
	return nil, fmt.Errorf("provider metrics: %w", err)
}

// ObserveQuota publishes a quota reading.
//
// A reading with Known false publishes nothing. That is deliberate:
// ProviderQuotaExhausted alerts on `sharpline_provider_quota_remaining == 0`, so
// exporting a zero for "we have not asked yet" would page for a healthy system
// during every cold start.
func (m *Metrics) ObserveQuota(name Name, q Quota) {
	if m == nil || !q.Known {
		return
	}
	m.quotaRemaining.WithLabelValues(name.String()).Set(float64(q.Remaining))
	m.quotaLimit.WithLabelValues(name.String()).Set(float64(q.Limit))
}

// ObserveRequest counts one provider request by its outcome. A nil err counts as
// "ok"; anything else is counted by its Disposition, so the label set stays
// closed and the error text never becomes a label.
func (m *Metrics) ObserveRequest(name Name, err error) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(name.String(), requestOutcome(err)).Inc()
}

func requestOutcome(err error) string {
	switch {
	case err == nil:
		return outcomeOK
	case errors.Is(err, ErrMalformedPayload):
		return outcomeInvalidResponse
	}
	switch Classify(err) {
	case DispositionQuotaExhausted:
		return outcomeQuotaExhausted
	case DispositionFatal:
		return outcomeFatal
	default:
		return outcomeRetryable
	}
}

// ObserveSnapshot records the provider-attributable staleness of every price in
// a snapshot, at stage="received".
//
// # Negative staleness is a real state and is not swallowed
//
// A provider may stamp an observation instant slightly in the future.
// domain.Price.Age returns the negative duration for exactly that reason —
// "returning it rather than clamping to zero is what lets a monitor detect the
// skew instead of silently reporting healthy staleness" — and migrations/00003
// declines a CHECK constraint so the skewed value stays storable.
//
// A Prometheus histogram would destroy that signal: a negative observation lands
// in the lowest bucket and reads as EXCELLENT freshness. So the contract in
// sharpline-alerts.yml is that the emitter clamps the observation at 0 AND
// counts the clamp in sharpline_odds_clock_skew_total. Clamping is never silent;
// ProviderClockSkewDetected alerts on the counter.
func (m *Metrics) ObserveSnapshot(s Snapshot) {
	if m == nil {
		return
	}
	provider := s.Provider.String()
	skew := 0
	for _, e := range s.Events {
		league := e.Event.LeagueID().String()
		for _, mk := range e.Markets {
			for _, p := range mk.Prices {
				age := p.Age(s.FetchedAt).Seconds()
				if age < 0 {
					skew++
					age = 0
				}
				m.staleness.WithLabelValues(StageReceived, league, provider).Observe(age)
			}
		}
	}
	if skew > 0 {
		m.clockSkew.WithLabelValues(provider, StageReceived).Add(float64(skew))
	}
}
