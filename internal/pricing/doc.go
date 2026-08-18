// Package pricing is the pricer service: it consumes the compacted
// odds.normalized topic, applies the phase-1 odds mathematics to every market
// that moves, and publishes the result to the compacted price.computed topic.
//
// CLAUDE.md §3 places it in the middle of the event flow
//
//	[odds.normalized] ──▶ pricer ──▶ [price.computed] ──▶ stream ──▶ browser
//
// and gives it one responsibility: "Devig, no-vig fair value, EV%, Kelly
// sizing, arbitrage + middles detection, parlay correlation."
//
// # What is here and what is deliberately not
//
// This file, service.go and metrics.go are the SERVICE: the consumer loop, the
// change-detection and monotonicity guards, the publication, the warm start,
// the readiness contract and the metric contract. THE MATHEMATICS IS NOT HERE.
// It is a [PriceFunc] injected at construction, and the arithmetic underneath it
// belongs to internal/domain/odds, which phase 1 shipped as pure functions over
// value types with property-based tests. Nothing in this package re-derives a
// devig, a fair price, an expected value or a Kelly fraction; a second
// implementation of odds mathematics is the one defect this project cannot
// absorb, because there would then be two answers and no way to tell which is
// wrong.
//
// The rest of the package is the ENGINE, and its arguments live in the files
// that make them rather than being restated here:
//
//	state.go      decoding one record into a market, and why the staleness anchor
//	              is the record's own instant rather than a clock
//	reference.go  choosing the sharp book, why it is not a consensus, and how a
//	              catalogue designation outranks the configured preference list
//	fairvalue.go  the devig, its single explicit fallback, and the cross-method
//	              disagreement carried as the fair value's error bar
//	ev.go         scoring every book against it, and why a quote at a moved line
//	              is carried but never scored
//	signals.go    the arbitrage and middles scanners, wired onto the published
//	              record — CLAUDE.md §3 puts both on this service, and they run
//	              over the market the pricing pass has already decoded
//	engine.go     the order of operations and the configuration digest that makes
//	              a settings change reprice the slate instead of suppressing it
//	payload.go    the price.computed document
//
// # The seam, and why it is a function rather than an interface
//
// [PriceFunc] takes a decoded [normalizer.NormalizedMarket] and returns the
// value to publish. It is a function type, not an interface, for a reason that
// is entirely practical: Go has no covariance, so an interface declaring
// `Price(...) (any, error)` would be satisfied by NOTHING that returned a
// concrete payload type, and every engine would need a hand-written adapter
// anyway. A function type lets the adapter be one line at the composition root
//
//	price := func(ctx context.Context, rec normalizer.NormalizedMarket) (any, error) {
//	    return engine.Price(ctx, rec)
//	}
//
// for an engine whose Price returns any concrete type, because Go's rules for
// `return f()` are assignability rules and every type is assignable to `any`.
//
// # The engine must be a pure function of the record
//
// This is a REQUIREMENT, not an observation, and the suppression below depends
// on it. Two calls with the same [normalizer.NormalizedMarket] must produce the
// same output. An engine that read the wall clock — weighting by time to kickoff,
// say — would break it: this service would suppress a republication whose result
// had in fact changed, and the compacted snapshot would hold an answer nothing
// would ever correct. If a future engine genuinely needs time as an input, the
// input has to come off the record (the event's scheduled start is on it) rather
// than off the clock.
//
// # Change detection without knowing what the engine produced
//
// The pricer suppresses a market whose INPUT has not changed since the last
// time this system priced it, and it decides that WITHOUT DECODING ITS OWN
// PAYLOAD. The state comes from the envelope:
//
//	kafka.Message.ID         ← the source record's normalizer fingerprint
//	kafka.Message.ObservedAt ← the source record's observation instant, unchanged
//
// so warm start reads price.computed with [kafka.Snapshotter] and folds
// Envelope.ID and Envelope.ObservedAt per key into the tracker. Two consequences
// follow, and both are the point:
//
//   - THIS PACKAGE IS INDEPENDENT OF THE ENGINE'S PAYLOAD SHAPE. The engine can
//     change what it computes, and add or remove fields, without touching the
//     service, the warm start or the suppression. A payload-decoding warm start
//     would couple them permanently and would break the day the payload's schema
//     version was bumped — exactly when the pipeline can least afford it.
//   - THE STATE IS RECONSTRUCTIBLE FROM THE COMPACTED TOPIC ALONE. CLAUDE.md §9
//     puts an HPA on this service, so replicas come and go; a replica that held
//     state no other replica could rebuild would make horizontal scaling a
//     correctness problem rather than a capacity one. Everything this package
//     remembers is a fold of price.computed, which any replica can read.
//
// # The four outcomes of one market
//
//	published    the source fingerprint differs (or is unknown) — repriced and published
//	suppressed   the source fingerprint is identical — the input did not move
//	stale        observed strictly before the state already published; skipped
//	tombstoned   the market was deleted upstream, so its price is deleted too
//
// `stale` is the monotonicity guard, and it is the same argument the normalizer
// makes for the same reason: price.computed is COMPACTED, so publishing an older
// observation makes the newest record for that key the OLDER state, and every
// consumer that builds its snapshot from the log then serves it. Kafka orders
// per key and both topics are keyed by market, so this fires only on a
// redelivery or on two replicas racing across a rebalance — both of which happen.
//
// `tombstoned` is not optional. odds.normalized is compacted, and a tombstone
// there means the market is gone for ever. Ignoring it would leave a priced
// market in price.computed's snapshot permanently, because no further record for
// that key is coming and compaction will never collapse it away. So a tombstone
// in propagates a tombstone out.
//
// # Publishing is SYNCHRONOUS, and that is load-bearing
//
// internal/platform/kafka's Consumer commits the last SUCCESSFULLY HANDLED
// record per partition, so returning nil from the handler is a claim that the
// record is durably handled. A handler that returned before its produce was
// acknowledged would let the odds.normalized offset commit ahead of a
// price.computed record that never reached the broker, and the market would
// simply be missing from the priced snapshot after the next restart, with
// nothing anywhere reporting a loss. [kafka.OddsProducer.PublishPrice] waits;
// PublishPriceAsync does not, and is not used here.
//
// # Why the warm start happens at startup and not on the first record
//
// It is a network read of a whole compacted topic, bounded by
// [kafka.DefaultSnapshotTimeout] — up to a minute. Paid for inside a handler it
// is paid for while holding up the consumer's poll loop, and
// kgo.BlockRebalanceOnPoll means the group's rebalance is blocked for exactly
// that long: one replica's lazy warm start stalls every other member's join. The
// normalizer learned this in phase 3b. So [Service.Warm] takes a context and the
// composition root calls it before the consumer exists.
//
// A failed warm start is NOT fatal. The service prices COLD and says so at
// ERROR: every market on the slate is repriced and republished once, which is
// bounded, self-healing and harmless on a compacted last-write-wins topic.
// Refusing to start instead would freeze the priced board for as long as the
// broker is unhappy, which is the worse failure.
//
// # Completeness of the priced snapshot
//
// A fresh consumer group has no committed offsets and
// [kafka.ConsumerOptions.StartAtEnd] is left false, so the first run replays
// odds.normalized from the beginning — which on a compacted topic IS the current
// slate. That replay, not the warm start, is what makes price.computed complete:
// every market that exists gets priced once, and thereafter only movement does.
// The warm start's job is the opposite one — to stop that replay from
// republishing what an earlier run already priced from identical input.
//
// # Shutdown order
//
// The order is phase 3b's and every step is load-bearing:
//
//  1. ctx cancellation stops the consumer polling for new records;
//  2. Consumer.Run drains the record already in the handler, then commits the
//     offsets of everything it handled — including that one;
//  3. only once Run has returned is the producer FLUSHED, on a context detached
//     from the cancelled one, because a flush that races a live handler flushes a
//     buffer still being filled and a flush on a cancelled context returns
//     instantly with the buffer intact.
//
// The producer is flushed but not CLOSED here: this package does not own its
// lifetime and must not be able to end it while something else still holds it.
// The composition root closes it, and kgo.Client.Close fails every still-buffered
// record, which is why the explicit flush comes first rather than being left to
// the deferred close.
//
// # Metrics are a contract
//
// deploy/observability/grafana/dashboards/sharpline-overview.json has a "Pricing
// latency" panel and deploy/observability/rules/sharpline-alerts.yml has a
// PricingLatencyHigh alert, both written against
// `sharpline_pricing_duration_seconds` with a REQUIRED bucket boundary at exactly
// 0.25. metrics.go emits that name with those boundaries. This package also
// contributes the stage="priced" slice of the shared
// `sharpline_odds_staleness_seconds` and `sharpline_pipeline_latency_seconds`
// histograms rather than opening a parallel series, because the dashboard's
// per-stage breakdown exists precisely so a regression is attributable to one
// segment of received → normalized → priced → fanout.
//
// # What this package does NOT talk to
//
// No database and no cache. Its input is a Kafka topic, its output is a Kafka
// topic, and its state is a fold of the latter. config.Pricer currently declares
// RequireRedis, which under the phase-2 rule ("RequirePostgres in config means
// the binary MUST open a pool") would oblige this service to open Redis and
// probe it — but there is no internal/platform/redis in the tree at all, so no
// service opens one, and inventing a client here to satisfy a declaration would
// be the phase-2 defect wearing a different hat. The readiness probe therefore
// reports what this process actually depends on, the bus and its own consumer
// loop, and the declaration is flagged for reconciliation rather than faked.
package pricing
