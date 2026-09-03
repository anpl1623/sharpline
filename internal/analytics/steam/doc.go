// Package steam detects a steam move: a correlated line move that one book takes
// first and other books follow.
//
// It is the ONE genuinely new detector in phase 9. Everything else in
// internal/analytics is a surface over arithmetic internal/pricing and
// internal/domain/odds already do; this computes something no earlier phase
// computed, and CLAUDE.md §3 specifies its shape in one sentence:
//
//	"Steam detection — hopping window over line-movement velocity, keyed by
//	 market, across books."
//
// Phase 12 rewrites this as a Flink SQL job and checks it against this
// implementation — "same inputs, same outputs, or the Flink job is wrong". Every
// parameter below is therefore a contract term rather than a tuning knob, and
// the whole of this document is written so that someone with the prose and no Go
// could write the SQL and get the same answers.
//
// # THE UNIT IS IMPLIED PROBABILITY, NEVER DECIMAL ODDS
//
// This is the first decision and it is not negotiable. Decimal odds are
// nonlinear in probability: a 0.10 move in decimal price is
//
//	d = 1.50 → 1.60   0.6667 → 0.6250   0.042 probability points
//	d = 3.00 → 3.10   0.3333 → 0.3226   0.011 probability points
//	d = 10.0 → 10.1   0.1000 → 0.0990   0.001 probability points
//
// so a single decimal threshold means forty times as much on a favourite as on a
// longshot, and a board scanned with one would report steam on every short price
// and none on any long one. Probability is the quantity a line move is ABOUT —
// the market changed its mind by this much — and it is additive, so a delta and a
// velocity in probability points mean the same thing everywhere on the board.
//
// # THE LINE IS NOT PART OF THE SERIES
//
// An observation is (selection, book, implied, observed_at). The LINE the quote
// was made at is deliberately absent, so one series per (market, selection, book)
// tracks the probability that book was quoting AT WHATEVER HANDICAP IT WAS
// QUOTING AT THE TIME. A book moving home -1 @ 2.10 to home -0.5 @ 1.556
// contributes the same +0.167 delta as a book moving home -1 @ 2.10 to
// home -1 @ 1.556.
//
// That follows the charter rather than working around it: CLAUDE.md §3 asks for
// "line-movement velocity", and a handicap moving from -1 to -0.5 at every book
// inside ninety seconds is the canonical steam move. A detector that segmented
// its series by line would see that event as two unrelated one-observation series
// and report nothing.
//
// TWO CONSEQUENCES AN OPERATOR HAS TO KNOW, because neither is visible on the
// finding:
//
// MinMagnitude WAS CALIBRATED ON MONEYLINE MARKETS, where a line change is
// impossible, so the sweep below measured pure price movement. On a lined market a
// single half-point step is worth 0.10-0.17 probability points near the middle of
// the distribution — two to three times the threshold — so the bar is effectively
// met by any coordinated half-point move. Measured against the live synthetic feed
// over an hour of a four-league slate, 273 of 284 findings were windows in which
// the LEAD BOOK'S OWN LINE CHANGED (141/144 spreads, 132/132 totals, 0/8
// moneylines), and the detector fired on 1.55% of evaluated windows against the
// 0.016%-0.225% the moneyline rig reports.
//
// THE FINDING IS NOT SELF-DESCRIBING. steam_signals has no line column and no flag
// saying the window spanned a line change, so delta_probability cannot be read as
// "the price moved by this much" on a lined market: most of it is usually the
// handicap. Separating the two populations means going back to the prices
// hypertable.
//
// The follow-up is recorded in docs/analytics-semantics.md §6.2b rather than
// performed here: a lead_line_changed column on the table and a PER-MARKET-TYPE
// MinMagnitude, calibrated on lined markets with the same sweep. Both are
// additive. Neither is in phase 9, and phase 12 must reproduce the CURRENT
// behaviour — no segmentation by line — or its answers will be almost disjoint
// from this one's.
//
// # Raw implied probability, not devigged: devig_method is "none"
//
// The detector works on 1/d as the book quoted it, margin included, and writes
// "none" into the finding's devig_method. That is a deliberate choice with a
// cost, and both halves are worth stating.
//
// The cost: a book's margin is inside every observation, so a book that WIDENED
// its market without changing its opinion shows a move. In practice a book's
// overround is close to constant over a three-minute window — it is a business
// setting, not a price — so the margin cancels almost exactly in a DELTA, which
// is the only thing this detector looks at.
//
// The benefit is decisive: devigging requires a book's COMPLETE outcome set at
// one instant ([odds.NewFairMarketSnapshot] refuses probabilities that do not
// sum sensibly), and a book that has refreshed one side of its market and not
// the other is EXACTLY the book whose lag carries the signal. Devigging would
// drop it from the window at the moment it became interesting, which is the
// opposite of what the detector is for.
//
// # The window: hopping, half-open, aligned to the Unix epoch
//
// A hopping (Flink: HOP, "sliding" in some dialects) window of length
// [Config.Window] advancing by [Config.Hop]. With the defaults — 180 seconds
// every 60 — each observation falls in three consecutive windows, so a move is
// seen against three different framings and the cooldown below is what stops it
// firing three times.
//
// Two properties fix the grid, and both exist so that two independent
// implementations agree on which observations belong to which window:
//
//   - HALF-OPEN, [start, end). An observation at exactly `end` belongs to the
//     NEXT window and never to this one. This is what makes consecutive windows a
//     partition rather than an overlapping cover with a double-counted boundary,
//     and it is what Flink's HOP does.
//   - ALIGNED TO THE UNIX EPOCH IN UTC. Window k is
//     [k·Hop, k·Hop + Window) counted in nanoseconds from 1970-01-01T00:00:00Z.
//     NOT aligned to the first observation this process happened to see, which
//     would make the answer depend on when the consumer started; not aligned to
//     local midnight, which would make it depend on a time zone. Flink aligns its
//     windows to the epoch too, so the two agree by construction rather than by
//     coincidence.
//
// # Closing a window: a watermark, not a timer
//
// Each market carries a WATERMARK: the greatest observation instant seen for
// that market so far, minus [Config.AllowedLateness], and MONOTONE NON-DECREASING
// — it never moves backwards, whatever order records arrive in. A window closes,
// and is evaluated exactly once, when the watermark reaches or passes its end.
//
// Nothing here reads a wall clock. That is the property that makes a replay
// produce the same findings as the original run: the same log in the same order
// yields the same watermark sequence, the same window closes and the same rows.
// A timer-based close would make the output depend on how fast the consumer was
// running, which would make the phase-12 comparison meaningless.
//
// ALLOWED LATENESS IS SIZED AGAINST THE FEED'S OWN LAG, and getting it wrong is
// the one way to make this detector silently blind. Books quote off lagged views
// — the synthetic generator's deepest book is 9 base steps, 90 seconds, behind —
// and each book stamps its quote with the instant of the view it is quoting. So
// a lagged book's observation covering event-time T does not ARRIVE until up to
// its lag plus one poll interval after the sharp book's. Close the window before
// then and the lagged books are simply absent from it, the correlation is
// computed over one book, and no finding is possible. [DefaultAllowedLateness] is
// 180 seconds: 90 for the deepest book lag, 90 for one live poll interval
// (ADR 0003).
//
// An observation older than the watermark is LATE: it is dropped, counted under
// [Stats.Late], and cannot resurrect a window that has already been evaluated.
// Admitting it would mean a window's answer depended on when the reader asked,
// which is the property the watermark exists to remove.
//
// # What is measured, per window, per SELECTION
//
// Steam is DIRECTIONAL, so the unit of detection is a selection and not a
// market. "The home side steamed" and "the away side steamed" are the same
// market and opposite findings; migrations/00009 keys steam_signals on
// (market_id, selection_id, window_start, window_end) for exactly this reason,
// and keying by market alone would drop half the findings.
//
// For one selection in one window, for each book with AT LEAST TWO observations
// inside it:
//
//	Δ_b        = p_last − p_first, by observation instant, in probability points
//	moved_at_b = the END instant of the largest single step in the direction of
//	             Δ_b; ties broken by the EARLIEST such instant
//
// Δ is first-to-last rather than a sum of absolute steps because the finding is
// about where the market ENDED UP: a book that moved out and back has not
// steamed. moved_at is an argmax over steps rather than the window's last
// instant because it is the propagation timing that distinguishes a lead from a
// follower, and the largest step is where the move actually happened. Both are
// single window functions in SQL (`first_value`/`last_value`, and `lag` inside an
// argmax), which is a requirement rather than a happy accident.
//
// The LEAD BOOK is the qualifying book — |Δ_b| at or above
// [Config.MinMagnitude] — whose moved_at is EARLIEST. Ties are broken by the
// greater |Δ|, then by the lexicographically smaller book identifier, which makes
// the choice total: two books cannot tie on all three because a book appears once
// per (selection, window).
//
// The FOLLOWERS are every other qualifying book whose Δ has the SAME SIGN as the
// lead's and whose lag — moved_at_b − moved_at_lead — lies in
// [0, Config.MaxFollowerLag]. A lag of exactly zero is legal and common: books
// that share a view of one latent process reprice on the same event-time grid,
// and simultaneity is corroboration rather than a disqualification. Followers are
// ordered by lag ascending and then by book identifier ascending, which is the
// order migrations/00009's JSONB column documents and which a database cannot
// enforce.
//
// The finding's DELTA IS THE LEAD BOOK'S, not an average across books. A steam
// move in progress has followers that have only partly repriced, so averaging
// would understate the move by however much of it has not propagated yet — and
// would understate it MOST at the moment the finding is most valuable. The lead
// is by definition the book that has already made the move.
//
//	velocity = Δ_lead / (Window in minutes)
//
// It is a WINDOW-AVERAGE RATE, not an instantaneous one, and it is the standard
// HOP formulation: last minus first over the window length. At a fixed window
// length it is a scalar multiple of the magnitude, so [Config.MinVelocity] and
// [Config.MinMagnitude] express the same constraint twice. Both are kept and both
// are stored on every finding, because they stop meaning the same thing the
// moment the window length changes — which is precisely when a stored population
// of findings would otherwise become uninterpretable.
//
// # Cross-book correlation, and what it does NOT do
//
//	correlation = mean over every book with ≥2 observations in the window of
//	                +1  if sign(Δ_b) = sign(Δ_lead) and |Δ_b| ≥ NoiseFloor
//	                −1  if sign(Δ_b) ≠ sign(Δ_lead) and |Δ_b| ≥ NoiseFloor
//	                 0  otherwise
//
// It is the mean signed agreement, in [−1, 1], and it is a single GROUP BY in
// SQL. The lead contributes +1 to its own statistic by construction, so a lone
// book scores 1/n where n is the number of books with data — which is why a small
// [Config.MinCorrelation] is still a real constraint when several books are
// quoting.
//
// BE HONEST ABOUT WHAT THIS DISCRIMINATES. On a feed where every book views one
// latent process — which is what the synthetic generator builds and what a real
// market approximately is — ORDINARY DRIFT IS ALSO CORRELATED ACROSS BOOKS,
// because the books are looking at the same thing. Correlation therefore does NOT
// separate steam from drift. What it separates is a genuine market move from ONE
// book's tick rounding or its persistent per-event bias moving alone, which is
// the commonest way to manufacture a phantom signal from a single soft book.
//
// THE MAGNITUDE THRESHOLD IS THE PRIMARY DISCRIMINATOR. A steam move in the
// generator is a discrete jump of 0.4σ to 1.5σ of the latent process landing
// inside one 10-second step, where ordinary drift is a mean-reverting mixture
// whose movement over a three-minute window is a fraction of that. Sweeping
// [DefaultMinMagnitude] over the generator's own output shows the two populations
// separating cleanly — a Gaussian collapse in the candidate count up to about
// five probability points, then a much flatter tail — and that knee is where the
// default sits. [DefaultMinMagnitude] records the measurement and the two
// consequences of expressing it in probability points rather than in latent
// sigma: recall is deliberately low, and sensitivity varies by about a factor of
// two across the four leagues because their conversion factors do.
//
// # The cooldown, and why it is measured in event time
//
// With a 180-second window hopping every 60, ONE jump appears in three
// consecutive windows. Propagation makes it worse: a follower that reprices 90
// seconds later extends the run. Without suppression, one steam move would emit
// four or five findings and the alerting surface would be useless.
//
// [Config.Cooldown] suppresses a finding for a (market, selection, direction)
// whose window end is within the cooldown of the last finding EMITTED for that
// triple. Keyed on direction as well as selection because a move out and a move
// back are two events and the second is not a duplicate of the first.
//
// IT IS MEASURED ON WINDOW END, WHICH IS EVENT TIME, NOT ON A CLOCK. A wall-clock
// cooldown would suppress different findings on a replay than on the original
// run, and the phase-12 comparison would fail for a reason that had nothing to do
// with the SQL.
//
// # Purity, state and bounds
//
// The detector is a pure function of the observation sequence it is given: no
// clock, no I/O, no randomness, no map-iteration order anywhere a result can
// reach. It DOES hold state — a bounded ring of recent observations per (market,
// selection, book), the per-market watermark, the last evaluated window end, and
// the cooldown table — because a window is by definition a statement about more
// than one record. All of it is bounded and prunable, and [Detector.Forget]
// releases a market's state when the market is tombstoned upstream.
//
// It is NOT safe for concurrent use. It is driven from one consumer's handler
// goroutine, records are delivered sequentially, and a mutex here would buy
// nothing but a false suggestion that a second caller is expected.
package steam
