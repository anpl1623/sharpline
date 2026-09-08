// The record this package publishes to odds.normalized, and its round trip back
// into domain values.
//
// # Why a DTO instead of the domain types
//
// Every domain type has unexported fields and a validating constructor, which is
// what makes an invalid Market unrepresentable. It also makes them unmarshalable:
// encoding/json cannot write them and, more importantly, must never be able to
// READ them, because a decoder that could set fields directly would be a second
// construction path that skips every check NewMarket makes.
//
// So the wire shape is its own type, and MarketView is the only way back. Decode
// gives you a NormalizedMarket; Domain() gives you validated domain values or an
// error. There is no path that produces a domain value the constructors would
// have refused.
//
// # What a record is
//
// One record per MARKET, keyed by market_id, on a compacted topic. It is
// self-contained: it carries the sport, league, event and books it refers to, not
// just their identifiers. That is denormalised on purpose. A consumer that builds
// its state from the compacted log alone — which is exactly what `stream` does to
// answer a new client's snapshot request, and what this package does to warm its
// own fingerprints — must be able to render a board from the log and nothing else.
// Requiring a second lookup would reintroduce the ordering problem the compacted
// topic was chosen to remove.
//
// The cost is repetition: an event's name is repeated once per market on it.
// Measured against the alternative — a catalogue topic, a join, and a window
// during which a market references an event nobody has seen yet — it is cheap.
//
// # What a record deliberately does NOT carry
//
// The live score and the game clock. They change every few seconds, and a record
// that carried them would have to be republished for every market on an event on
// every score change, which is the exact bus flood change detection exists to
// prevent. They are not needed to price a market. Event status IS carried,
// because it gates whether a market accepts wagers.
//
// THAT EXCLUSION IS RIGHT AND IT LEAVES SETTLEMENT WITH NO INPUT. A FINAL score
// changes exactly once, and nothing else in the tree carries it, so events.score_*
// is NULL on every row of a running stack and internal/settlement's results feed
// is permanently empty. Two further facts make it structural rather than a
// tuning problem: raw.go's RawEvent has no status, score or completed field, so a
// provider cannot state a result even when it knows one; and mapper.go derives
// status from timestamps alone, so EventStatusEnded is unreachable and the status
// carried here is only ever scheduled or live.
//
// The fix is NOT to relax this exclusion — the flood argument above still holds.
// CLAUDE.md §3's event flow already draws results as their own source into
// `settle`, and that is the seam to build. See internal/settlement/pgstore/doc.go,
// which carries the full chain and the evidence.
package normalizer

