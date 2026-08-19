package wsgw

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// -----------------------------------------------------------------------------
// Fixtures
//
// Everything below is built through the real types and marshalled with the real
// encoder, so a record that reaches the store has travelled the same path a
// record from the bus does. Nothing here hand-writes a payload: a fixture that
// diverged from what the pricer actually publishes would test this package
// against a document nobody produces.
// -----------------------------------------------------------------------------

const (
	sampleMarketID   = "mkt-1"
	sampleEventID    = "evt-1"
	sampleLeagueSlug = "nfl"
	sampleLeagueID   = "league-nfl"
	sampleProvider   = "synthetic"
)

var sampleObservedAt = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

// sampleComputed builds a minimal record that passes
// pricing.ComputedMarket.Validate: the schema version, a usable market
// identifier, a stated reference source, a devig method and at least
// odds.MinMarketSelections fair selections.
func sampleComputed(t *testing.T) pricing.ComputedMarket {
	t.Helper()
	return pricing.ComputedMarket{
		SchemaVersion:       pricing.SchemaVersion,
		Provider:            sampleProvider,
		SourceFingerprint:   "fingerprint",
		SourceSchemaVersion: normalizer.SchemaVersion,
		Sport:               normalizer.SportRef{ID: "americanfootball", Slug: "americanfootball", Name: "Football"},
		League:              normalizer.LeagueRef{ID: sampleLeagueID, SportID: "americanfootball", Slug: sampleLeagueSlug, Name: "NFL"},
		Event:               normalizer.EventRef{ID: sampleEventID, LeagueID: sampleLeagueID, Kind: "match", Name: "A at B", ScheduledStart: sampleObservedAt, Status: "scheduled"},
		Market:              normalizer.MarketRef{ID: sampleMarketID, EventID: sampleEventID, Type: "moneyline", ProviderKey: "h2h", Status: "open", UpdatedAt: sampleObservedAt},
		Reference: pricing.ReferenceRef{
			BookID:     domain.BookID("book-1"),
			Slug:       domain.Slug("pinnacle"),
			Name:       "Pinnacle",
			Kind:       domain.BookKindExternal,
			Source:     pricing.ReferenceSourceConfigured,
			ObservedAt: sampleObservedAt,
		},
		Fair: pricing.FairValue{
			Method:          odds.MethodMultiplicative,
			RequestedMethod: odds.MethodMultiplicative,
			Attribution:     "proportional",
			Selections: []pricing.FairSelection{
				{SelectionID: "sel-home", Role: domain.SelectionRoleHome, Name: "A", Probability: 0.5, Decimal: 2},
				{SelectionID: "sel-away", Role: domain.SelectionRoleAway, Name: "B", Probability: 0.5, Decimal: 2},
			},
		},
		Books: []pricing.BookAssessment{{
			BookID: domain.BookID("book-1"),
			Slug:   domain.Slug("pinnacle"),
			Name:   "Pinnacle",
			Kind:   domain.BookKindExternal,
			Quotes: []pricing.QuoteAssessment{
				{SelectionID: "sel-home", Status: pricing.QuoteStatusPriced, Decimal: 1.95, Implied: 0.5128, ObservedAt: sampleObservedAt},
				{SelectionID: "sel-away", Status: pricing.QuoteStatusPriced, Decimal: 1.95, Implied: 0.5128, ObservedAt: sampleObservedAt.Add(-2 * time.Second)},
			},
		}},
		ObservedAt: sampleObservedAt,
		IngestedAt: sampleObservedAt.Add(500 * time.Millisecond),
	}
}

