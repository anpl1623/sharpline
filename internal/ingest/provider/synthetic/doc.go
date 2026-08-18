// Package synthetic is a stochastic market maker that implements
// provider.Adapter without touching the network.
//
// CLAUDE.md §5: "Build ProviderAdapter first, ship a synthetic provider that
// runs a stochastic market-maker generating realistic line movement, steam
// moves, and book disagreement. This unblocks the entire pipeline, makes tests
// deterministic with a seeded RNG, and means demos never burn API quota."
//
// It is the adapter `ingest` constructs when ODDS_API_KEY is unset, which is the
// default for a clone of this repository.
//
// # This is not mock data
//
// The distinction is the whole reason this package exists rather than a fixtures
// directory, so it is worth being exact about.
//
// Mock data is a stored answer: a JSON literal, a seeded database row, a canned
// HTTP response. Nothing here is stored. Every price this package returns is
// COMPUTED, on the call, from a latent probability process that has been
// evolving since the adapter was constructed, through a per-book view of that
// process, through a per-book overround, through a per-book quoting tick. Ask
// twice within one model step and you get the same price because the market has
// not moved; ask an hour later and the line is somewhere else because the
// process ran. That is a simulation, and simulations are how you test a pricing
// pipeline without a data bill.
//
// What it is NOT is a claim about any real contest. Every league, competitor and
// player named here is invented (see universe.go) precisely so that no output of
// this package can be mistaken for a real market, and every book it quotes is
// domain.BookKindSynthetic so that a +EV or arbitrage signal computed against it
// is identifiable as a statement about a random number generator. ADR 0003 is
// blunt about the line that must not be crossed: the synthetic fallback "must
// not silently substitute for real data in a running deployment — that would be
// indistinguishable from fabricating market data".
//
// # Determinism
//
// The contract is:
//
//	the same seed, the same construction instant, and the same sequence of clock
//	readings produce byte-identical snapshots, always.
//
// Two things follow from the wording, and both are deliberate.
//
// The model advances with TIME, not with polls. The state at a given instant is
// the same whether the scheduler asked once or a thousand times in between, and
// a poll that arrives inside the same model step returns the identical price.
// This is not a nicety: change detection (CLAUDE.md §5, "most polls return
// identical data and must not generate bus traffic") is only exercised — and can
// only be demonstrated to work — against a generator that does not manufacture a
// new number for every request. A per-call random walk would make every poll a
// change and the suppression rate would read 0% for ever.
//
// Advancement is path-independent. The noise driving step n of a given process
// is a counter-based hash of (seed, process key, n), not a draw from a
// sequential stream, so advancing ten steps one at a time and advancing ten
// steps in one go produce the same state. Without that property the output would
// depend on the polling schedule after all, just less obviously.
//
// A RESTART CHANGES NOTHING. The step index is measured from the Unix epoch, so
// the state at an instant is the same in every process and after every restart —
// stop the binary, start it again, and the board is where you left it. The
// adapter holds no model state for a restart to lose. (An earlier draft of this
// file described the opposite, a book re-opened from the stationary distribution
// at construction; that was abandoned because it makes the model depend on when
// the process started, which is the one input a seed is supposed to remove.)
//
// # The model
//
// Per event, two mean-reverting processes evolve: the expected home margin μ and
// the expected combined total τ. Mean reversion rather than a plain random walk
// is what keeps a line from wandering to nonsense over a day.
//
// They are written in MOVING-AVERAGE form rather than as a recursion — an AR(1)
// is identically the convolution of white noise with an exponential kernel, and
// only the convolution can be evaluated at an arbitrary step without iterating
// from an origin. noise.go carries the full argument, including why each
// component lives on its own coarse grid and is interpolated: a process sampled
// at every 10-second step moves too far per step for change detection to have
// anything to suppress.
//
// Every quoted probability is derived from those two numbers by the same normal
// model a trader would use — P(home wins) = Φ(μ/σ_result), P(home covers L) =
// Φ((μ+L)/σ_result), P(over T) = 1 − Φ((T−τ)/σ_total) — so the moneyline, the
// spread and the total on one event are mutually consistent rather than three
// independent walks. Φ is odds.NormalCDF; this package does not carry a second
// implementation of anything internal/domain/odds already has.
//
// For a live event the remaining game is what is priced: the final margin is the
// current lead plus μ·(1−f) with dispersion σ·√(1−f), where f is the fraction of
// the contest played. That is why an in-play line tightens as the clock runs, and
// why the score the event carries and the price on it cannot disagree.
//
// STEAM MOVES are a correlated jump: at a rare, hash-determined step the latent
// process takes a large step and its mean moves with it, so the repricing is
// permanent rather than something mean reversion pulls back. The market driven by
// that process is briefly suspended, which is what a book actually does.
//
// BOOK DISAGREEMENT is not injected as noise. Each book quotes off its own
// LAGGED VIEW of the latent process — book b sees μ as it was lag(b) steps ago —
// plus a small persistent per-(book, event) bias. Two consequences matter
// downstream: books disagree by a realistic amount at all times, and after a
// steam move they converge in lag order over the following seconds. That
// staggered convergence is the signal phase 9's steam detector keys on, so it is
// a property of the model rather than a flag on a message.
//
// VIG is applied per book, so the prices are genuinely overround and phase 4 has
// something real to devig. Two of the five books apply a multiplicative
// overround and three a power overround, which is deliberate: CLAUDE.md §4 notes
// the four devig methods "disagree meaningfully on longshots", and a slate where
// every book's vig was applied the same way would make that disagreement
// unobservable.
//
// QUOTING TICKS are the last step and the one that makes change detection real.
// Books quote in whole American units, so the fair decimal is converted to
// American, floored to the book's tick (1 for the sharp book, 5 or 10 for the
// rest), and converted back. Flooring rather than rounding is the direction that
// preserves the book's edge, which is both what a real book does and what
// guarantees the emitted market is overround by at least its configured margin.
// The visible effect is that a market's price changes only when the latent
// process moves far enough to cross a tick — which is exactly why most polls
// return identical data.
//
// # Quota
//
// The adapter carries a real token bucket, charged len(scope.Markets) credits
// per fetch to mirror The Odds API's markets × regions billing, and reports it
// through provider.Quota. It is not a fiction about a remote service: it is this
// adapter's own limiter, and exhausting it returns ErrQuotaExhausted like any
// other provider would. It exists so that the quota path — the gauge, the
// ProviderQuotaLow alert, the scheduler's backoff — is exercised on the offline
// path instead of only when someone pays for an API key. The default budget is
// far larger than any demo consumes.
package synthetic
