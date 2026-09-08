// Package clv turns a placed wager's leg into a closing line value measurement.
//
// CLAUDE.md §6 makes "CLV tracking per user" one of the three analytics
// differentiators, and §11 phase 9 makes this the REFERENCE IMPLEMENTATION that
// phase 12's Flink SQL job is validated against: "same inputs, same outputs, or
// the Flink job is wrong". Everything in this file is therefore written to be
// reimplemented from the prose alone. A sentence here that leaves a choice open
// is a sentence that lets the two implementations disagree, and the disagreement
// would be a leaderboard that ranks two different populations.
//
// # What this package is NOT
//
// It is not the CLV arithmetic. odds.EvaluateCLV is the arithmetic, and
// internal/domain/odds/clv.go carries the argument for every part of it: fair
// probabilities only, the two scalar measures, the tie band, the line-move rule,
// the void rule, and the aggregation. None of that is repeated here and none of
// it may be re-derived here — CLAUDE.md §10 is blunt about why two
// implementations of one formula eventually disagree.
//
// What this package does is THE JOIN. odds.EvaluateCLV takes two complete,
// devigged market snapshots and refuses everything that is not a like-for-like
// comparison; the hard part is producing those two snapshots from a hypertable
// of individual observations, and deciding — precisely, and identically in Go and
// in SQL — which observations they are made of.
//
// # WHAT IS HERE
//
//	doc.go    the definition of a closing price, and the argument for it
//	ports.go  the two reads this package needs, declared by this package
//	clv.go    the snapshot construction, the devig, and the guarded evaluation
//
// =============================================================================
// THE CLOSING PRICE
// =============================================================================
//
// # The definition, in one sentence
//
// The CLOSING SNAPSHOT of market M is, for EVERY selection s of M, the price at
// BOOK B for s with the greatest observed_at that is at or before M's CLOSING
// INSTANT, strictly after (closing instant − ClosingLookback), and NOT inside any
// suspension episode of M; and it is a closing snapshot at all only if every
// selection of M yielded such a price and every one of them agrees on the market
// line.
//
// Each clause below is a decision, and each is load-bearing.
//
// # 1. The closing instant is events.scheduled_start
//
// Not the actual kickoff, not the instant the event's status changed to `live`,
// not the last observation before the status changed, and not "the last price we
// have".
//
// scheduled_start is the only candidate that is (a) knowable before the event
// starts, so a market can be described as closing before anyone has to grade it;
// (b) stable under replay, because it is a stored column rather than a
// consequence of when the ingest poller happened to fire; and (c) identical in Go
// and in Flink SQL without either side needing a status-transition history.
//
// The rejected alternatives, and why:
//
//   - ACTUAL KICKOFF. There is no such column. The closest is the first
//     observation carrying status `live`, which is a function of the poll cadence
//     — ADR 0003's ladder — so two replays with different cadences would close the
//     same market at two different instants and compute two different CLVs.
//   - THE LAST PRICE BEFORE THE MARKET SUSPENDED. Circular: a market may be
//     suspended and reopened several times, so "the last price before suspension"
//     is not a single instant, and picking one of them requires a rule that would
//     itself have to be written down.
//   - THE MAXIMUM observed_at ON THE MARKET. Contaminated by in-play prices,
//     which are answers to a different question — see §5 below.
//
// A scheduled_start that has moved (a postponed fixture rescheduled) closes the
// market at the NEW start, because that is the row the query reads. That is the
// correct behaviour: the market that traded into the rescheduled start is the
// market the wager was live against.
//
// # 2. ClosingLookback is a REQUIRED lower bound, and it is semantic
//
// The lower bound exists for two independent reasons and either alone would
// justify it.
//
// Mechanically, `prices` is a Timescale hypertable partitioned on observed_at
// with no retention policy (migration 00004), so an unbounded backward walk
// consults every chunk that has ever existed. queries/analytics.sql calls this
// the sharpest edge on that table.
//
// Semantically, a quote from six days before kickoff is not a closing line. It is
// a market nobody has priced since, and scoring a wager against it would report
// the bettor as having beaten a number the market had abandoned. The lookback is
// the declared statement of how stale a quote may be and still count as the
// close; it is a parameter of this package ([Options.ClosingLookback]) rather than
// a constant inside a query, and phase 12 must be given the same value.
//
// # 3. ONE BOOK, AND IT IS THE BOOK THE WAGER WAS STRUCK AT
//
// Devigging is defined over a complete market: the margin is the excess of Σ1/d
// over 1, and a set of best-prices-across-books has no such excess to remove.
// odds.NewFairMarketSnapshot enforces this mechanically by refusing probabilities
// that do not sum to 1 within odds.CLVDevigTolerance. So the close is one book's
// whole outcome set, never a mosaic.
//
// WHICH book is a real decision, and this package fixes it: the SAME BOOK the leg
// was priced at. odds/clv.go permits either — "the two snapshots may come from
// different books, and usually should" — and describes scoring against a sharp
// reference book as the standard construction. That construction is declined
// here, and the reason is not preference:
//
//	A sharp reference book quotes its own line. When the customer took home −3
//	at their book and the reference book closed at −3.5, odds.EvaluateCLV
//	reports a LINE MOVE — and odds.AggregateCLV excludes every line-moved
//	sample from the mean and the beat rate. Scoring against a different book
//	would therefore drop most spread and total legs out of every aggregate,
//	because two books disagreeing by half a point is the ordinary state of a
//	market rather than an exception.
//
// Same-book scoring keeps the line comparison meaningful: a line move then means
// THE MARKET MOVED, which is the thing the flag is for, instead of meaning two
// books disagreed. It also measures the number the customer could actually have
// had, which is the honest reading of "did you beat the close".
//
// The cost is named rather than hidden: two users betting two different books are
// scored against two different closes, so the leaderboard compares each user
// against their own book's market. A single mandated reference book would not fix
// that either — it would replace it with a mass exclusion — and the only real fix
// is a consensus close over several sharp books, which is a model rather than a
// query and belongs behind its own ADR.
//
// # 4. A SUSPENDED MARKET'S STALE QUOTE IS NOT A CLOSE
//
// When a market suspends, the books stop moving it. The last observation before
// kickoff may then be a frozen price from an hour earlier that nobody could have
// bet, and scoring a wager against it measures nothing.
//
// So a candidate quote is EXCLUDED when it falls inside a suspension episode:
//
//	suspended_at ≤ observed_at < COALESCE(lifted_at, +∞)
//
// Half-open at both ends, deliberately. A quote observed at the exact instant a
// suspension is LIFTED counts; one observed at the exact instant it BEGINS does
// not. `markets.status` is NOT consulted: it is current state and says nothing
// about what was true at the closing instant, whereas market_suspensions is the
// episode history.
//
// The two cases the rule has to get right, and how it does so with no special
// casing at all:
//
//   - SUSPENDED AND REOPENED BEFORE THE START. Quotes inside the episode are
//     excluded, quotes after the lift are eligible, and the last eligible one
//     wins. This is the ordinary shape and it needs no branch.
//   - SUSPENDED AND NEVER REOPENED. The episode has lifted_at IS NULL, so every
//     quote from suspended_at onward is excluded and the walk falls back to the
//     last quote before the suspension began — correctly, the last price at which
//     the market was actually open. If THAT quote is older than ClosingLookback,
//     the selection yields nothing, the snapshot is incomplete, and there is no
//     close. Also correct: a market that shut an hour before kickoff and never
//     reopened did not close, it stopped.
//
// # 5. AN IN-PLAY WAGER HAS NO CLV UNDER THIS DEFINITION, AND THAT IS DELIBERATE
//
// A bet struck after kickoff has a placement instant AFTER the closing instant.
// odds.EvaluateCLV then returns ErrCLVClosingBeforeTaken, and this package
// reports [ReasonCloseBeforeTake] and writes no row.
//
// This is not a gap to be patched later with a different closing rule. Closing
// line value is a claim about the PRE-GAME market's final estimate; an in-play
// price answers a different question — it is conditioned on a scoreline and a
// clock — and the pre-game close is not the right comparison for it. Measuring
// one against the other would produce a number that looks like CLV, ranks like
// CLV, and means something else. A live-wager analogue exists in the literature
// and would need its own definition, its own column set and its own ADR.
//
// The consequence to expect in the metrics: on a system with live betting
// enabled, a visible share of graded legs is unmeasurable for this reason and
// that share is NOT a defect. It is counted separately for exactly that purpose.
//
// # 6. THE TAKEN SNAPSHOT IS BUILT BY THE SAME RULE
//
// A leg stores its own price and nothing else, and devigging needs the whole
// outcome set — so the market as it stood when the leg was booked has to be
// reconstructed from `prices` too. It is reconstructed with the SAME statement,
// at the leg's own book, with the leg's own price_observed_at as the closing
// instant and TakenLookback as the lower bound. One rule for both sides is what
// makes the two snapshots odds.EvaluateCLV compares comparable in the first
// place; two rules would eventually differ in a way that shows up as a
// systematic bias rather than as an error.
//
// Applying the suspension exclusion to the taken side is harmless — a wager
// cannot be placed into a suspended market — and keeping it means there is
// literally one query.
//
// The taken snapshot carries one EXTRA requirement the closing snapshot does not:
// the quote it finds for the leg's OWN selection must be the leg's own quote,
// i.e. its observed_at must equal the leg's price_observed_at exactly.
// prices_natural_key_idx is UNIQUE on (selection_id, book_id, observed_at), so
// that equality identifies the row uniquely and therefore pins the price too. A
// mismatch means the reconstruction found a DIFFERENT quote from the one the
// customer was sold, which is a reconstruction that describes a market the wager
// was not struck in. It produces [ReasonTakenQuoteMismatch] and no row.
//
// # 7. EVERY SELECTION MUST AGREE ON THE LINE
//
// Change detection hashes a whole normalised market (CLAUDE.md §5), so a book's
// selections are written together and normally share an instant. Normally is not
// always: a snapshot assembled per selection can end up holding the home side at
// −3.5 and the away side at +3 if one of them has no eligible quote at the newer
// instant.
//
// That is not a market. Every quote is converted into the MARKET's frame — the
// away side of a spread is inverted, per domain.EffectiveLine's convention, and
// nothing else is — and all of them must then be equal under domain.Line.Equal,
// which distinguishes an absent line from a pick'em of 0.0. A snapshot that fails
// this is [ReasonTakenIncoherent] or [ReasonClosingIncoherent] and produces no
// row.
//
// This is a strengthening of "the snapshot was incomplete" rather than a new
// category: an incoherent snapshot is one whose selections are not all describing
// the same market question, which is the same defect an absent selection has.
//
// # 8. THE SNAPSHOT'S INSTANT IS ITS NEWEST QUOTE
//
// A snapshot is assembled from up to n quotes with up to n distinct instants, and
// odds.FairMarketSnapshot needs ONE. It is the MAXIMUM of the quotes' observed_at
// — the instant at which every part of the snapshot was simultaneously true —
// and never the closing instant itself, because the closing instant is a value of
// ours (a scheduled start) rather than a provider observation, and
// migrations/00009 requires taken_at and closed_at to be provider instants off
// one clock so that `closed_at >= taken_at` can be a database constraint.
//
// On the taken side the maximum is always the leg's own price_observed_at, since
// no quote may exceed the as-of bound and the leg's own quote sits exactly on it.
//
// =============================================================================
// WHICH CASES PRODUCE WHAT
// =============================================================================
//
// migrations/00009 states the rule this package implements: ABSENCE IS
// MEANINGFUL. "We could not measure it" and "it measured zero" must not share a
// shape, or a leaderboard cannot tell them apart. So a leg either gets a complete
// row or gets no row, never a row of nulls.
//
//	CASE                                        RESULT
//	------------------------------------------  ------------------------------
//	Everything present, same line               A ROW. Ranked.
//	Line moved between take and close           A ROW, line_moved = true.
//	                                            Served for display; EXCLUDED
//	                                            from every aggregate by
//	                                            odds.AggregateCLV and by the
//	                                            leaderboard query.
//	Leg voided (market cancelled, event          A ROW, voided = true. Also
//	abandoned)                                  excluded from aggregates. A
//	                                            PUSH is NOT void and IS ranked.
//	Market never traded at the taken line        NO ROW — the taken snapshot is
//	before the close                            what the taken line comes from,
//	                                            so this case is either a line
//	                                            move (a row) or an incomplete
//	                                            taken snapshot (no row). There
//	                                            is no third state.
//	Closing snapshot incomplete                  NO ROW. [ReasonClosingIncomplete]
//	Taken snapshot cannot be reconstructed       NO ROW. [ReasonTakenIncomplete]
//	Reconstruction found a different quote for   NO ROW.
//	the leg's own selection                     [ReasonTakenQuoteMismatch]
//	Snapshot lines disagree                      NO ROW. [ReasonTakenIncoherent]
//	                                            or [ReasonClosingIncoherent]
//	Outcome set changed between take and close   NO ROW. [ReasonOutcomeSetChanged]
//	Close would precede the take (in-play)       NO ROW. [ReasonCloseBeforeTake]
//	Every candidate close was inside a           NO ROW. Surfaces as
//	suspension                                  [ReasonClosingIncomplete], because
//	                                            the excluded quotes leave
//	                                            selections unpriced.
//	Market has no usable closing instant         NO ROW. [ReasonNoClose]
//	Neither devig method could price a side      NO ROW. [ReasonNotDevigable]
//
// The one row that exists for a leg whose MARKET moved its line is the reason
// EvaluateCLVAcrossLineMove exists at all: the user is shown "you took −3, it
// closed −3.5" and the number beside it, and nobody is ranked by it.
//
// # A note on "the market never traded at the taken line"
//
// It reads like a distinct case and it is not, which is worth stating because a
// reimplementation will look for it. The taken line is not supplied from outside
// — it is READ OFF the taken snapshot, which is the market as it stood when the
// leg was booked. So either that snapshot exists (and the taken line is whatever
// it says, by construction) or it does not (and there is no measurement). What
// varies is whether the CLOSING snapshot's line matches, and that is the
// line-move case above.
//
// =============================================================================
// THE DEVIG
// =============================================================================
//
// One method devigs BOTH sides. migrations/00009 gives wager_leg_clv a single
// devig_method column for this reason: "comparing a Shin-devigged take against a
// multiplicatively devigged close measures the difference between two devig
// methods, not closing line value."
//
// The configured method is tried on both sides first. If EITHER side refuses it,
// BOTH are recomputed with multiplicative and the row records `multiplicative`.
// Falling back on only the failing side would break the single-method rule in the
// one situation where it matters most. Multiplicative is the fallback for the
// reason internal/pricing/fairvalue.go gives: it is total, so a market it also
// refuses is a market whose prices are not a market.
//
// The quotes are devigged in SELECTION-ID ORDER on both sides. The four methods
// are order-independent in exact arithmetic; they are not bit-independent in
// float64, and a fixed order is what makes a replay reproduce a stored row.
//
// =============================================================================
// PURITY AND CLOCKS
// =============================================================================
//
// [Measurer.Measure] performs exactly two reads, both through [Store], and reads
// no clock except to stamp [Measurement.ComputedAt] — which is recorded, never
// keyed, and never compared. Every instant that decides anything comes from the
// provider: the leg's price_observed_at, the event's scheduled_start, and the
// quotes' own observed_at. That is what makes a replay of this package against
// unchanged data produce byte-identical rows, which is the property phase 12's
// validation depends on.
package clv