import (
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// SchemaVersion is the version of the NormalizedMarket document.
//
// It is INSIDE the fingerprint (see fingerprint.go), so bumping it republishes
// every market once. That is the point: a payload whose shape changed must reach
// consumers, and a consumer's compacted snapshot must not be left holding a
// document in a shape its decoder no longer expects.
//
// This is version-of-the-payload, distinct from kafka.EnvelopeVersion, which
// versions the frame around it. The envelope's doc states the same split.
const SchemaVersion = 1

// MessageType is the kafka.Message.Type this package publishes. Consumers switch
// on it.
const MessageType = "odds.normalized.v1"

// NormalizedMarket is one market and every price currently quoted on it.
type NormalizedMarket struct {
	// SchemaVersion is SchemaVersion as written.
	SchemaVersion int `json:"schema_version"`

	// Provider is the adapter slug this record came from — "the-odds-api" or
	// "synthetic". It is here so a consumer can TELL when a record is synthetic,
	// which ADR 0003 requires: "the synthetic fallback covers development but
	// must not silently substitute for real data in a running deployment — that
	// would be indistinguishable from fabricating market data. Failover must be
	// explicit and surfaced."
	Provider string `json:"provider"`

	// Fingerprint is the hex SHA-256 that change detection compared to decide
	// this record was worth publishing. It is carried so a consumer can
	// deduplicate a redelivery without rehashing, and so warm start can verify
	// its own recomputation against what the producer actually decided.
	Fingerprint string `json:"fingerprint"`

	Sport      SportRef       `json:"sport"`
	League     LeagueRef      `json:"league"`
	Event      EventRef       `json:"event"`
	Market     MarketRef      `json:"market"`
	Books      []BookRef      `json:"books"`
	Selections []SelectionRef `json:"selections"`
	Prices     []PriceRef     `json:"prices"`

	// ObservedAt is the NEWEST observation instant among this record's prices.
	//
	// It answers "when was this version of the record observed", which is what
	// the envelope needs for ordering and what a monotonicity guard compares. It
	// is NOT the staleness subtrahend: the dashboard defines freshness as
	// "instant the price is written to the client socket − observed_at carried on
	// THAT PRICE", so staleness is per-price and PriceRef.ObservedAt is the value
	// to use. Using this field instead would report the freshest book's age for
	// every book on the market.
	ObservedAt time.Time `json:"observed_at"`

	// IngestedAt is when `ingest` received the payload this record was derived
	// from — the raw envelope's ProducedAt, propagated unchanged.
	//
	// migrations/00003 and the phase-2 handoff are both emphatic that this and
	// ObservedAt are not interchangeable: (IngestedAt − ObservedAt) is the
	// provider-attributable share of staleness, the part no engineering here can
	// lower, and it is what sharpline_odds_staleness_seconds{stage="received"}
	// measures.
	IngestedAt time.Time `json:"ingested_at"`
}

// SportRef is a sport as carried on the wire.
type SportRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// LeagueRef is a league as carried on the wire.
type LeagueRef struct {
	ID      string `json:"id"`
	SportID string `json:"sport_id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
}

// CompetitorRef is one side of a match. ID is optional — domain.NewCompetitor
// documents that "providers frequently supply a display name and nothing else".
type CompetitorRef struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

// EventRef is an event as carried on the wire. Score and clock are deliberately
// absent; see the file comment.
type EventRef struct {
	ID             string        `json:"id"`
	LeagueID       string        `json:"league_id"`
	Kind           string        `json:"kind"`
	Name           string        `json:"name"`
	Home           CompetitorRef `json:"home,omitzero"`
	Away           CompetitorRef `json:"away,omitzero"`
	ScheduledStart time.Time     `json:"scheduled_start"`
	Status         string        `json:"status"`
}

// MarketRef is a market as carried on the wire.
type MarketRef struct {
	ID      string `json:"id"`
	EventID string `json:"event_id"`
	Type    string `json:"type"`

	// ProviderKey is the provider's own market key ("h2h", "player_pass_tds").
	// It is retained because it is an input to the market identifier and because
	// it is the only thing that distinguishes two provider markets that map to
	// one domain type.
	ProviderKey string `json:"provider_key"`

	// Line is the CONSENSUS line across the quoting books, from the home side's
	// perspective for a spread and absolute for a total, per the convention
	// domain.Market documents. It is null for a moneyline or a futures market.
	//
	// It is not any single book's line. PriceRef.Line carries that, and the two
	// legitimately differ — migrations/00003: "markets.line carries the market's
	// CURRENT line. This column carries the line THE QUOTE WAS MADE AT. They are
	// different facts."
	Line domain.Line `json:"line"`

	// Subject names the individual a player prop is about. Empty otherwise.
	Subject string `json:"subject,omitempty"`

	Status string `json:"status"`

	// UpdatedAt stamps the observation this market state came from. It is not in
	// the fingerprint, so on a suppressed poll the published record keeps the
	// instant of the last CHANGE, which is the more useful of the two readings.
	UpdatedAt time.Time `json:"updated_at"`
}

// BookRef is a bookmaker as carried on the wire.
type BookRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`

	// Kind is "external" or "synthetic". A synthetic book's quotes are computed
	// by this system, not observed from a real bookmaker, and every consumer
	// that displays a price must be able to say so.
	Kind string `json:"kind"`

	// Reference is the catalogue's sharp-reference designation, propagated from
	// RawBook.Reference. internal/pricing resolves a market's fair value from
	// the designated book first and falls back to its own configured preference
	// list only when no book on the record carries this flag — and it records
	// which of the two happened on every computed record.
	Reference bool `json:"reference,omitempty"`
}

// SelectionRef is a selection as carried on the wire.
type SelectionRef struct {
	ID       string `json:"id"`
	MarketID string `json:"market_id"`
	Role     string `json:"role"`
	Name     string `json:"name"`
}

// PriceRef is one book's quote on one selection.
type PriceRef struct {
	SelectionID string `json:"selection_id"`
	BookID      string `json:"book_id"`

	// Decimal is the canonical price format. The phase-1 handoff: "Decimal is
	// the canonical price type. Store, transport and compute with it. American
	// and Fractional are DISPLAY formats — convert at the edge, render, discard."
	Decimal float64 `json:"decimal"`

	// Line is the line THIS BOOK quoted, from this selection's own perspective.
	Line domain.Line `json:"line"`

	// ObservedAt is this quote's own provider observation instant, and it is the
	// subtrahend the staleness SLO is defined on.
	ObservedAt time.Time `json:"observed_at"`
}

// MarketView is the domain-typed form of a NormalizedMarket.
//
// The mapper produces one of these first and derives the wire record from it, so
// the wire record can never describe something the domain constructors would have
// rejected.
type MarketView struct {
	Sport      domain.Sport
	League     domain.League
	Event      domain.Event
	Market     domain.Market
	Books      []domain.Book
	Selections []domain.Selection
	Prices     []domain.Price

	// ProviderKey is the provider's own market key. It is not a domain concept —
	// the domain has no opinion about what a provider calls a market — but it is
	// an input to MarketIDFor, so it has to survive onto the wire for the
	// derivation to be reproducible from a published record alone.
	ProviderKey string
}

// ObservedAt returns the newest observation instant among the view's prices, and
// whether there is one.
func (v MarketView) ObservedAt() (time.Time, bool) {
	var newest time.Time
	for _, p := range v.Prices {
		if p.ObservedAt().After(newest) {
			newest = p.ObservedAt()
		}
	}
	return newest, !newest.IsZero()
}

// newRecord builds the wire record from a validated view.
//
// The fingerprint is left empty; the caller computes it over the finished record
// and fills it in, which is what makes "the fingerprint covers the published
// payload" true by construction rather than by a parallel list of fields.
func newRecord(provider string, v MarketView, ingestedAt time.Time) NormalizedMarket {
	observedAt, _ := v.ObservedAt()

	rec := NormalizedMarket{
		SchemaVersion: SchemaVersion,
		Provider:      provider,
		Sport: SportRef{
			ID:   v.Sport.ID().String(),
			Slug: v.Sport.Slug().String(),
			Name: v.Sport.Name(),
		},
		League: LeagueRef{
			ID:      v.League.ID().String(),
			SportID: v.League.SportID().String(),
			Slug:    v.League.Slug().String(),
			Name:    v.League.Name(),
		},
		Event: EventRef{
			ID:             v.Event.ID().String(),
			LeagueID:       v.Event.LeagueID().String(),
			Kind:           v.Event.Kind().String(),
			Name:           v.Event.Name(),
			ScheduledStart: v.Event.ScheduledStart(),
			Status:         v.Event.Status().String(),
		},
		Market: MarketRef{
			ID:          v.Market.ID().String(),
			EventID:     v.Market.EventID().String(),
			Type:        v.Market.Type().String(),
			ProviderKey: v.ProviderKey,
			Line:        v.Market.Line(),
			Subject:     v.Market.Subject(),
			Status:      v.Market.Status().String(),
			UpdatedAt:   v.Market.UpdatedAt(),
		},
		Books:      make([]BookRef, 0, len(v.Books)),
		Selections: make([]SelectionRef, 0, len(v.Selections)),
		Prices:     make([]PriceRef, 0, len(v.Prices)),
		ObservedAt: observedAt,
		IngestedAt: ingestedAt.UTC(),
	}
	if !v.Event.Home().IsZero() {
		rec.Event.Home = CompetitorRef{ID: v.Event.Home().ID().String(), Name: v.Event.Home().Name()}
	}
	if !v.Event.Away().IsZero() {
		rec.Event.Away = CompetitorRef{ID: v.Event.Away().ID().String(), Name: v.Event.Away().Name()}
	}
	for _, b := range v.Books {
		rec.Books = append(rec.Books, BookRef{
			ID: b.ID().String(), Slug: b.Slug().String(), Name: b.Name(), Kind: b.Kind().String(),
			Reference: b.IsReference(),
		})
	}
	for _, s := range v.Selections {
		rec.Selections = append(rec.Selections, SelectionRef{
			ID: s.ID().String(), MarketID: s.MarketID().String(),
			Role: s.Role().String(), Name: s.Name(),
		})
	}
	for _, p := range v.Prices {
		rec.Prices = append(rec.Prices, PriceRef{
			SelectionID: p.SelectionID().String(), BookID: p.BookID().String(),
			Decimal: p.Decimal(), Line: p.Line(), ObservedAt: p.ObservedAt(),
		})
	}
	return rec
}

// MarketID returns the record's market identifier, validated.
//
// It is what the record is keyed by on the compacted topic, so a caller that
// publishes must get it from here rather than from the raw string — the typed
// identifier is what makes kafka.OddsProducer.PublishNormalized's signature a
// compile-time guarantee.
func (m NormalizedMarket) MarketID() (domain.MarketID, error) {
	return domain.NewMarketID(m.Market.ID)
}

// Domain rebuilds validated domain values from a decoded record.
//
// Every consumer of odds.normalized — the pricer, the Timescale line-history
// writer, the fanout hub — should go through this rather than reading the wire
// structs directly. A record that has been on a compacted topic for a month was
// written by an older build; running it back through the constructors is what
// catches the case where it no longer satisfies today's invariants, instead of
// letting it through as a plausible-looking struct.
//
// It runs domain.ValidateSelectionForMarket on every selection and deliberately
// does NOT run domain.ValidatePriceForSelection on every price. That validator
// requires a price's line to equal its selection's EFFECTIVE line, which is the
// right rule on the wagering path — "settling a wager against it grades a bet at
// a handicap the customer never took" — and the wrong rule here, where the market
// line is a consensus across books and a book quoting -3 against a -3.5 consensus
// is normal market disagreement rather than corruption. Multi-book comparison
// (CLAUDE.md §6) exists precisely to show that disagreement.
func (m NormalizedMarket) Domain() (MarketView, error) {
	var v MarketView
	v.ProviderKey = m.Market.ProviderKey

	if m.SchemaVersion != SchemaVersion {
		return v, fmt.Errorf("normalized market %q: schema version %d, this build reads %d",
			sample(m.Market.ID), m.SchemaVersion, SchemaVersion)
	}

	sportID, err := domain.NewSportID(m.Sport.ID)
	if err != nil {
		return v, err
	}
	sportSlug, err := domain.NewSlug(m.Sport.Slug)
	if err != nil {
		return v, err
	}
	if v.Sport, err = domain.NewSport(domain.SportParams{
		ID: sportID, Slug: sportSlug, Name: m.Sport.Name,
	}); err != nil {
		return v, err
	}

	leagueID, err := domain.NewLeagueID(m.League.ID)
	if err != nil {
		return v, err
	}
	leagueSlug, err := domain.NewSlug(m.League.Slug)
	if err != nil {
		return v, err
	}
	if v.League, err = domain.NewLeague(domain.LeagueParams{
		ID: leagueID, SportID: sportID, Slug: leagueSlug, Name: m.League.Name,
	}); err != nil {
		return v, err
	}

	if v.Market, err = m.Market.domain(); err != nil {
		return v, err
	}
	if v.Event, err = m.Event.domain(v.Market.UpdatedAt()); err != nil {
		return v, err
	}

	v.Books = make([]domain.Book, 0, len(m.Books))
	for _, b := range m.Books {
		id, err := domain.NewBookID(b.ID)
		if err != nil {
			return v, err
		}
		slug, err := domain.NewSlug(b.Slug)
		if err != nil {
			return v, err
		}
		kind, err := domain.ParseBookKind(b.Kind)
		if err != nil {
			return v, err
		}
		book, err := domain.NewBook(domain.BookParams{
			ID: id, Slug: slug, Name: b.Name, Kind: kind, Reference: b.Reference,
		})
		if err != nil {
			return v, err
		}
		v.Books = append(v.Books, book)
	}

	v.Selections = make([]domain.Selection, 0, len(m.Selections))
	for _, s := range m.Selections {
		id, err := domain.NewSelectionID(s.ID)
		if err != nil {
			return v, err
		}
		marketID, err := domain.NewMarketID(s.MarketID)
		if err != nil {
			return v, err
		}
		role, err := domain.ParseSelectionRole(s.Role)
		if err != nil {
			return v, err
		}
		sel, err := domain.NewSelection(domain.SelectionParams{
			ID: id, MarketID: marketID, Role: role, Name: s.Name,
		})
		if err != nil {
			return v, err
		}
		if err := domain.ValidateSelectionForMarket(v.Market, sel); err != nil {
			return v, err
		}
		v.Selections = append(v.Selections, sel)
	}

	v.Prices = make([]domain.Price, 0, len(m.Prices))
	for _, p := range m.Prices {
		selectionID, err := domain.NewSelectionID(p.SelectionID)
		if err != nil {
			return v, err
		}
		bookID, err := domain.NewBookID(p.BookID)
		if err != nil {
			return v, err
		}
		price, err := domain.NewPrice(domain.PriceParams{
			SelectionID: selectionID, BookID: bookID,
			Decimal: p.Decimal, Line: p.Line, ObservedAt: p.ObservedAt,
		})
		if err != nil {
			return v, err
		}
		v.Prices = append(v.Prices, price)
	}

	return v, nil
}

// domain rebuilds a domain.Event from its wire form.
//
// updatedAt comes from the market rather than from a field of its own. An event's
// observation instant carried on a per-market record would be that market's
// instant anyway, and a second copy of one fact is a second copy that drifts.
func (e EventRef) domain(updatedAt time.Time) (domain.Event, error) {
	id, err := domain.NewEventID(e.ID)
	if err != nil {
		return domain.Event{}, err
	}
	leagueID, err := domain.NewLeagueID(e.LeagueID)
	if err != nil {
		return domain.Event{}, err
	}
	kind, err := domain.ParseEventKind(e.Kind)
	if err != nil {
		return domain.Event{}, err
	}
	status, err := domain.ParseEventStatus(e.Status)
	if err != nil {
		return domain.Event{}, err
	}
	var home, away domain.Competitor
	if e.Home.Name != "" {
		if home, err = domain.NewCompetitor(domain.CompetitorID(e.Home.ID), e.Home.Name); err != nil {
			return domain.Event{}, err
		}
	}
	if e.Away.Name != "" {
		if away, err = domain.NewCompetitor(domain.CompetitorID(e.Away.ID), e.Away.Name); err != nil {
			return domain.Event{}, err
		}
	}
	return domain.NewEvent(domain.EventParams{
		ID: id, LeagueID: leagueID, Kind: kind, Name: e.Name,
		Home: home, Away: away, ScheduledStart: e.ScheduledStart,
		Status: status, UpdatedAt: updatedAt,
	})
}

// domain rebuilds a domain.Market from its wire form.
func (m MarketRef) domain() (domain.Market, error) {
	id, err := domain.NewMarketID(m.ID)
	if err != nil {
		return domain.Market{}, err
	}
	eventID, err := domain.NewEventID(m.EventID)
	if err != nil {
		return domain.Market{}, err
	}
	typ, err := domain.ParseMarketType(m.Type)
	if err != nil {
		return domain.Market{}, err
	}
	status, err := domain.ParseMarketStatus(m.Status)
	if err != nil {
		return domain.Market{}, err
	}
	return domain.NewMarket(domain.MarketParams{
		ID: id, EventID: eventID, Type: typ, Line: m.Line,
		Subject: m.Subject, Status: status, UpdatedAt: m.UpdatedAt,
	})
}
