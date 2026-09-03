package results

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/ingest/provider"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// In-memory doubles for the two seams the poller depends on.
//
// These are FAKES, NOT MOCKS: [fakeStore] holds real rows and applies the same
// guards the statement applies, so a test asserts that the poller behaves
// correctly against a store that behaves like the database rather than that a
// method was called. The guards are the interesting part — "a zero row count is
// not an error" is the whole contract of results.Store — and a mock returning a
// canned bool would prove nothing about it.
//
// They exist only in _test.go. Nothing in the shipped poller has a fallback that
// would serve invented results at run time.

// testNow is the pinned clock. Every test that asserts on a horizon or a
// settlement lag compares against it, so no test depends on wall time.
var testNow = time.Date(2026, 8, 20, 18, 0, 0, 0, time.UTC)

func fixedClock() func() time.Time { return func() time.Time { return testNow } }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// -----------------------------------------------------------------------------
// Store
// -----------------------------------------------------------------------------

// fakeStore is results.Store over a map, applying the two guards
// queries/results.sql applies: it refuses a source status that is already
// terminal, and it refuses an observation older than the one stored.
type fakeStore struct {
	mu sync.Mutex

	// pending is what the work queue returns, in the order given.
	pending []PendingEvent

	// rows is every event this deployment has ingested, with the observed_at
	// currently stored on it. A result for an event absent from this map is one
	// the UPDATE finds no row for, which is a zero row count and not an error.
	rows map[domain.EventID]time.Time

	// recorded is the terminal state per event, so a test can assert what
	// actually landed rather than only how many writes were attempted.
	recorded map[domain.EventID]provider.FinalResult

	// queries records every (horizon, limit) pair the poller asked for.
	queries []storeQuery

	// listErr, when set, fails the work-queue read.
	listErr error

	// writeErrs fails RecordResult for the named events, so a test can make one
	// contest in a batch fail without failing the rest.
	writeErrs map[domain.EventID]error
}

type storeQuery struct {
	FinishedBefore time.Time
	Limit          int
}

// newFakeStore seeds the table from the work queue: every pending contest is a
// row that exists, carrying the observed_at the queue reports for it.
func newFakeStore(pending ...PendingEvent) *fakeStore {
	s := &fakeStore{
		pending:   pending,
		rows:      map[domain.EventID]time.Time{},
		recorded:  map[domain.EventID]provider.FinalResult{},
		writeErrs: map[domain.EventID]error{},
	}
	for _, e := range pending {
		s.rows[e.EventID] = e.ObservedAt
	}
	return s
}

func (s *fakeStore) EventsAwaitingResult(
	_ context.Context, finishedBefore time.Time, limit int,
) ([]PendingEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, storeQuery{FinishedBefore: finishedBefore, Limit: limit})
	if s.listErr != nil {
		return nil, s.listErr
	}
	if limit < len(s.pending) {
		return append([]PendingEvent(nil), s.pending[:limit]...), nil
	}
	return append([]PendingEvent(nil), s.pending...), nil
}

// RecordResult applies both of the statement's guards and returns a row count,
// not a success flag. A false with a nil error is the steady state the whole
// design rests on; a fake that reported it as an error would let the poller pass
// a test the database would fail it on.
func (s *fakeStore) RecordResult(_ context.Context, id domain.EventID, r provider.FinalResult) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeErrs[id]; err != nil {
		return false, err
	}

	// `status NOT IN ('ended','settled','cancelled')`. It is the stronger guard
	// and it fires first: a row already in the results feed matches nothing
	// before any instant is compared, which is what makes a replay free.
	if _, done := s.recorded[id]; done {
		return false, nil
	}

	// `observed_at <= @observed_at`. The stored instant for a row still on the
	// queue is the last time the odds path saw it alive, so a result finalised
	// BEFORE that is an out-of-order observation and is declined.
	if row, known := s.rows[id]; known && row.After(r.FinalisedAt) {
		return false, nil
	}

	// No such event: this deployment never ingested the contest. Zero rows, and
	// not an error — the statement is an UPDATE and can never create one.
	if _, known := s.rows[id]; !known {
		return false, nil
	}

	s.recorded[id] = r
	return true, nil
}

func (s *fakeStore) result(id domain.EventID) (provider.FinalResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recorded[id]
	return r, ok
}

func (s *fakeStore) lastQuery(t *testing.T) storeQuery {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queries) == 0 {
		t.Fatal("the poller never read the work queue")
	}
	return s.queries[len(s.queries)-1]
}

// -----------------------------------------------------------------------------
// Provider
// -----------------------------------------------------------------------------

