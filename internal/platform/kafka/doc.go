// Package kafka is the event-bus layer every Sharpline service shares: a
// franz-go producer with an explicit durability posture, a consumer-group
// consumer with explicit offset semantics, first-class compacted-topic support
// (snapshot reads and a deliberately awkward tombstone API), a versioned wire
// envelope, Prometheus metrics whose names are a contract, and OpenTelemetry
// trace context carried through the record headers so one odds update is
// followable from ingest to the browser.
//
// It is shaped like internal/platform/postgres on purpose: typed options,
// constructor injection, no package-level mutable state, a readiness checker
// that performs a real round trip, and metrics registered on an injected
// registry rather than the Prometheus default.
//
// # franz-go, not confluent-kafka-go
//
// CLAUDE.md §3 settles this and states the reason in terms of the prime
// directive: confluent-kafka-go "binds librdkafka through cgo, which breaks
// CGO_ENABLED=0 and therefore breaks the distroless image ... This constraint is
// not optional." The same argument put pgx in internal/platform/postgres. A
// static binary is what makes gcr.io/distroless/static:nonroot possible, and
// deploy/docker/go.Dockerfile enforces it with a `go tool nm | grep _cgo_`
// guard. Nothing in this package may reintroduce a C dependency.
//
// github.com/twmb/franz-go/pkg/kadm is used as well, for consumer-group lag.
// It is the same project, pure Go, and no other client offers group lag
// without hand-rolling DescribeGroups + OffsetFetch + ListOffsets.
//
// # The four topics
//
// CLAUDE.md §3: "odds.raw.{provider} (retention-based), odds.normalized
// (compacted, keyed by market), price.computed (compacted), wager.events
// (retention-based, the settlement audit trail)". See topics.go — the registry
// there is the Go-side mirror of what Terraform declares, and it records only
// the two properties the CODE has to reason about: whether a topic is compacted
// (which decides whether a tombstone is meaningful and whether a from-the-start
// read is a snapshot) and which domain identifier it is keyed by.
//
// Topic creation is NOT here. CLAUDE.md §9: topics are "created by Terraform,
// not by hand", and the broker runs with KAFKA_AUTO_CREATE_TOPICS_ENABLE=false
// so a typo surfaces as UNKNOWN_TOPIC_OR_PARTITION instead of silently
// conjuring a 1-partition topic with the wrong cleanup policy. This package
// therefore never creates, alters or deletes a topic; the integration tests
// create their own throwaway topics because a test that shares Terraform's
// topics is a test that pollutes a compacted snapshot.
//
// # Wire format: versioned JSON
//
// Chosen over protobuf/Avro, with the cost acknowledged rather than hidden.
//
// For JSON:
//
//   - kafka-ui. CLAUDE.md §9 calls it "non-negotiable while learning Kafka —
//     inspecting topics, offsets, and consumer lag visually is the difference
//     between a day and a week". A protobuf payload in kafka-ui is base64 noise
//     unless a schema is registered, and the value of the tool collapses.
//   - No schema registry exists. The §9 container inventory is a closed list and
//     a registry is not on it; adding one is an inventory change that needs an
//     ADR, and it would also be a new single point of failure in front of the
//     bus. Avro without a registry is worse than JSON, not better.
//   - The payloads are small and the topics are compacted. A compacted topic
//     keyed by market holds one record per market, so the steady-state size of
//     odds.normalized is bounded by the slate, not by the message rate.
//
// Against JSON, honestly: it is 2-4x larger on the wire than protobuf, it has
// no schema enforcement, and float64 round-tripping through decimal text is a
// real hazard for a system whose core type is a price. Three mitigations:
//
//  1. Batch compression (lz4) recovers most of the size difference, because
//     JSON from one topic is highly repetitive.
//  2. The envelope is VERSIONED — see EnvelopeVersion. A decoder refuses a
//     version it does not know rather than silently reading half a record, which
//     is the failure mode that would otherwise poison a compacted topic that
//     CLAUDE.md §3 requires to stay "replayable from scratch".
//  3. Go's encoding/json emits float64 with strconv's shortest-round-trip
//     formatting, so a float64 survives a marshal/unmarshal cycle exactly. That
//     is a property of the encoder, not an accident, and internal/domain/odds
//     already stores prices as Decimal (float64) rather than as text.
//
// The envelope is the payload of a NON-tombstone record. A tombstone has no
// value at all, so its metadata cannot live in the body — which is why the same
// facts also appear as record headers. See envelope.go.
//
// # Durability posture: two producers, and neither is a compromise
//
// CLAUDE.md's headline SLO is odds staleness, which argues for latency;
// wager.events is "the settlement audit trail", which argues for never losing a
// record. These pull in different directions, so this package exposes two
// producer types rather than one producer with a knob:
//
//	NewOddsProducer  -> odds.raw.{provider}, odds.normalized, price.computed
//	NewAuditProducer -> wager.events
//
// It is two TYPES, not two configurations of one type, because that makes the
// pairing a compile-time property: there is no method on an OddsProducer that
// writes wager.events, and no async fire-and-forget method on an AuditProducer
// at all. No service needs both — ingest and pricer produce odds, api and
// settle produce wager events — so the split costs nothing at the call sites.
//
// What is IDENTICAL in both, and why:
//
//   - acks=all (kgo.AllISRAcks). Not tunable. On the single-broker KRaft
//     development cluster replication factor is 1, so acks=all and acks=1 cost
//     exactly the same — there is no latency argument for weakening it here. And
//     weakening it here would become silent data loss the moment phase 10 runs
//     Kafka as a StatefulSet with RF>1, which is precisely the class of bug that
//     is invisible until the day a broker dies.
//   - Idempotent production ON (franz-go's default, and never disabled). This is
//     what makes a producer-side retry safe: without a producer ID and sequence
//     numbers, an ack lost on the way back produces a duplicate on retry. It
//     constrains max in-flight requests per broker to 5, which franz-go enforces
//     for us. On a compacted topic a duplicate is merely wasteful; on
//     wager.events it is a phantom entry in an audit trail.
//   - lz4 batch compression, falling back to no compression if the broker
//     refuses. Not zstd: zstd's Go implementation costs more CPU per byte, and
//     the target is a 2 OCPU Ampere box shared with Postgres and the whole
//     service set.
//
// What DIFFERS, and it is not acks — it is what happens when delivery keeps
// failing:
//
//	                        odds (low latency)        audit (wager.events)
//	linger                  0                         5ms
//	record delivery timeout  30s, then give up        none, retry forever
//	record retries           bounded (10)             effectively unbounded
//	API shape               sync + async + Flush      sync only
//
// The asymmetry follows from the data. A lost odds update is SELF-HEALING: the
// topic is compacted and keyed by market, the next provider poll recomputes the
// same market, and the next publish restores the current line. So bounding the
// delivery timeout and dropping is correct — better a 30-second-old line than a
// producer buffer that grows until the process dies. A lost wager event is NOT
// recoverable by anything: there is no poller that will re-derive it, and
// CLAUDE.md §4 makes the ledger the source of truth. So the audit producer never
// gives up, and its Publish is synchronous so the caller learns the outcome and
// can refuse to commit the surrounding database transaction.
//
// linger=0 on the odds path is not "no batching". franz-go keeps buffering
// records for a partition while a produce request for that partition is in
// flight, so a high-rate publisher batches naturally without paying a fixed
// delay per batch. Linger only helps a LOW-rate publisher, and a low-rate
// publisher does not need help. sharpline_kafka_produce_batch_records is on the
// dashboard-adjacent metric set precisely so this claim can be checked rather
// than believed.
//
// # What is deliberately NOT here: Kafka transactions
//
// A transactional producer (exactly-once semantics) would be the obvious next
// reach for wager.events, and it is not built, because it does not solve the
// problem that actually exists. The dangerous window is between COMMIT on
// Postgres and the ack from Kafka: Kafka transactions cannot span the two, so
// exactly-once on the bus alone converts a duplicate-message problem into a
// lost-message problem and calls it progress. The correct mechanism is a
// transactional outbox — write the event to a Postgres table inside the same
// transaction as the ledger movement, and relay it to Kafka afterwards — and
// that is a phase 8 decision, made with the betting and settlement packages, not
// something to be pre-committed to here. Flagged rather than silently omitted.
//
// # Offset semantics: at-least-once everywhere, committed explicitly
//
// Auto-commit is DISABLED (kgo.DisableAutoCommit) on every consumer, and this is
// the most load-bearing decision in the file. CLAUDE.md §10: "the interesting
// bugs live in consumer-group rebalancing and offset handling."
//
// franz-go's auto-commit commits the offsets of records that PollFetches has
// RETURNED, not records the application has PROCESSED. A crash or a rebalance
// between the poll and the end of processing therefore loses work silently — the
// offsets say the records were handled and nobody will ever fetch them again.
// franz-go also offers kgo.AutoCommitMarks, which commits only what
// MarkCommitRecords has marked, and that is a genuinely safe middle ground. It
// is still not used, because it hides WHEN the commit happens (on a timer, in
// the background) and the whole point of this layer is that the commit boundary
// is visible in the code that owns the work.
//
// So: poll, process the batch in partition order, commit exactly the records
// that were handled successfully, then release the rebalance. At-least-once, and
// therefore duplicate deliveries are normal and every consumer must be
// idempotent. It is chosen for EVERY topic, and at-most-once is chosen for none:
//
//	odds.normalized, price.computed  a duplicate is a no-op. The topics are
//	                                 compacted and keyed, so a redelivered
//	                                 record carries the same value under the
//	                                 same key. The price hypertable additionally
//	                                 has a UNIQUE natural key
//	                                 (prices_natural_key_idx on selection_id,
//	                                 book_id, observed_at) so a redelivered
//	                                 price insert fails with 23505 and the
//	                                 writer treats that as success — see
//	                                 postgres.IsUniqueViolation.
//	odds.raw.{provider}              a duplicate is re-normalized to the same
//	                                 output and suppressed by the change-detection
//	                                 hash (CLAUDE.md §5).
//	wager.events                     a duplicate MUST NOT double-post to the
//	                                 ledger, and the answer is an idempotency key
//	                                 in Postgres, not offset gymnastics. Losing
//	                                 a settlement event is unrecoverable; seeing
//	                                 one twice is a unique-constraint violation.
//	                                 at-most-once here would be indefensible.
//
// # Rebalance handling, and the trap in it
//
// Consumers run with kgo.BlockRebalanceOnPoll, so no rebalance can be processed
// between a poll and the AllowRebalance that follows the commit. That single
// option removes the entire race where a partition is reassigned while its
// records are still being handled.
//
// OnPartitionsRevoked commits progress synchronously (franz-go blocks the
// rebalance until the callback returns, which is what makes this safe).
// OnPartitionsLost does NOT commit: the partitions are already gone, the
// generation is stale, and a commit at that point either fails or clobbers the
// progress of the member that now owns them.
//
// The trap: the obvious implementation of the revoke callback is
// Client.CommitUncommittedOffsets, and it is WRONG here. "Uncommitted" means
// "polled but not yet committed", which includes records this process fetched
// and never handled. Committing them on revoke is data loss with a clean
// conscience. So the consumer tracks the last SUCCESSFULLY HANDLED record per
// partition and commits exactly those. See consumer.go.
//
// # Compacted topics: a null value is a deletion
//
// CLAUDE.md §3: "a compacted topic keyed by market_id IS the current-line
// snapshot, replayable from scratch". Two consequences this package makes
// first-class rather than leaving to a comment:
//
//   - A NULL VALUE IS A TOMBSTONE. It deletes the key from the snapshot for
//     ever, and after delete.retention.ms the tombstone itself is collected, so
//     the deletion becomes unobservable and irreversible. Publish therefore
//     REFUSES an empty payload and names the alternative. Deleting a key
//     requires the separate Tombstone API, which requires a written Reason and
//     an explicit acknowledgement constant. It is awkward on purpose: an
//     accidental tombstone on odds.normalized silently removes a market from
//     every client that resyncs afterwards.
//   - READING FROM THE BEGINNING IS A SNAPSHOT. Snapshotter does exactly that,
//     WITHOUT a consumer group, so a snapshot read commits no offsets and
//     disturbs no live consumer. It also folds last-write-wins per key, because
//     compaction is asynchronous: an uncompacted tail legitimately holds several
//     versions of one key, and code that assumes one-record-per-key on a
//     compacted topic is code that works only after the log cleaner has run.
//
// # Keying is type-safe
//
// Phase 1 made confusing a MarketID with an EventID a compile error
// (internal/domain/ids.go says so, and names the compacted Kafka key as one of
// the two reasons the file exists). That guarantee is preserved here: the
// publish methods take the domain type the topic is keyed by, so
//
//	p.PublishNormalized(ctx, someEventID, msg)   // does not compile
//
// A mis-keyed record on a compacted topic breaks two things at once — ordering
// (per-key ordering is per-partition, and the key chooses the partition) and
// compaction (the snapshot would hold two keys for one market and never collapse
// them). Neither failure is loud.
//
// # Metric names are a contract
//
// Three series here were already written into deploy/observability before a line
// of this package existed, and they are matched EXACTLY:
//
//	sharpline_kafka_consumer_lag_records{group,topic}   dashboard panel 14, alert KafkaConsumerLagHigh
//	sharpline_kafka_consumer_lag_seconds{group,topic}   dashboard panel 15, alert KafkaConsumerLagSecondsHigh
//	sharpline_kafka_consumer_group_rebalances_total{group}  dashboard panel 15, alert KafkaRebalanceStorm
//
// The dashboard's own description of panel 14 says the lag is "reported by each
// franz-go consumer for its own assigned partitions", and that sentence is a
// specification: the group's lag is available cluster-wide from any member, so a
// consumer that exported all of it would multiply the graph by the number of
// members. This package therefore filters group lag down to the partitions THIS
// member currently owns, and deletes the series for partitions it loses, so that
// the panel's `sum by (group, topic)` equals the true group lag. The refresher
// that does it is Consumer.runLagRefresher in consumer.go, driven off the same
// assignment callbacks that maintain the owned-partition set — it lives with the
// consumer because it needs that set, and there is no separate lag file.
//
// A `partition` label is added to both lag gauges. It aggregates away under the
// dashboard's sum/max, and it is what turns "the group is behind" into "partition
// 2 is behind", which is the difference between a diagnosis and an observation.
//
// Everything else this package exports is new, prefixed sharpline_kafka_, and
// documented at its definition in metrics.go with the PromQL a panel would use.
// Three of those new series have since been adopted by the dashboard and the
// alert rules and are now just as binding as the three above; metrics.go's header
// lists all six in one place.
//
// # Trace context travels in the headers
//
// CLAUDE.md §9 wants "traces spanning ingest → pricer → stream so a single odds
// update can be followed end to end". A span that ends at the producer cannot do
// that, so the producer INJECTS W3C trace context into the record headers and
// the consumer EXTRACTS it and starts its span as a child of the remote span.
// One trace therefore spans the whole pipeline traversal rather than four
// disconnected traces joined by a correlation id.
//
// The default propagator is propagation.TraceContext, set explicitly and NOT
// taken from otel.GetTextMapPropagator(). That is a deliberate divergence from
// how internal/platform/postgres treats the OTel globals, and the reason is
// mechanical: the OTel Go global propagator defaults to a no-op until some
// cmd/ entrypoint calls otel.SetTextMapPropagator. No entrypoint does that yet.
// Relying on the global would mean headers silently carried nothing, the traces
// would stop at every hop, and the symptom would be indistinguishable from
// tracing being switched off. A caller that has installed a global propagator
// can pass it in Options.
//
// Baggage is not propagated. Adding it is a decision about what user-derived
// data is allowed to cross a service boundary, and that belongs in an ADR.
//
// # Concurrency: one record at a time, per member
//
// The consume loop handles a poll's records sequentially, in partition order.
// This is a choice, not an oversight: Kafka's own unit of parallelism is the
// partition, and the way to go faster is more partitions and more group members
// — which is exactly what the phase 10 HPA demo scales. In-process fan-out
// across partitions would add a commit barrier, a partial-failure matrix and an
// ordering hazard, in exchange for throughput that a second replica provides for
// free. Revisit when a real measurement says a single member cannot keep up with
// one partition.
package kafka
