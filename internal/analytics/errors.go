// This package's sentinel errors.
//
// They are declared in one file rather than beside their first use for the
// reason internal/pricing/errors.go gives: a caller classifies with errors.Is
// against a NAME, never against message text, and a reader working out which
// failures are distinguishable should be able to see the whole vocabulary at
// once rather than grep for `errors.New`.
package analytics

import "errors"

var (
	// ErrInvalidOptions reports a configuration this package cannot run under.
	// CLAUDE.md §12: "fail fast and loudly on a bad config".
	ErrInvalidOptions = errors.New("analytics: invalid options")

	// ErrInvalidConfig reports a detector threshold set that cannot mean what it
	// says — a negative magnitude bound, a hop longer than its window, a
	// correlation requirement outside [-1, 1].
	//
	// It is separate from ErrInvalidOptions because the two are different kinds
	// of mistake: ErrInvalidOptions is a wiring error (no store, no logger) and
	// this is a JUDGEMENT error, a threshold nobody meant to set. They are
	// reported at the same moment — construction — but a reader of the log needs
	// to know which of the two it is looking at.
	ErrInvalidConfig = errors.New("analytics: invalid detector configuration")

	// ErrUnsupportedMessage reports a record on price.computed whose envelope
	// type this build does not read. Counted rather than ignored: it means
	// something this build does not understand is writing the topic.
	ErrUnsupportedMessage = errors.New("analytics: unsupported message type")

	// ErrNotRunning is what the readiness checker reports before Run has started
	// the consumer and after it has stopped. A replica whose consumer has exited
	// but whose listener is still up would otherwise look healthy while
	// detecting nothing.
	ErrNotRunning = errors.New("analytics: service is not running")

	// ErrNotRunnable is returned by Run when it was given no consumer.
	ErrNotRunnable = errors.New("analytics: no consumer")

	// ErrNoStore reports an attempted persistence with no store wired. It exists
	// so that "persistence is off" is a legible condition rather than a silent
	// one; see [ServiceOptions.Store] for why the dependency is optional at all.
	ErrNoStore = errors.New("analytics: no store is wired, so nothing is persisted")

	// ErrNoPublisher is ErrNoStore for the bus half.
	ErrNoPublisher = errors.New("analytics: no publisher is wired, so nothing reaches the signals topics")

	// ErrContended reports a store write that lost a lock-ordering race and was
	// rolled back WITHOUT WRITING ANYTHING. It is the one store failure this
	// package retries on its own.
	//
	// # Why it exists, and why it is a sentinel rather than a SQLSTATE
	//
	// A [Store] adapter is the only layer that can recognise the condition —
	// Postgres reports it as 40P01 deadlock_detected or 40001
	// serialization_failure — and this package must not learn a driver's
	// vocabulary to react to it. The adapter classifies; the consumer decides. That
	// split is the same one CLAUDE.md §12 asks for when it says interfaces are
	// declared by the consumer.
	//
	// # Why retrying it is safe here and is not safe in general
	//
	// internal/platform/postgres deliberately ships no retry helper, because a
	// caller's function may have produced to Kafka or mutated state between the
	// failing statement and the return. Every [Store] method is a single upsert (or
	// one transaction of upserts) on a replay key derived from the input alone,
	// and the port promises idempotence; a re-run therefore writes exactly what the
	// first attempt would have. The publish half happens strictly after the persist
	// half returns, so a retried write cannot double-publish either.
	//
	// # What made it necessary
	//
	// Phase 9 put a second writer on the catalogue's foreign-key parents. `ingest`
	// upserts leagues, books, markets and selections in FK order and ON CONFLICT DO
	// UPDATE takes an exclusive row lock on every conflicting row whether or not the
	// update is then declined; a signals insert takes FOR KEY SHARE on those same
	// rows in referential-integrity-trigger order. Two orders, two processes, one
	// deadlock — and before phase 9 nothing else wrote those parents, so none was
	// reachable.
	ErrContended = errors.New("analytics: the store write lost a lock-ordering race and wrote nothing")

	// ErrCatalogueLag reports a store write refused because a CATALOGUE PARENT
	// this finding references — its league, its book, its market or its selection
	// — is not in the database yet. Postgres reports it as 23503
	// foreign_key_violation.
	//
	// # It is NOT a disagreement between a detector and the schema, and must not
	// # be reported as one
	//
	// internal/analytics validates every finding against migrations/00009's CHECK
	// constraints before it reaches a [Store], which is what makes a 23514 a real
	// drift between the two. It cannot do the same for a FOREIGN KEY, because it
	// has no view of the catalogue at all: a finding is derived from one
	// price.computed record and nothing in that record says whether `ingest` has
	// committed the market row yet.
	//
	// # Why the gap exists, and why it is transient
	//
	// The catalogue is written by a DIFFERENT SERVICE. `ingest` hosts the
	// Timescale writer, which consumes odds.normalized and upserts leagues, books,
	// markets and selections; `pricer` consumes the same topic, prices it, and
	// publishes price.computed, which this stage then consumes. Nothing orders
	// those two consumers against each other, so on a cold start — and for a
	// genuinely new event on a running one — a market can be priced, published and
	// read here in the moment before its catalogue row commits. It is at its worst
	// on a first `docker compose up`, where a fresh consumer group replays the
	// whole compacted topic against an empty catalogue.
	//
	// # What recovers the finding
	//
	// Not redelivery: the pricer wires this stage with kafka.ErrorPolicySkip, so a
	// returned error advances past the record rather than replaying it. What
	// recovers it is the market's NEXT price change, which republishes
	// price.computed for that key and re-derives the same finding against a
	// catalogue that has since landed. Because the write is an upsert on an
	// input-derived replay key, the recovered row is the row the first attempt
	// would have written.
	ErrCatalogueLag = errors.New("analytics: a catalogue row this finding references is not in the database yet")
)