// fakeProvider is provider.ResultsProvider over a fixed answer.
//
// The answer is a function of the window rather than a fixed slice, because half
// the poller's contract is about what it does with outcomes nothing was waiting
// on — the unsolicited and duplicate cases — and because the window is itself
// something the poller derives and a test needs to inspect.
//
// Every key it states is a PROVIDER key, never a domain identifier. That is not
// decoration: a fake that answered with domain identifiers would let the poller
// pass while skipping the derivation that the whole seam exists to perform.
type fakeProvider struct {
	mu sync.Mutex

	name   provider.Name
	answer func(provider.ResultWindow) ([]provider.FinalResult, error)

	asked []provider.ResultWindow
}

func newFakeProvider(answer func(provider.ResultWindow) ([]provider.FinalResult, error)) *fakeProvider {
	return &fakeProvider{name: provider.NameSynthetic, answer: answer}
}

func (p *fakeProvider) Name() provider.Name { return p.name }

func (p *fakeProvider) Results(
	_ context.Context, window provider.ResultWindow,
) ([]provider.FinalResult, error) {
	p.mu.Lock()
	p.asked = append(p.asked, window)
	answer := p.answer
	p.mu.Unlock()
	if answer == nil {
		return nil, nil
	}
	return answer(window)
}

func (p *fakeProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.asked)
}

// lastWindow is the span the poller last asked about.
func (p *fakeProvider) lastWindow(t *testing.T) provider.ResultWindow {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.asked) == 0 {
		t.Fatal("the poller never asked the provider anything")
	}
	return p.asked[len(p.asked)-1]
}

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

// The two identifier spaces, in the two directions a test needs them.
//
// A PROVIDER KEY is what an adapter states: `syn-nba-1`. A DOMAIN IDENTIFIER is
// what the database holds, derived from that key by internal/ingest/normalizer
// when the catalogue row was written: `synthetic.e.syn-nba-1`. Every fixture
// below takes the provider key and derives the identifier, rather than spelling
// a domain identifier by hand, so that no test in this package can compare the
// two spaces against each other and pass.
//
// The spelling matters because the opposite arrangement is what stopped this
// system settling anything: the poller held domain identifiers, the adapter
// compared them against its own, and both sides of every test agreed with
// themselves.

// providerFor is the results source's name in the form the derivation takes.
func providerFor(t *testing.T, name provider.Name) kafka.Provider {
	t.Helper()
	p, err := kafka.NewProvider(name.String())
	if err != nil {
		t.Fatalf("kafka.NewProvider(%q): %v", name, err)
	}
	return p
}

// eventIDFor is the identifier the database holds for one provider event key.
func eventIDFor(t *testing.T, key string) domain.EventID {
	t.Helper()
	id, err := normalizer.EventIDFor(providerFor(t, provider.NameSynthetic), key)
	if err != nil {
		t.Fatalf("EventIDFor(%q): %v", key, err)
	}
	return id
}

// leagueIDFor is the same derivation for a league key.
func leagueIDFor(t *testing.T, key string) domain.LeagueID {
	t.Helper()
	id, err := normalizer.LeagueIDFor(providerFor(t, provider.NameSynthetic), key)
	if err != nil {
		t.Fatalf("LeagueIDFor(%q): %v", key, err)
	}
	return id
}

func mustScore(t *testing.T, home, away int) domain.Score {
	t.Helper()
	s, err := domain.NewScore(home, away)
	if err != nil {
		t.Fatalf("NewScore(%d, %d): %v", home, away, err)
	}
	return s
}

// pendingEvent builds one work-queue row that started `ago` before the pinned
// clock, for the contest the provider calls `key`.
//
// The row carries the DOMAIN identifier, because that is what the database holds
// and what EventsAwaitingResult returns.
func pendingEvent(t *testing.T, key string, ago time.Duration) PendingEvent {
	t.Helper()
	start := testNow.Add(-ago)
	return PendingEvent{
		EventID:        eventIDFor(t, key),
		League:         leagueIDFor(t, "syn-nba"),
		Kind:           domain.EventKindMatch,
		Name:           "Home vs Away",
		Status:         domain.EventStatusLive,
		ScheduledStart: start,
		ObservedAt:     start.Add(time.Minute),
	}
}

// endedResult is the ordinary answer: a contest played to a final score, named
// the way the PROVIDER names it.
func endedResult(t *testing.T, key string, finalisedAt time.Time) provider.FinalResult {
	t.Helper()
	return provider.FinalResult{
		EventKey:    key,
		Status:      domain.EventStatusEnded,
		Score:       mustScore(t, 104, 99),
		HasScore:    true,
		FinalisedAt: finalisedAt,
	}
}

// newPoller builds a poller with the pinned clock and unregistered collectors.
func newPoller(t *testing.T, store Store, src provider.ResultsProvider, cfg Config) *Poller {
	t.Helper()
	cfg.Now = fixedClock()
	p, err := New(Options{
		Config:   cfg,
		Provider: src,
		Store:    store,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}
