// Where change-detection state lives.
//
// # The state must survive a restart
//
// If it does not, the first poll after every deploy republishes the entire
// board: every market's fingerprint is unknown, so every market looks changed.
// On a full slate that is thousands of records, a hypertable write per quote,
// and a pricing pass for all of it — triggered by a rolling restart that changed
// nothing about the odds.
//
// It is warmed from odds.normalized itself. CLAUDE.md §3 chose Kafka over NATS
// partly on this: "a compacted topic keyed by market_id IS the current-line
// snapshot, replayable from scratch, WHICH REMOVES A WHOLE CLASS OF
// CACHE-COHERENCY BUGS BETWEEN THE BUS AND REDIS." Putting the authoritative
// fingerprint in Redis reintroduces exactly that class, and the failure is
// concrete: Redis remembers fingerprint F for market M, the record for M is gone
// from the log (a recreated topic, a truncated partition, a produce that failed
// after the fingerprint was written), and M is suppressed FOR EVER — invisible
// to every client that builds its snapshot from the log.
//
// The warm-start path cannot have that bug, because the thing it reads is the
// thing clients read.
//
// # Redis is still the right home for a SHARED cache later
//
// Once the normalizer runs as more than one replica, a group rebalance moves
// partitions between members and a per-process store makes the new owner
// republish the markets it inherited. That is bounded, self-healing and safe, so
// it is not urgent. FingerprintStore is the seam — declared HERE, by the
// consumer, per CLAUDE.md §12 — and MemoryStore is the implementation this phase
// ships. A Redis-backed one drops in behind it without touching normalizer.go,
// and should STILL be warmed from the topic rather than trusted over it.
package normalizer

import (
	"context"
	"sync"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Entry is what change detection remembers about one market.
//
// It is deliberately not the whole record. Retaining every published
// NormalizedMarket would make this a second copy of the compacted topic — a few
// megabytes on a full slate — and buy nothing this package needs, because every
// decision below is a comparison against these three values.
type Entry struct {
	// Fingerprint is the hash of the last record published for this market.
	Fingerprint Fingerprint

	// ObservedAt is that record's newest provider observation instant. It is the
	// monotonicity guard: a payload observed strictly before this one would
	// regress the compacted snapshot if it were published.
	ObservedAt time.Time

	// PublishedAt is when the record was written to the bus — the envelope's
	// ProducedAt, so a warm-started entry carries the ORIGINAL instant rather
	// than the moment this process read it. That matters: using the read instant
	// would restart the refresh ceiling on every deploy, and the ceiling exists
	// precisely to bound how long a defect in the fingerprint can persist.
	PublishedAt time.Time
}

// IsZero reports whether the entry is unset.
func (e Entry) IsZero() bool {
	return e.Fingerprint.IsZero() && e.ObservedAt.IsZero() && e.PublishedAt.IsZero()
}

// FingerprintStore is the change-detection state.
//
// context.Context is first on every method even though MemoryStore never blocks,
// because the seam exists for a Redis-backed implementation and adding a context
// later would change every call site. CLAUDE.md §12 puts a context on "anything
// doing I/O"; this interface is the declaration that an implementation MAY.
type FingerprintStore interface {
	// Load returns the entry for a market and whether one is held.
	Load(ctx context.Context, id domain.MarketID) (Entry, bool, error)

	// Store records the entry for a market, replacing any previous one.
	Store(ctx context.Context, id domain.MarketID, e Entry) error

	// Delete forgets a market. Warm start calls it for every tombstone on the
	// compacted topic: a deleted market must not stay suppressed, because
	// nothing else will ever republish it.
	Delete(ctx context.Context, id domain.MarketID) error

	// Len reports how many markets are held. It is the warm-start gauge and the
	// number an operator compares against the market count on the board.
	Len(ctx context.Context) (int, error)
}

// MemoryStore is the in-process FingerprintStore.
//
// Safe for concurrent use. The consumer drives one handler at a time today, but
// the readiness probe and any future parallel-partition handler both touch it,
// and a map without a lock is a data race that appears under load rather than
// under test.
type MemoryStore struct {
	mu      sync.RWMutex
	entries map[domain.MarketID]Entry
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: make(map[domain.MarketID]Entry)}
}

// Load implements FingerprintStore.
func (s *MemoryStore) Load(_ context.Context, id domain.MarketID) (Entry, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[id]
	return e, ok, nil
}

// Store implements FingerprintStore.
func (s *MemoryStore) Store(_ context.Context, id domain.MarketID, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[id] = e
	return nil
}

// Delete implements FingerprintStore.
func (s *MemoryStore) Delete(_ context.Context, id domain.MarketID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, id)
	return nil
}

// Len implements FingerprintStore.
func (s *MemoryStore) Len(_ context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries), nil
}

var _ FingerprintStore = (*MemoryStore)(nil)