// delivery wraps a computed market in the envelope the bus would carry it in.
//
// The Delivery's unexported value field stays zero, which is correct: nothing in
// this package reads Delivery.Value() — it reads Envelope.Data, because that is
// where the payload lives and because the raw record value would carry the
// envelope around it.
func delivery(t *testing.T, c pricing.ComputedMarket, offset int64) *kafka.Delivery {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal computed market: %v", err)
	}
	return &kafka.Delivery{
		Topic:     kafka.TopicPriceComputed,
		Partition: 0,
		Offset:    offset,
		Key:       c.Market.ID,
		Timestamp: c.ObservedAt,
		Envelope: kafka.Envelope{
			Version:    kafka.EnvelopeVersion,
			Type:       pricing.MessageType,
			ID:         c.SourceFingerprint,
			Producer:   "pricer",
			ProducedAt: c.IngestedAt,
			ObservedAt: c.ObservedAt,
			Data:       data,
		},
	}
}

// tombstone builds the deletion of one market key.
func tombstone(key string, observedAt time.Time) *kafka.Delivery {
	d := &kafka.Delivery{
		Topic:           kafka.TopicPriceComputed,
		Key:             key,
		Tombstone:       true,
		TombstoneReason: "settled",
		Headers:         map[string]string{},
	}
	if !observedAt.IsZero() {
		d.Headers[kafka.HeaderObservedAt] = observedAt.UTC().Format(time.RFC3339Nano)
	}
	return d
}

// -----------------------------------------------------------------------------
// Apply
// -----------------------------------------------------------------------------

func TestStoreApply(t *testing.T) {
	cases := []struct {
		name        string
		build       func(t *testing.T) *kafka.Delivery
		wantOutcome ApplyOutcome
		wantErr     error
		wantLen     int
	}{
		{
			name:        "a valid record is stored",
			build:       func(t *testing.T) *kafka.Delivery { return delivery(t, sampleComputed(t), 1) },
			wantOutcome: ApplyStored,
			wantLen:     1,
		},
		{
			name: "an unsupported message type is refused, not decoded",
			build: func(t *testing.T) *kafka.Delivery {
				d := delivery(t, sampleComputed(t), 1)
				d.Envelope.Type = "price.computed.v99"
				return d
			},
			wantOutcome: ApplyUnsupported,
			wantErr:     ErrUnsupportedMessage,
		},
		{
			name: "an undecodable payload is rejected",
			build: func(t *testing.T) *kafka.Delivery {
				d := delivery(t, sampleComputed(t), 1)
				d.Envelope.Data = json.RawMessage(`{"schema_version":"not a number"}`)
				return d
			},
			wantOutcome: ApplyRejected,
			wantErr:     ErrRecordRejected,
		},
		{
			name: "a document that fails Validate is rejected, not stored",
			build: func(t *testing.T) *kafka.Delivery {
				c := sampleComputed(t)
				// One fair selection: below odds.MinMarketSelections, so the
				// market has no defined margin and cannot be reasoned about.
				c.Fair.Selections = c.Fair.Selections[:1]
				return delivery(t, c, 1)
			},
			wantOutcome: ApplyRejected,
			wantErr:     ErrRecordRejected,
		},
		{
			name: "a record whose key disagrees with its payload is rejected",
			build: func(t *testing.T) *kafka.Delivery {
				d := delivery(t, sampleComputed(t), 1)
				d.Key = "some-other-market"
				return d
			},
			wantOutcome: ApplyRejected,
			wantErr:     ErrRecordRejected,
		},
		{
			name: "a record with no key is rejected",
			build: func(t *testing.T) *kafka.Delivery {
				d := delivery(t, sampleComputed(t), 1)
				d.Key = ""
				return d
			},
			wantOutcome: ApplyRejected,
			wantErr:     ErrRecordRejected,
		},
		{
			name: "a market that cannot be routed is rejected",
			build: func(t *testing.T) *kafka.Delivery {
				c := sampleComputed(t)
				c.League.Slug = "NFL" // uppercase: domain.NewSlug rejects rather than folds
				return delivery(t, c, 1)
			},
			wantOutcome: ApplyRejected,
			wantErr:     ErrRecordRejected,
		},
		{
			name:        "a nil delivery is rejected rather than panicking",
			build:       func(*testing.T) *kafka.Delivery { return nil },
			wantOutcome: ApplyRejected,
			wantErr:     ErrRecordRejected,
		},
		{
			name:        "a tombstone for a key we never held changes nothing",
			build:       func(*testing.T) *kafka.Delivery { return tombstone(sampleMarketID, sampleObservedAt) },
			wantOutcome: ApplyRemoved,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore(nil)
			res, err := s.Apply(tc.build(t))

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", res.Outcome, tc.wantOutcome)
			}
			if got := s.Len(); got != tc.wantLen {
				t.Errorf("slate holds %d markets, want %d", got, tc.wantLen)
			}
		})
	}
}

