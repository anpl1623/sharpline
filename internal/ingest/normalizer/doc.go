// Package normalizer maps provider payloads onto the domain and decides which
// of them are worth putting on the bus.
//
// CLAUDE.md §3 places it between two topics:
//
//	provider → ingest → [odds.raw.{provider}] → normalizer → [odds.normalized]
//
// and §5 states the two obligations that shape everything here:
//
//	"Each provider gets an adapter behind one interface. […] Hash each
//	 normalized market to suppress no-op updates — most polls return identical
//	 data and must not generate bus traffic."
//
// Nothing downstream of this package knows which provider a price came from.
// That is the whole point: the pricer, the fanout hub and the browser see
// domain values, and the provider's quirks — American odds where the
// documentation promises decimal, a line stated per bookmaker, a market key
// vocabulary, a timestamp that lives at two different nesting levels — are
// absorbed here or nowhere.
//
// # The three stages, and why they are separate types
//
//	Decoder     provider bytes  → RawEvent      (syntax; one per provider)
//	Mapper      RawEvent        → []MarketView  (semantics; pure, shared)
//	Normalizer  kafka.Delivery  → odds.normalized (change detection; stateful)
//
// Only the Decoder is per-provider. The Mapper is shared, which is what makes
// "two providers produce identical domain values for equivalent input" a
// property the code has BY CONSTRUCTION rather than one that two
// implementations have to agree on by luck: there is one mapping, so there is
// nothing for a second one to diverge from. mapper_test.go pins the remaining
// degree of freedom — that two decoders reading different wire shapes converge
// on the same RawEvent and therefore on byte-identical records — and the
// golden-file half of that claim lives with the decoder it tests, in
// internal/ingest/provider/theoddsapi, because that is where the recorded
// payloads are.
//
// The Mapper is a PURE FUNCTION. It reads no clock, does no I/O, and consults
// no state. Every time-dependent derivation — whether an event is live, what a
// market's UpdatedAt is — is computed from the payload's own observation
// instants. Replaying odds.raw.* six months later therefore reproduces exactly
// the records it produced the first time, which is what makes the raw topic
// worth retaining at all.
//
// # Identifier derivation, and why it is load-bearing
//
// odds.normalized is COMPACTED and KEYED BY market_id (kafka/topics.go). The
// broker keeps the latest record per key, so the topic IS the current-line
// snapshot. That property survives only if a market's identifier is byte-identical
// across every poll and across every restart. An identifier that drifted — because
// it embedded a timestamp, a map iteration order, a hash of the whole payload, or
// a counter — would make compaction accumulate one dead key per drift instead of
// collapsing to one live key, and the failure is silent: the topic still works,
// the snapshot just quietly grows and serves stale duplicates.
//
// So identifiers are derived, deterministically, from provider-stable attributes
// only. See identity.go for the derivation and identity_test.go for the
// cross-process stability test.
//
// The market identifier deliberately DOES NOT include the line. A market whose
// line moves from -3.5 to -4 is the same market — domain.Market.WithLine exists
// for exactly that transition — and splitting it into two keys would shatter the
// line-movement history that CLAUDE.md §6 puts on the board and §9's CLV depends
// on. The consequence is that Market.Line is a CONSENSUS across books while each
// domain.Price carries the line its own book quoted; migrations/00003 says the
// same thing from the schema side: "markets.line carries the market's CURRENT
// line. This column carries the line THE QUOTE WAS MADE AT. They are different
// facts and the second one is the whole reason this table is interesting."
//
// # Change detection
//
// Fingerprint is a SHA-256 over a canonical encoding of the published record.
// The rule is one sentence and fingerprint_test.go enforces it structurally:
//
//	THE FINGERPRINT COVERS THE ENTIRE PUBLISHED PAYLOAD EXCEPT THE FINGERPRINT
//	FIELD ITSELF AND THE OBSERVATION AND INGESTION INSTANTS.
//
// Both halves are load-bearing and getting either wrong is a serious defect:
//
//   - Too inclusive — put the provider's observation timestamp in, and every
//     poll differs, suppression never fires, and the bus carries thousands of
//     no-op records a minute. The Odds API's `last_update` advances on every
//     refresh whether or not the price moved, so this is not a hypothetical.
//   - Too exclusive — leave the line, a selection's price or the market status
//     out, and a real move is suppressed. The compacted topic keeps serving the
//     old record for ever, because the only thing that would replace it is the
//     next change to the same key, and the board goes stale while the market
//     moves.
//
// The structural test is what keeps this true as the payload grows: it walks
// every field of NormalizedMarket by reflection, mutates it, and requires the
// fingerprint to change unless the field is on a short, explicitly-declared
// exclusion list. Adding a field without deciding is a build failure, not a
// silent staleness bug six weeks later.
//
// # Where the fingerprint state lives, and why it is not Redis
//
// The state must survive a restart, or the first poll after every deploy
// republishes the entire board. It is warmed from odds.normalized itself — the
// compacted topic, read from the beginning with no consumer group, which is
// exactly what kafka.Snapshotter is for.
//
// CLAUDE.md §3 chose Kafka over NATS partly on this: "a compacted topic keyed by
// market_id IS the current-line snapshot, replayable from scratch, WHICH REMOVES
// A WHOLE CLASS OF CACHE-COHERENCY BUGS BETWEEN THE BUS AND REDIS." Putting the
// authoritative fingerprint in Redis would reintroduce precisely that class. The
// failure is concrete: Redis remembers fingerprint F for market M, the record for
// M is gone from the topic (a recreated topic, a truncated partition, a produce
// that failed after the fingerprint was written), and M is then suppressed for
// ever — invisible to every client that builds its snapshot from the log. The
// warm-start path cannot have that bug, because the thing it reads is the thing
// clients read.
//
// Redis is still the right home for a SHARED fingerprint cache once the
// normalizer runs as more than one replica: a group rebalance moves partitions
// between members, and a per-process store makes the new owner republish the
// markets it inherited. That is bounded and safe, so it is not urgent.
// FingerprintStore is the seam — declared here, by the consumer, per CLAUDE.md
// §12 — and MemoryStore is the implementation this phase ships. A Redis-backed
// implementation drops in behind it without touching this package, and should
// still be warmed from the topic rather than trusted over it.
//
// # Suppression has a ceiling
//
// Options.RefreshAfter republishes an unchanged market once its last publication
// is older than that. It costs a bounded trickle of bus traffic and buys two
// things: the record's observation instant cannot drift arbitrarily far behind
// the provider's, and any defect in the fingerprint self-heals within one refresh
// interval instead of persisting until the market next moves.
//
// # Staleness
//
// This package emits the `stage="normalized"` slice of the two pipeline
// histograms named in deploy/observability/rules/sharpline-alerts.yml. It is
// measured from the PROVIDER's observation instant, never from ingest time, and
// it is observed ONCE PER PUBLISHED PRICE because that is the unit the SLO is
// defined on — the dashboard says "freshness = … − observed_at carried on THAT
// PRICE". The headline SLO itself is `stage="fanout"` and belongs to `stream`.
//
// See metrics.go for the exact series and labels. They are a contract with
// deploy/observability; read the dashboard JSON before renaming one.
//
// # What this package does NOT do
//
//   - It does not talk to a provider. That is the adapter, in internal/ingest.
//
//   - It does not write Postgres. The Timescale line-history writer (§3) is a
//     separate consumer of odds.normalized.
//
//   - It does not price anything. Devig, EV and Kelly live in
//     internal/domain/odds and are applied by `pricer`.
//
//   - It does not suspend or close a market that stops appearing in a payload,
//     and that is a REVERSAL of what an earlier draft of this file promised.
//     The reasoning is worth keeping, because the feature is genuinely wanted.
//
//     Inferring "this market is gone" from "this market is absent from this
//     payload" requires knowing what the payload was ASKED for. It is not.
//     The Odds API serves featured markets from /odds and player props from
//     /events/{id}/odds, so one event produces two raw records with disjoint
//     market sets, and a normalizer that suspended on absence would have each
//     record suspend the other's markets — a flip-flop on every poll, on the
//     compacted topic, at full slate width. That is a worse failure than the
//     staleness it would fix.
//
//     Doing it correctly needs the request scope carried on the raw record
//     (which markets were requested for which event), which is a change to
//     internal/ingest and to the raw envelope, not to this package. Until then
//     a market that stops being quoted keeps its last published state, bounded
//     by the refresh ceiling only in freshness and not in existence. An event
//     that disappears entirely is invisible here either way, and sweeping it
//     belongs to `settle` in phase 8.
package normalizer
