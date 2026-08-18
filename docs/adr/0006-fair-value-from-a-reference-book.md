# ADR 0006: Fair value comes from one sharp reference book, devigged with Shin

- **Status:** Accepted
- **Date:** 2026-08-18
- **Charter reference:** CLAUDE.md §3, §4, §6

## Context

Phase 4 builds the pricing engine. Every downstream analytics claim the charter calls the
project's differentiator — the positive-EV finder, Kelly sizing, arbitrage detection, CLV —
rests on one number per selection: the **no-vig fair probability**. Two choices decide what
that number means, and both are the kind that get made implicitly and then argued about for
ever.

**First: whose prices does the fair value come from?** A market on this system is quoted by
several books at once. The tempting answer is to devig the consensus — average the books,
remove the margin from the average — because it uses all the information and never has a
missing input.

**Second: which margin model removes the vig?** Phase 1 shipped four devig methods behind a
runtime-selectable enum precisely because they disagree, and CLAUDE.md §4 says they
"disagree meaningfully on longshots". A market quoted at 1.91 / 1.91 devigs identically
under all four. A market quoted at 1.10 / 8.00 does not, and the disagreement is largest
exactly where a +EV finder is most likely to report an edge. Something has to pick, and a
pick made by whichever constant a call site happened to pass is not a decision.

A third question follows from the first two and is easy to miss: a consumer reading a
computed price cannot tell a deliberate trading judgement from a configuration default
unless the record says which happened.

## Decision

**A market's fair value is derived from ONE designated sharp reference book, never from a
consensus. A market with no eligible reference book is refused and publishes no fair value
at all.**

The reference is resolved by a ranked walk: books the provider designated on the wire
(`normalizer.BookRef.Reference`, from the adapter's own catalogue) first, then position in
`SHARPLINE_PRICER_REFERENCE_BOOKS`, and the first *eligible* candidate wins — eligible
meaning it quoted every selection and its oldest quote is within `MaxReferenceAge`. Walking
past an ineligible top choice rather than collapsing to "no reference" is deliberate: on a
real provider the first-choice book does not quote most props, and collapsing would blank
the +EV surface for all of them.

**`ReferenceSource` — `catalogue` or `configured` — is on every published record**, and is
counted as `sharpline_pricer_reference_book_total{source}`. The zero value refuses to
serialise, so a record cannot claim a provenance it does not have.

**The default devig method is Shin**, recorded on every record as `fair.method`, alongside
`fair.requested_method`, a `fallback` flag, and `fair.disagreement` — the spread across
every method that could price the same market.

**The single fallback is multiplicative, and it is never silent.** If the configured method
refuses a market, the engine falls back to multiplicative, records that it did, and counts
`sharpline_pricer_devig_fallback_total{from,to}`.

## Consequences

**What this makes easy.** The published fair value has a stated author. "This price is 3%
above Pinnacle's no-vig line" is a claim a reader can check and act on; "this price is 3%
above the average of five books including itself" is not, because the book being measured
contaminates the measurement. Kelly, EV and CLV inherit that interpretability for free. The
reference book also scores *itself* on every record, which gives the cheapest possible
self-check on the devig: a book cannot have an edge against probabilities extracted from
its own prices, so its EV must be ≤ 0 and its Kelly exactly 0 — asserted in tests across all
four methods and observable live on any published record.

**What this makes hard, and the cost accepted.** A market the reference book does not quote,
or quotes stale, or quotes incompletely, has **no fair value and no +EV surface at all**,
even when four other books are quoting it happily. That is a real and visible loss of
coverage, and it is the price of the claim above. It is made visible rather than papered
over: `sharpline_pricer_fair_value_total{result}` separates `no_reference`,
`reference_stale` and `reference_incomplete`, so the coverage cost is a number on a
dashboard rather than a silent gap.

The system is also now **dependent on one book's uptime** for a whole class of features.
The ranked list is the mitigation, not a cure.

**Shin's cost.** It is the most opinionated of the four and the most expensive: it solves
for an insider-money share `z` numerically. Neither cost is load-bearing at this scale —
the whole pricing pass is measured in tens of microseconds against a 250 ms p99 budget — but
the *opinion* is real. Shin shades longshots harder than multiplicative does, and on a
market with one big price the two produce materially different fair probabilities. That is
why `fair.disagreement` ships on every record: a fair probability with no error bar is an
opinion presented as a measurement, and this decision is meant to be revisable from
measurement rather than from taste.

## Alternatives considered

**Devig the consensus across all books.** Rejected on three independent grounds, any one of
which is sufficient. It averages the soft books' *errors* into the number those same errors
are supposed to be measured against. It includes the book being measured in the measurement,
so a book's edge against consensus shrinks as the book's own weight in the consensus grows —
an artefact, not a signal. And "consensus fair value" is a different quantity from "sharp
fair value" wearing the same name, so every downstream claim would quietly mean something
other than what it says. A consensus is a legitimate thing to compute; it is not this thing,
and conflating them is the failure this ADR exists to prevent.

**Multiplicative as the default devig.** Rejected because it is the crudest model and is
wrong in a known direction: it scales every implied probability by the same factor, which
under-shades longshots relative to observed markets. `internal/domain/odds/devig.go` calls
it "the worst possible silent default" in as many words. It is kept as the *explicit*
fallback for the one property Shin lacks — it is total, `q = p/S` cannot go negative and
needs no root-find — so a market multiplicative also refuses is not a market.

**Additive.** Rejected as a default because it is not total: it drives a long enough shot's
fair probability to zero or below and returns an error, so a board carrying one big price
loses its fair value entirely. A default that fails on the markets most worth pricing is
not a default.

**Power.** A genuinely close second and competitive on accuracy. It lost on interpretability:
it is a one-parameter fit whose exponent has no story about what it represents, where Shin's
`z` is an insider-money share that can be argued about, audited, and compared against
observed hold. Note that it is nonetheless the *correct* choice against the synthetic
provider specifically — that generator quotes three-way markets by applying a power margin
by construction, so `MethodPower` recovers its latent probabilities exactly (to ~10⁻¹⁵ in
the known-answer test). Making the method configurable and recording it is what lets both
statements be true at once.

**Hard-coding the reference book in the binary.** Rejected: sharpness is an opinion, not a
fact any provider reports, and The Odds API publishes no such label. A binary that could not
be told which book to trust would compile a trading judgement into an image.

**Requiring the reference book to be configured (no catalogue designation).** Rejected
because it duplicates a fact the provider layer already knows, and two sources of one truth
drift. The designation now travels the whole path — adapter → `RawBook.Reference` → mapper →
`BookRef` → `books.is_reference` → the engine — and outranks the configured list wherever it
exists. The configured list remains, as the answer for a provider that designates nothing.
