// Package writer is the Timescale writer: the consumer that turns the
// `odds.normalized` stream into durable line history.
//
// CLAUDE.md §3's event flow names it explicitly — `odds.normalized` fans out to
// the pricer, to the Flink jobs (phase 12), and to the "timescale writer (line
// history)". This package is that third branch. It is the only write path into
// the `prices` hypertable, and it is the reason CLAUDE.md §3 calls line history
// "the interesting dataset (CLV, steam detection, book disagreement)".
//
// # What a record is, and why the catalogue travels with it
//
// `odds.normalized` is COMPACTED and keyed by domain.MarketID (see
// internal/platform/kafka/topics.go). The log cleaner keeps only the most recent
// record per key, so anything a record omits is gone from the snapshot for ever.
// That makes the record value necessarily a COMPLETE current-market snapshot —
// not a delta — and it is what lets CLAUDE.md §3 say "a compacted topic keyed by
// market_id *is* the current-line snapshot, replayable from scratch".
//
// The writer needs exactly that completeness for a second, independent reason:
// `prices` carries foreign keys to `selections` and `books`, and `selections`
// hangs off `markets` → `events` → `leagues` → `sports`. A price cannot be
// stored until its whole spine exists. So the catalogue is upserted from the
// same record, in the same transaction, immediately before the prices that
// reference it. There is no separate catalogue service and no seeding step: the
// catalogue is a projection of the stream, and an empty database before ingest
// runs is CORRECT.
//
// Payload documents the wire contract in full.
//
// # Two tables, two mutability rules, and they are not the same rule
//
//	prices       APPEND-ONLY, enforced by trigger (migrations/00003 installs
//	             prices_no_update, prices_no_delete, prices_no_truncate). A new
//	             price is a new row (CLAUDE.md §4). There is no upsert path and
//	             this package must never look for one. The only tolerated
//	             conflict resolution is ON CONFLICT DO NOTHING against the
//	             natural key, which is the idempotency guard described below.
//
//	catalogue    MUTABLE, with a schema-wide set_updated_at trigger. An event's
//	             status, clock and score move; a market's line and status move.
//	             Those rows are upserted in place, guarded so that an
//	             out-of-order redelivery cannot roll a newer observation
//	             backwards.
//
// # Idempotency, and what it has to mean on an append-only table
//
// Delivery is at-least-once. A consumer-group rebalance, a crash between the
// write and the offset commit, or a deliberate topic replay all redeliver
// records this process has already handled. On a mutable table "idempotent"
// means last-write-wins and costs nothing to arrange. On an APPEND-ONLY table it
// is sharper: writing the same observation twice does not overwrite anything, it
// manufactures a second row that never happened. Two rows for one quote inflate
// every line-movement count, double-weight a book in a cross-book comparison,
// and give CLV two candidate closing prices.
//
// The fix is the natural key, and it is a database constraint rather than a
// convention in this package:
//
//	CREATE UNIQUE INDEX prices_natural_key_idx
//	    ON prices (selection_id, book_id, observed_at DESC);
//
// (selection_id, book_id, observed_at) is domain.Price's identity — price.go
// says so outright — and a hypertable's unique index MUST include the
// partitioning column, which observed_at is. Every insert here is therefore
//
//	... ON CONFLICT (selection_id, book_id, observed_at) DO NOTHING
//
// so a redelivery is a measured no-op rather than a unique-violation storm.
// Duplicates suppressed this way are counted under
// sharpline_writer_price_rows_total{outcome="duplicate"}: a redelivery is normal
// operation, but a SUSTAINED duplicate rate means the group is thrashing and is
// worth seeing.
//
// This is also why observed_at must be the PROVIDER's instant and not ours. A
// row keyed on a clock reading of our own would be a different key on every
// replay, and the constraint would silently stop deduplicating anything.
//
// # The commit boundary: why the flush is synchronous
//
// internal/platform/kafka's Consumer does not auto-commit. It commits the last
// SUCCESSFULLY HANDLED record per partition, once per poll, after the whole
// batch has been through the handler. A handler returning nil is therefore a
// claim that the record is durable — and if that claim is false, the offset
// advances over work that was never written and the prices are gone. No replay
// recovers them, because the offset says they were handled.
//
// So HandleMessage does not buffer across records. It opens a transaction,
// upserts the catalogue, inserts every quote in the record as ONE multi-row
// statement, commits, and only then returns nil. Durability precedes the return
// on every path.
//
// # Why that is still batched, and what a cross-record buffer would cost
//
// The unit that gets batched is the RECORD, not the row, and the two are far
// apart: one `odds.normalized` record is a whole market — every selection at
// every book — which migrations/00003 sizes at 6 selections × 10-20 books, so
// 60 to 120 price rows arrive together and are written by a single INSERT ...
// SELECT unnest(...) with one round trip. MaxRowsPerStatement chunks anything
// larger inside the same transaction. "One row per message" — the failure mode
// worth avoiding — is not what this does.
//
// A buffer spanning several records, flushed on a size AND a time bound, was
// considered and is UNIMPLEMENTABLE ON TOP OF THIS CONSUMER WITHOUT LOSING
// DATA, which is worth writing down because it is the obvious next idea:
//
//   - The Consumer hands over one record at a time on one goroutine and offers
//     no end-of-poll hook. A handler that buffers and returns nil has already
//     told the Consumer the record is durable; the commit that follows the poll
//     will advance past every buffered record, and a crash in that window loses
//     exactly the rows still in memory.
//   - Making the handler BLOCK until its record is flushed does not rescue it.
//     Because the handler is sequential, a blocked handler stops new records
//     from arriving, so the buffer can never grow beyond one record and the
//     time bound degenerates into "one row per flush interval" — strictly worse
//     than flushing immediately.
//
// If measurement ever shows a transaction per record is the bottleneck, the
// correct fix is a BATCH-LEVEL handler on the Consumer (hand the whole poll to
// the handler, commit after it returns), not a buffer in this package that
// quietly weakens the durability claim. That is recorded as a request against
// internal/platform/kafka rather than worked around here.
//
// # Retries are Kafka's job, not this package's
//
// The phase-2 handoff is explicit: "Retries are restricted to transient
// CONNECTION failures. Never retry a failed transaction: it double-applies a
// ledger write. The classification lives in internal/platform/postgres — extend
// it there rather than adding a second policy." This package adds no policy. A
// failed transaction is returned to the Consumer, whose ErrorPolicy decides:
// under ErrorPolicyStop the offset is not committed and the record is
// redelivered, which is the retry. postgres.InTx is used exactly as shipped, so
// the deferred-constraint handling, the detached rollback and the panic guard
// all apply unchanged.
//
// # Tombstones are NOT deletions here, and that is a decision, not an oversight
//
// A tombstone on `odds.normalized` removes a market from the CURRENT-LINE
// snapshot. It says nothing about history, and history is what this table is.
// Deleting price rows because a market left the slate would destroy the dataset
// CLAUDE.md §3 calls the interesting one — and it is impossible anyway, because
// prices_no_delete refuses it.
//
// The writer therefore RETAINS everything on a tombstone and counts it under
// sharpline_writer_messages_total{outcome="tombstone"}. It also declines to
// invent a catalogue mutation: a tombstone carries no market status, so writing
// `status = 'closed'` would be fabricating a state transition the provider never
// reported. The market row keeps its last observed status, which is the last
// thing that was actually true.
//
// # Time
//
// Three instants, three different questions, all stored (migrations/00003 has
// the full argument and the measured byte cost):
//
//	observed_at   the PROVIDER's own instant, propagated unchanged. The
//	              hypertable's partitioning column, the subtrahend in the
//	              headline staleness SLO, and phase 12's Flink event-time
//	              attribute. This package NEVER stamps it.
//	ingested_at   when `ingest` received the payload carrying the quote. Read
//	              off the record, never re-stamped, so a replay reproduces the
//	              original value rather than today's.
//	created_at    the database's own clock, defaulted by the schema.
//
// All three are TIMESTAMPTZ. Every time.Time this package writes has been
// through a domain constructor, which normalises to UTC.
//
// # NO MOCK DATA
//
// Nothing in this package produces a row from a literal. Every value written
// arrives on the bus, and every row is derived from a payload that has been
// through internal/domain's constructors — so a market that could not exist in
// the domain cannot reach the database. The tests create their own records and
// assert on the rows those records produced; nothing is seeded and nothing
// stands in for ingested data.
package writer