// TestStoreCarriesThePayloadThroughByteForByte. The client receives the
// price.computed document verbatim; a re-marshal here would be a second
// declaration of the pricer's schema, which is the drift
// internal/pricing/payload.go argues against.
func TestStoreCarriesThePayloadThroughByteForByte(t *testing.T) {
	s := NewStore(nil)
	d := delivery(t, sampleComputed(t), 1)
	want := string(d.Envelope.Data)

	res, err := s.Apply(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Market.Payload) != want {
		t.Fatalf("payload was re-encoded.\n got: %s\nwant: %s", res.Market.Payload, want)
	}
}

// TestStoreCopiesThePayloadOutOfTheFetchBuffer.
//
// internal/platform/kafka states the obligation: a handler "MUST NOT retain
// Delivery.Value() past its return: those bytes alias the fetch buffer".
// Envelope.Data is part of the same allocation. Retaining the alias would give
// every connected client a view of whatever the NEXT record decoded into that
// memory, which presents as one market rendering another market's prices — a
// defect with no error and no log line anywhere.
func TestStoreCopiesThePayloadOutOfTheFetchBuffer(t *testing.T) {
	s := NewStore(nil)
	d := delivery(t, sampleComputed(t), 1)
	before := string(d.Envelope.Data)

	if _, err := s.Apply(d); err != nil {
		t.Fatal(err)
	}

	// Simulate the fetch buffer being reused under the Delivery.
	for i := range d.Envelope.Data {
		d.Envelope.Data[i] = 'x'
	}

	held := s.Snapshot(mustChannel(t, "market:"+sampleMarketID))
	if len(held) != 1 {
		t.Fatalf("snapshot holds %d markets, want 1", len(held))
	}
	if string(held[0].Payload) != before {
		t.Fatalf("the stored payload aliased the delivery's buffer and was overwritten:\n%s",
			held[0].Payload)
	}
}

