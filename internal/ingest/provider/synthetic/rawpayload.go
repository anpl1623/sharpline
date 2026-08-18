package synthetic

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
)

// The bytes this "provider" says it sent.
//
// # Why the neutral shape rather than a bespoke one
//
// normalizer/raw.go settles it: RawEvent's "JSON tags are a wire contract with
// the synthetic adapter, which marshals this type directly: the synthetic
// provider is ours and has no legacy format to absorb, so its raw shape is the
// neutral one and NeutralDecoder is a plain unmarshal." Inventing a second
// serialisation here would buy a second decoder to keep in agreement with the
// first, and the two would eventually disagree about something small and
// invisible.
//
// # Why produce raw bytes at all for a generator
//
// Because odds.raw.{provider} is the replayable record of what the provider
// actually said (provider.RawPayload), and that record is worth having precisely
// when something has gone wrong in parsing. A synthetic feed that published only
// its already-parsed form would leave the raw topic empty on the offline path,
// which is the path CI runs — so the raw producer, the topic, the retention
// policy and the decoder would all be exercised only by whoever holds an API
// key.
//
// # The keys are the adapter's own, and they are stable
//
// The identifiers inside this payload — the event key, the league key, the book
// keys — are the PROVIDER-NATIVE ones, and the normalizer derives the domain
// identifiers from them (normalizer/identity.go). So their stability is what
// keeps the compacted odds.normalized topic collapsing: universe.go composes
// each from the league, the event's own scheduled date and its slot, none of
// which move for a given seed.

// rawContentType is the payload's media type. It never varies; a constant makes
// that a fact rather than a repeated literal.
const rawContentType = "application/json"

// rawEventFor renders one event and its markets as the neutral shape.
//
// books is indexed the same way a.books is, and marketRaws[m][b] is market m as
// book b quotes it. A nil entry means that book has no quote on that market,
// which is how a suspended or closed market appears: the book has taken the
// price down, so it is absent from the payload rather than present with an empty
// outcome list.
func (a *Adapter) rawEventFor(es *eventState, marketRaws [][]normalizer.RawMarket) normalizer.RawEvent {
	l := es.ev.league
	re := normalizer.RawEvent{
		ID:           string(es.ev.id),
		SportKey:     l.sportSlug,
		SportName:    l.sportName,
		LeagueKey:    string(l.leagueID()),
		LeagueName:   l.name,
		Name:         es.ev.name,
		CommenceTime: es.ev.start.UTC(),
	}
	if es.ev.kind == domain.EventKindMatch {
		re.HomeTeam = es.ev.home.Name()
		re.AwayTeam = es.ev.away.Name()
	}

	for b, book := range a.books {
		var markets []normalizer.RawMarket
		for _, per := range marketRaws {
			if per == nil || per[b].Key == "" {
				continue
			}
			markets = append(markets, per[b])
		}
		if len(markets) == 0 {
			continue
		}
		re.Books = append(re.Books, normalizer.RawBook{
			Key:  book.slug,
			Name: book.name,
			// universe.go designates the in-house book as the sharp reference.
			// Carrying it here is what makes that designation reach the
			// normalizer, the database and internal/pricing rather than dying
			// at this boundary.
			Reference:  book.reference,
			LastUpdate: a.stepTime(es.n - int64(book.lagSteps)),
			Markets:    markets,
		})
	}
	return re
}

// marshalRaw encodes the neutral shape into a provider.RawPayload.
//
// A marshalling failure is a bug in this package, not a provider problem, so it
// is classified fatal: retrying would re-encode the same unencodable value for
// ever. Nothing in RawEvent can hold a credential — the synthetic adapter has no
// credential — but the rule from provider/errors.go still applies to the error
// text, which is why it names the event identifier and nothing else.
func marshalRaw(re normalizer.RawEvent, observed time.Time) (provider.RawPayload, error) {
	body, err := json.Marshal(re)
	if err != nil {
		return provider.RawPayload{}, provider.Newf("fetch", provider.NameSynthetic,
			provider.DispositionFatal, provider.ErrMalformedPayload,
			"encoding event %s: %v", re.ID, err)
	}
	return provider.RawPayload{
		ContentType: rawContentType,
		Body:        body,
		ObservedAt:  observed,
	}, nil
}

// assertRawDecodes is a construction-time check that the payload this package
// writes is the payload the normalizer reads.
//
// It exists because the coupling is a documented wire contract between two
// packages with no shared type at the seam — RawEvent's JSON tags — and a change
// to either side would otherwise surface as an empty board rather than as a
// failure. It runs once, in New, over a single hand-built value, so it costs
// nothing per fetch.
func assertRawDecodes() error {
	dec, err := normalizer.NewNeutralDecoder(neutralProviderSlug)
	if err != nil {
		return fmt.Errorf("synthetic: neutral decoder: %w", err)
	}
	probe := normalizer.RawEvent{
		ID:           "syn-probe",
		LeagueKey:    "syn-probe",
		CommenceTime: time.Unix(0, 0).UTC(),
	}
	body, err := json.Marshal(probe)
	if err != nil {
		return fmt.Errorf("synthetic: encoding the decoder probe: %w", err)
	}
	got, err := dec.Decode(body)
	if err != nil {
		return fmt.Errorf("synthetic: the neutral decoder rejects this package's payload shape: %w", err)
	}
	if got.ID != probe.ID || got.LeagueKey != probe.LeagueKey {
		return fmt.Errorf("synthetic: the neutral decoder round trip lost fields: got %+v", got)
	}
	return nil
}
