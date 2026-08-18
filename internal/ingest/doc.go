// Package ingest is the composition root of the `ingest` binary: it is where
// the four stages CLAUDE.md §3 draws as one arrow are actually joined together.
//
//	provider → ingest → [odds.raw.{provider}] → normalizer → [odds.normalized]
//	                                                            ├→ pricer
//	                                                            └→ timescale writer (line history)
//
// Nothing here implements a stage. internal/ingest/provider is the adapter seam,
// internal/ingest/scheduler owns adaptive polling and the quota budget,
// internal/ingest/normalizer owns the mapping and change detection, and
// internal/ingest/writer owns the Timescale line history. This package supplies
// the glue those four deliberately do not have: it selects the adapter, adapts
// the adapter to the scheduler's Poller and Catalogue seams, publishes raw
// payloads to the bus, runs the two consumer loops, and sequences shutdown.
//
// # Which adapter is running is never ambiguous
//
// The selection rule is fixed by ADR 0003 and the contract ledger: the real The
// Odds API adapter when ODDS_API_KEY is set, the synthetic stochastic market
// maker when it is not. That is the single most consequential runtime switch in
// this binary, because a board of simulated prices and a board of real ones look
// identical, and CLAUDE.md §0's whole framing — this is a simulation and that
// must be stated plainly — collapses the moment an operator cannot tell which
// one they are looking at.
//
// So it is announced four ways, and none of them is a single line that scrolls
// past:
//
//  1. A startup log record at WARN when the source is simulated, at INFO when it
//     is real, naming the adapter, the mode and whether a key was present.
//  2. `provider` and `simulated` are attached to the SERVICE LOGGER, so every
//     subsequent line this binary writes carries them.
//  3. sharpline_ingest_provider_info{provider,mode,simulated} is a constant 1 —
//     an info metric, so a dashboard or an alert can join on it.
//  4. Every published record already carries the provider slug: the raw topic is
//     odds.raw.{provider} and NormalizedMarket.Provider repeats it, which ADR
//     0003 requires so a consumer can tell.
//
// There is deliberately NO FAILOVER from the real adapter to the synthetic one.
// ADR 0003: substituting simulated prices for real ones in a running deployment
// "would be indistinguishable from fabricating market data". A key that is set
// but whose adapter cannot be built is a startup failure, and a quota that runs
// out freezes the board loudly (see scheduler.sweep) rather than quietly filling
// it with a simulation.
//
// # Change detection happens twice, and the two are not the same check
//
// This package hashes what the ADAPTER returned, per market, and publishes an
// event's raw payload only when at least one of its markets moved or the refresh
// ceiling elapsed. That is CLAUDE.md §5's "backing off on unchanged payloads":
// it is an upstream traffic filter over odds.raw.{provider}, it lives only in
// memory, and it is NOT authoritative.
//
// The normalizer hashes the PUBLISHED RECORD, per market, and that fingerprint
// is the authoritative one: it is carried on the wire, warmed from the compacted
// topic across restarts, and covered by a structural test. If the two ever
// disagree the normalizer wins, and the refresh ceiling here bounds how long a
// disagreement can hide a real move — one Config.RawRefreshAfter, after which
// the payload is republished whether or not this package thinks it changed.
//
// # Shutdown order, and what each step is protecting
//
// Getting this wrong loses data on every deploy, so it is explicit:
//
//  1. Stop polling. Cancelling the run context stops every league goroutine from
//     scheduling a new sweep.
//  2. Drain in flight. scheduler.Run does not return until every sweep it
//     started has finished; a sweep that has already issued its provider request
//     has already spent the credit, so it is given Config.ShutdownGrace rather
//     than being severed.
//  3. Flush the producer. kafka.OddsProducer accepts a record before it is
//     written; closing without flushing discards accepted-but-unwritten records.
//     Flush happens after every stage has stopped producing and before the
//     producer is closed.
//  4. Commit consumer offsets. kafka.Consumer commits the last successfully
//     handled record per partition as its Run returns.
//
// Steps 3 and 4 look inverted against the sequence "flush, then commit", and the
// property that makes them safe is a contract on the handlers rather than an
// ordering here: both stage handlers must publish SYNCHRONOUSLY. A record whose
// acknowledgement is awaited inside HandleMessage is durable before the offset
// is even marked, so no offset can ever be committed ahead of the record it
// produced. Options.Normalizer states this as a requirement; a handler that used
// PublishNormalizedAsync would silently reintroduce the loss this ordering
// exists to prevent.
//
// # What this package does not do
//
//   - It does not decode a provider payload. The adapter does syntax; the
//     normalizer does semantics.
//   - It does not price anything, and it does not write Postgres itself — it
//     runs the writer that does.
//   - It never reads the process environment. internal/platform/config is the
//     one loader (CLAUDE.md §12) and LoadConfig turns its output plus the
//     selected adapter into this package's typed configuration.
package ingest
