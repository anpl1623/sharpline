// Package analytics is phase 9: the signals stage that turns the priced board
// into findings — positive expected value, arbitrage, and steam.
//
// CLAUDE.md §6 calls this surface "the differentiator" and §11 row 9 says
// something about it that no other package in this repository has to carry:
//
//	"Analytics IN GO: +EV finder, arbitrage scanner, steam detection, CLV
//	 tracking. THIS IS THE REFERENCE IMPLEMENTATION PHASE 12 VALIDATES AGAINST."
//
// # The semantics here are a cross-language contract, not an implementation
//
// §3 sequences Flink deliberately: these detectors are written in Go first, and
// phase 12 rewrites them as Flink SQL jobs which are then checked against this
// code — "same inputs, same outputs, or the Flink job is wrong". That makes
// every threshold, every window boundary, every tie-break and every exclusion
// rule in this package a term of a contract that has to survive a rewrite into a
// different language by a different execution engine.
//
// The practical consequence, and the rule this package is written under: A
// SEMANTIC THAT IS NOT WRITTEN DOWN PRECISELY ENOUGH TO REIMPLEMENT FROM THE
// PROSE IS A DEFECT, not a documentation nit. Someone holding docs/analytics-
// semantics.md and no Go source must be able to write the SQL and get identical
// answers, which is why the comments below spell out things that would otherwise
// be obvious from three lines of code — the tie-break that makes a sort total,
// the half-open bound on a window, the epoch the hop grid is aligned to, the
// exact instant an age is measured from.
//
// docs/analytics-semantics.md is the prose form of that contract. Where this
// package and that document disagree, one of them is wrong and it is a bug in
// both until they agree again.
//
// # This package is a SURFACE over existing primitives, not a second copy
//
// Nothing here re-derives odds mathematics, and that is the one defect this
// project cannot absorb (CLAUDE.md §10: "wrong odds math is the one bug class
// that destroys the project's credibility" — two implementations means two
// answers and no way to tell which is wrong). Specifically:
//
//   - internal/domain/odds owns devigging, expected value, edge, Kelly and CLV.
//   - internal/pricing computes fair value from the sharp reference book
//     (ADR 0006) and already scores EVERY quote on EVERY market, and already
//     runs the arbitrage and middles scanners, publishing all of it on
//     price.computed as a [pricing.ComputedMarket].
//
// What phase 9 adds on top of that is the part that is genuinely missing:
// WINDOWING, THRESHOLDS, RANKING, PERSISTENCE, ALERTING, and one new detector.
// ev.go filters and ranks quotes internal/pricing already scored; arb.go applies
// a staleness discipline to findings internal/pricing already made; steam/ is
// the only detector in this tree that computes something no earlier phase did.
//
// # Where each analytic runs
//
// CLAUDE.md §3's service table has exactly six binaries and phase 9 adds none.
// The three detectors in this package run INSIDE `pricer`, as a second consumer
// loop beside internal/pricing's:
//
//	odds.normalized ──▶ pricing.Service ──▶ price.computed ──▶ analytics.Service
//	                                                              ├─▶ signals.ev
//	                                                              ├─▶ signals.arb
//	                                                              ├─▶ signals.steam
//	                                                              └─▶ Postgres
//
// It is a SECOND CONSUMER rather than a hook inside the pricing pass, and the
// reason is a requirement internal/pricing states about itself: [pricing.PriceFunc]
// "MUST BE A PURE FUNCTION OF rec", because the pricer suppresses a
// republication whose input fingerprint has not changed and that suppression is
// only sound if two calls over one record produce one answer. A detector that
// wrote to Postgres and to Kafka from inside that call would break the purity
// the whole change-detection path rests on. Consuming the pricer's OUTPUT keeps
// the seam clean, costs one extra decode of a record already in the page cache,
// and has a second benefit that matters more: the analytics stage can be lifted
// out of the pricer process and into its own deployment later without touching a
// line of it, because it depends on a topic rather than on a function call.
//
// CLV is deliberately NOT here. [odds.CLVResult]'s own doc comment assigns it:
// "the settle service writes one per graded leg, the API serves it, and the
// phase-12 Flink job reproduces it." A closing price is knowable only once an
// event has started, which is settlement's clock and not the pricer's.
//
// # Signals go on the bus, and they are events rather than state
//
// The four signals topics are declared in internal/platform/kafka/topics.go and
// in deploy/terraform/modules/kafka-topics. All four are RETENTION-BASED. A
// finding is a point-in-time event: "the latest steam move for market X"
// supersedes nothing, so compacting the topic would silently destroy signal
// history the way compacting wager.events would destroy the audit trail — the
// head of the log still looks right, which is what makes it invisible.
//
// signals.ev is an ADDITION to §3's event-flow diagram, which names only steam,
// arb and clv. It is flagged as an addition in topics.go, in the Terraform
// variables and in both READMEs. §6's Analytics bullet leads with the +EV
// finder, so leaving it off the bus would make the one analytic phase 12 could
// not replace like for like.
//
// # Everything a detector writes is REPLAYABLE and keyed on its inputs
//
// migrations/00009 keys all three signal tables on values derived from the
// input alone — never on a clock reading of ours — and every write is
// ON CONFLICT DO UPDATE rather than DO NOTHING, because a replay after a
// detector fix IS the correction and must land:
//
//	ev_signals         (selection_id, book_id, quote_observed_at)
//	steam_signals      (market_id, selection_id, window_start, window_end)
//	arbitrage_signals  (market_id, observed_at, legs_fingerprint)
//
// This package honours that by holding NO clock anywhere a finding's identity
// could reach. `detected_at` is stamped from the injected [Clock] and is stored
// but never keyed; every other instant on a finding is propagated from the
// provider observation that produced it. Two runs of this package over the same
// price.computed log therefore write the same rows, which is what makes the
// phase-12 comparison possible at all.
//
// # What is NOT here, and where it is instead
//
//	the closing price and CLV        internal/analytics/clv, written by `settle`
//	the query surface, leaderboard   internal/httpapi, served by `api`
//	the middles detector             internal/pricing/middles.go — phase 9 adds
//	                                 no middle ALERT (a middle is a POSITION with
//	                                 a hit probability, not an event with a
//	                                 guaranteed return), so nothing here persists
//	                                 one and migrations/00009 declares no table
//	the odds arithmetic              internal/domain/odds
package analytics