// TestStoreReplacesAndRemoves walks the full lifecycle of one key.
func TestStoreReplacesAndRemoves(t *testing.T) {
	s := NewStore(nil)
	market := mustChannel(t, "market:"+sampleMarketID)

	first := sampleComputed(t)
	if _, err := s.Apply(delivery(t, first, 1)); err != nil {
		t.Fatal(err)
	}

	second := sampleComputed(t)
	second.ObservedAt = sampleObservedAt.Add(time.Minute)
	second.SourceFingerprint = "moved"
	res, err := s.Apply(delivery(t, second, 2))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ApplyStored {
		t.Fatalf("outcome = %q, want stored", res.Outcome)
	}
	if s.Len() != 1 {
		t.Fatalf("a replacement added a key: slate holds %d", s.Len())
	}
	if got := s.Snapshot(market); len(got) != 1 || got[0].Computed.SourceFingerprint != "moved" {
		t.Fatalf("the replacement was not visible in the snapshot: %+v", got)
	}

	res, err = s.Apply(tombstone(sampleMarketID, second.ObservedAt.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ApplyRemoved {
		t.Fatalf("outcome = %q, want removed", res.Outcome)
	}
	// The deletion must name the channels the entry HAD. A tombstone carries no
	// payload, so nothing else could derive them, and without them the removal
	// could only be announced on market:{id} — leaving the market on every board
	// that had been showing it.
	if len(res.Channels) != 3 {
		t.Fatalf("removal announced on %d channels, want the 3 the entry held: %v", len(res.Channels), res.Channels)
	}
	if got := s.Snapshot(market); len(got) != 0 {
		t.Fatalf("the market survived its tombstone: %+v", got)
	}
	if st := s.Stats(); st.Markets != 0 || st.Events != 0 || st.Leagues != 0 {
		t.Fatalf("indexes were not torn down: %+v", st)
	}
}

// TestStoreRefusesToRegress is the monotonicity guard.
//
// This fold IS the snapshot every connecting client receives, so applying an
// older observation would publish a stale price as news and then serve it to
// every subsequent connection. A redelivery is normal under at-least-once, so
// this fires in ordinary operation rather than only under a fault.
func TestStoreRefusesToRegress(t *testing.T) {
	s := NewStore(nil)

	newer := sampleComputed(t)
	newer.ObservedAt = sampleObservedAt.Add(time.Minute)
	newer.SourceFingerprint = "newer"
	if _, err := s.Apply(delivery(t, newer, 1)); err != nil {
		t.Fatal(err)
	}

	older := sampleComputed(t)
	older.ObservedAt = sampleObservedAt
	older.SourceFingerprint = "older"
	res, err := s.Apply(delivery(t, older, 2))
	if err != nil {
		t.Fatalf("a stale record is not an error: %v", err)
	}
	if res.Outcome != ApplyStale {
		t.Fatalf("outcome = %q, want stale", res.Outcome)
	}

	held := s.Snapshot(mustChannel(t, "market:"+sampleMarketID))
	if len(held) != 1 || held[0].Computed.SourceFingerprint != "newer" {
		t.Fatalf("the older record overwrote the newer state: %+v", held)
	}

	// A redelivery of the record already held is idempotent: applied, not
	// refused. An equal instant is a real republication (the pricer reprices
	// when its configuration digest changes), and suppressing it would be the
	// worse error of the two.
	res, err = s.Apply(delivery(t, newer, 3))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ApplyStored {
		t.Fatalf("a redelivery at the same instant was %q, want stored", res.Outcome)
	}
	if s.Len() != 1 {
		t.Fatalf("a redelivery duplicated the key: slate holds %d", s.Len())
	}

	// The same guard on the deletion path.
	res, err = s.Apply(tombstone(sampleMarketID, sampleObservedAt.Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ApplyStale {
		t.Fatalf("an old tombstone was %q, want stale", res.Outcome)
	}
	if s.Len() != 1 {
		t.Fatal("an old tombstone deleted a market that had since moved on")
	}
}

// TestIndexTeardownOnReKey. A market whose event or league changed must leave
// the old index, or it keeps being delivered to a board it has left — and the
// stale entry is invisible to every other assertion.
func TestIndexTeardownOnReKey(t *testing.T) {
	s := NewStore(nil)
	if _, err := s.Apply(delivery(t, sampleComputed(t), 1)); err != nil {
		t.Fatal(err)
	}

	moved := sampleComputed(t)
	moved.ObservedAt = sampleObservedAt.Add(time.Minute)
	moved.League.Slug = "nba"
	moved.Event.ID = "evt-2"
	if _, err := s.Apply(delivery(t, moved, 2)); err != nil {
		t.Fatal(err)
	}

	if got := s.Snapshot(mustChannel(t, "league:"+sampleLeagueSlug)); len(got) != 0 {
		t.Errorf("the market is still on its old league channel: %+v", got)
	}
	if got := s.Snapshot(mustChannel(t, "event:"+sampleEventID)); len(got) != 0 {
		t.Errorf("the market is still on its old event channel: %+v", got)
	}
	if got := s.Snapshot(mustChannel(t, "league:nba")); len(got) != 1 {
		t.Errorf("the market is not on its new league channel: %+v", got)
	}
	if st := s.Stats(); st.Events != 1 || st.Leagues != 1 {
		t.Errorf("empty index buckets were not collected: %+v", st)
	}
}

// TestSnapshotOfAnEmptyChannelIsEmptyAndValid. An empty subscription that yields
// a correct EMPTY snapshot is the right answer; fabricating a placeholder market
// to make a screen look populated is a phase failure.
func TestSnapshotOfAnEmptyChannelIsEmptyAndValid(t *testing.T) {
	s := NewStore(nil)
	for _, name := range []string{"league:nfl", "event:evt-404", "market:mkt-404"} {
		got := s.Snapshot(mustChannel(t, name))
		if got == nil {
			t.Errorf("%s snapshot is nil; an empty channel must produce an empty slice", name)
		}
		if len(got) != 0 {
			t.Errorf("%s snapshot has %d markets on an empty store", name, len(got))
		}
	}
}

// TestSnapshotIsOrdered. Map iteration order is randomised in Go, so an
// unordered snapshot would put the same markets on the wire in a different order
// per connection — which makes a golden file impossible and an order-dependent
// rendering bug intermittent.
func TestSnapshotIsOrdered(t *testing.T) {
	s := NewStore(nil)
	for _, id := range []string{"mkt-c", "mkt-a", "mkt-d", "mkt-b"} {
		c := sampleComputed(t)
		c.Market.ID = id
		if _, err := s.Apply(delivery(t, c, 1)); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Snapshot(mustChannel(t, "league:"+sampleLeagueSlug))
	ids := make([]string, len(got))
	for i, m := range got {
		ids[i] = m.ID.String()
	}
	if strings.Join(ids, ",") != "mkt-a,mkt-b,mkt-c,mkt-d" {
		t.Fatalf("snapshot order = %v, want ascending by market id", ids)
	}
}

// TestSnapshotChannelsAgree. The same market must be reachable from all three of
// its channels, because ChannelsFor is the single definition and the indexes are
// built from it.
func TestSnapshotChannelsAgree(t *testing.T) {
	s := NewStore(nil)
	if _, err := s.Apply(delivery(t, sampleComputed(t), 1)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"market:" + sampleMarketID,
		"event:" + sampleEventID,
		"league:" + sampleLeagueSlug,
	} {
		got := s.Snapshot(mustChannel(t, name))
		if len(got) != 1 || got[0].ID.String() != sampleMarketID {
			t.Errorf("%s snapshot = %+v, want the one market", name, got)
		}
	}
}

// TestFoldPublishesOnlyWhatChanged. A record that changed nothing is not news,
// and announcing it would put a delta on the wire that every client must ignore.
func TestFoldPublishesOnlyWhatChanged(t *testing.T) {
	s := NewStore(nil)
	var published []ApplyOutcome
	collect := func(r ApplyResult) { published = append(published, r.Outcome) }

	if _, err := s.Fold(delivery(t, sampleComputed(t), 1), collect); err != nil {
		t.Fatal(err)
	}

	older := sampleComputed(t)
	older.ObservedAt = sampleObservedAt.Add(-time.Hour)
	if _, err := s.Fold(delivery(t, older, 2), collect); err != nil {
		t.Fatal(err)
	}
	// A tombstone against a key that is not held changes nothing either.
	if _, err := s.Fold(tombstone("mkt-404", sampleObservedAt), collect); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fold(tombstone(sampleMarketID, sampleObservedAt.Add(time.Hour)), collect); err != nil {
		t.Fatal(err)
	}

	want := []ApplyOutcome{ApplyStored, ApplyRemoved}
	if len(published) != len(want) {
		t.Fatalf("published %v, want %v", published, want)
	}
	for i := range want {
		if published[i] != want[i] {
			t.Errorf("published[%d] = %q, want %q", i, published[i], want[i])
		}
	}
}

// TestAttachIsAtomicAgainstFold is D2, asserted.
//
// The registration callback runs under the same write lock the publish path
// needs, so a Fold cannot interleave with it. If it could, a delta published in
// the window between "the snapshot was read" and "this connection is a
// subscriber" would be lost, and the market would simply stop moving on that
// client with nothing anywhere saying so.
//
// The witness is a Fold attempted from another goroutine while the register
// callback is running: it must not have completed by the time the callback
// returns.
func TestAttachIsAtomicAgainstFold(t *testing.T) {
	s := NewStore(nil)

	started := make(chan struct{})
	folded := make(chan struct{})
	var insideCallback bool
	var raced bool

	go func() {
		<-started
		if _, err := s.Fold(delivery(t, sampleComputed(t), 1), func(ApplyResult) {
			if insideCallback {
				raced = true
			}
		}); err != nil {
			t.Error(err)
		}
		close(folded)
	}()

	s.Attach([]Channel{mustChannel(t, "league:"+sampleLeagueSlug)}, func(snaps map[Channel][]Market) {
		insideCallback = true
		close(started)
		// Give the other goroutine every chance to get in.
		for i := 0; i < 1000; i++ {
			select {
			case <-folded:
				raced = true
			default:
			}
		}
		if len(snaps) != 1 {
			t.Errorf("register received %d snapshots, want one per requested channel", len(snaps))
		}
		insideCallback = false
	})

	<-folded
	if raced {
		t.Fatal("a Fold completed while a subscription was being registered; " +
			"the snapshot and the delta stream are not linearised")
	}
}

// TestAttachSnapshotsEveryRequestedChannel, including the ones with nothing on
// them: a client subscribing to an empty league must be told it is empty rather
// than left waiting.
func TestAttachSnapshotsEveryRequestedChannel(t *testing.T) {
	s := NewStore(nil)
	if _, err := s.Apply(delivery(t, sampleComputed(t), 1)); err != nil {
		t.Fatal(err)
	}

	full := mustChannel(t, "league:"+sampleLeagueSlug)
	empty := mustChannel(t, "league:nba")

	var got map[Channel][]Market
	s.Attach([]Channel{full, empty, {}}, func(snaps map[Channel][]Market) { got = snaps })

	if len(got) != 2 {
		t.Fatalf("register received %d entries, want 2 (the zero channel is skipped)", len(got))
	}
	if len(got[full]) != 1 {
		t.Errorf("the populated channel snapshotted %d markets, want 1", len(got[full]))
	}
	if v, ok := got[empty]; !ok || v == nil || len(v) != 0 {
		t.Errorf("the empty channel snapshotted %#v, want an empty non-nil slice", v)
	}
}

func TestExclusiveRunsUnderTheSameLock(t *testing.T) {
	s := NewStore(nil)
	ran := false
	s.Exclusive(func() { ran = true })
	if !ran {
		t.Fatal("Exclusive did not run the callback")
	}
	s.Exclusive(nil) // must not panic
}

// TestStoreIsSafeUnderConcurrentUse. Run with -race, which is how CI runs it
// (CLAUDE.md §9: `go test -race`). Every replica folds the bus on one goroutine
// while serving connections on thousands of others, so this is the store's
// normal operating condition rather than an edge case.
func TestStoreIsSafeUnderConcurrentUse(t *testing.T) {
	s := NewStore(nil)
	const markets = 32
	const readers = 8

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			league := mustChannel(t, "league:"+sampleLeagueSlug)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.Snapshot(league)
				s.Attach([]Channel{league}, func(map[Channel][]Market) {})
				_ = s.Stats()
				_ = s.MarketsTracked()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < markets; i++ {
			c := sampleComputed(t)
			c.Market.ID = fmt.Sprintf("mkt-%02d", i)
			if _, err := s.Fold(delivery(t, c, int64(i)), func(ApplyResult) {}); err != nil {
				t.Error(err)
				return
			}
		}
		for i := 0; i < markets; i += 2 {
			if _, err := s.Apply(tombstone(fmt.Sprintf("mkt-%02d", i), sampleObservedAt.Add(time.Hour))); err != nil {
				t.Error(err)
				return
			}
		}
		close(stop)
	}()

	wg.Wait()
	if got := s.Len(); got != markets/2 {
		t.Fatalf("slate holds %d markets, want %d", got, markets/2)
	}
}
