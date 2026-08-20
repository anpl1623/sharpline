// The seams this package needs from the outside world.
//
// CLAUDE.md §12: "Interfaces are declared by the consumer, not the producer.
// Keep them small." Every interface below is declared here, in the package that
// CALLS it, and is deliberately narrower than the type that satisfies it — the
// store is three writes, not a repository; the publisher is three publishes, not
// a producer. A wider interface would let this package reach for a capability it
// has no business having, and would make it impossible to satisfy in a test
// without a database.
package analytics

import (
	"context"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// Clock is this package's only source of "now".
//
// It exists for one field on each signal — DetectedAt — and for the detector
// latency histogram, and for NOTHING ELSE. Every other instant on a finding is
// propagated from the provider observation that produced it, and doc.go explains
// why that is a requirement rather than a habit: a replay key that contained a
// clock reading of ours would make a replay write a NEW row instead of
// correcting the old one, and the phase-12 comparison would be against a table
// that grew a duplicate every time the job ran.
//
// It is injected rather than read from time.Now directly because CLAUDE.md §12
// forbids reaching for a global, and because a test that wants to assert an
// exact DetectedAt needs it.
type Clock func() time.Time

// Store persists findings.
//
// # "Not found" does not appear here, on purpose
//
// Every method is a WRITE, and all three are upserts on a replay key derived
// from the input alone. There is no read on this port and there is no
// ErrNotFound: this package never asks the database what it already knows, and a
// read seam would invite a check-then-write race that the ON CONFLICT DO UPDATE
// in migrations/00009 exists to make unnecessary. The QUERY surface over these
// tables belongs to `api` and is a different port in a different package.
//
// A method returns an error only for a failure the CALLER can act on — a dead
// connection, a rejected constraint. There is no "already exists" outcome to
// report, because an upsert has none.
//
// # Every method is idempotent, and that is load-bearing
//
// Calling any of them twice with the same argument leaves the database in the
// same state as calling it once. The service publishes to the bus AFTER
// persisting, so a publish failure returns an error, the record's offset stays
// uncommitted, and the whole record is redelivered — which re-runs the detectors
// and re-upserts identical rows. Without idempotence that retry would duplicate
// every finding on the record.
//
// # RecordArbitrageSignal writes its legs in the SAME transaction
//
// A finding and its outcome set are one fact. migrations/00009 cascades the
// delete from parent to legs and the recomputation path is delete-then-reinsert,
// so a caller that wrote the parent and failed before the legs would leave a
// finding with no outcomes — which a reader cannot distinguish from a finding
// whose legs have not been written yet. The implementation therefore runs
// UpsertArbitrageSignal, DeleteArbitrageSignalLegs and one
// UpsertArbitrageSignalLeg per leg inside one postgres.InTx, and the port
// promises that.
// # One failure an implementation MUST classify: lock contention
//
// A write that Postgres rolled back because it lost a lock-ordering race must be
// returned wrapped in [ErrContended], and only that failure may be. The service
// re-runs such a write in place rather than paying for a whole redelivery, and
// that is safe precisely because of the idempotence promised above. An adapter
// that reported a deadlock as an ordinary error would still be correct — the
// record is redelivered — but it would re-run every detector on the record and
// re-emit its other findings to do it. See [ErrContended] for the mechanism that
// makes the condition reachable at all.
type Store interface {
	RecordEVSignal(ctx context.Context, sig EVSignal) error
	RecordArbitrageSignal(ctx context.Context, sig ArbitrageSignal) error
	RecordSteamSignal(ctx context.Context, sig SteamSignal) error
}

// Publisher writes the three signals topics.
//
// # Keyed by MARKET, all three
//
// Every method takes a [domain.MarketID] even though a +EV finding is about a
// selection and a steam finding is about a (selection, window). topics.go argues
// it: odds.normalized and price.computed are keyed by market with the same
// partition count, so a market-keyed signals topic lands the same market on the
// same partition INDEX across all three, and phase 12's Flink joins are local
// rather than a network shuffle. Keying finer would buy a granularity nothing
// consumes and would cost the co-partitioning.
//
// # Synchronous, and no async variant will be added
//
// A finding that was accepted by the client and never reached the broker is a
// loss nobody can detect: unlike a price, a signal is not restated on the next
// record, and there is no compacted snapshot in which the gap would show. The
// service returns an error on a failed publish, which the consumer answers
// according to its ErrorPolicy — a redelivery under Stop, an advance under the
// Skip that `pricer` wires. Neither loses the finding permanently: the row is
// already in Postgres, because the order is persist-then-publish, and the record
// is re-emitted when the market next reprices.
//
// # Flush and Close are deliberately absent
//
// This package does not own the producer's lifetime and must not be able to end
// it, exactly as internal/pricing's Publisher omits Close for the same reason.
// The composition root closes the producer, which flushes, and internal/pricing's
// service performs an explicit pre-close flush on the same instance.
//
// # The concrete implementation
//
// *kafka.OddsProducer satisfies this port through three named methods beside
// PublishPrice. They are named rather than generic because that producer's
// publish path is unexported and typed per topic on purpose, which is what makes
// `PublishNormalized(ctx, someEventID, msg)` fail to compile rather than key a
// record wrongly; there is no way to reach a signals topic without naming it.
//
// A nil Publisher remains supported and LOUD rather than silent — see
// [ServiceOptions.Publisher] — because a test or a one-shot tool may want the
// detectors without a broker.
type Publisher interface {
	PublishEVSignal(ctx context.Context, id domain.MarketID, msg kafka.Message) error
	PublishArbitrageSignal(ctx context.Context, id domain.MarketID, msg kafka.Message) error
	PublishSteamSignal(ctx context.Context, id domain.MarketID, msg kafka.Message) error
}

// Consumer is the part of *kafka.Consumer that [Service.Run] drives.
//
// The Consumer owns the poll loop, the commit boundary and the group lifecycle,
// and this package reimplements none of them: it hands over a handler and waits
// for Run to return, which it does only after committing what it handled.
//
// The Consumer handed in MUST be built with DisableLagExport left false. The
// dashboard's bus-lag panels are fed by its background refresher, and a signals
// stage whose lag is invisible is one that can fall an hour behind the priced
// board while every panel reads healthy.
type Consumer interface {
	Run(ctx context.Context, h kafka.Handler) error
}

// Compile-time proof that the shipped implementations satisfy the declarations
// above. They are here rather than at the call site because a mismatch should
// break THIS package's build, where the interfaces are declared — a port that has
// drifted from its only implementation is a defect in the port.
//
// [Store] is deliberately absent and asserted in internal/analytics/pgstore
// instead: that package imports this one, so the dependency only runs one way.
var (
	_ Consumer  = (*kafka.Consumer)(nil)
	_ Publisher = (*kafka.OddsProducer)(nil)
)
