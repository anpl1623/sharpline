/**
 * Odds format conversion and rendering.
 *
 * This is a PORT of `internal/domain/odds` (convert.go and format.go). Every
 * exported function names the Go function it corresponds to. The rules, the
 * bounds, the rounding mode and the lossiness are reproduced exactly — a price
 * that renders "+150" from the API must render "+150" here, because the board is
 * fed from two transports (REST and WebSocket) and both carry only
 * `decimal_odds`. If the two conversions disagreed, the same price would render
 * differently depending on which frame delivered it.
 *
 * DECIMAL IS CANONICAL. American and fractional are LOSSY DISPLAY formats,
 * derived at the edge, rendered, and discarded. Nothing in this system stores or
 * computes with them. That is the phase-1 handoff rule and it is why the API's
 * `odds_format` query parameter is deliberately not used: it only adds a
 * `display` string to a REST payload, and the WebSocket carries no such string,
 * so relying on it would make REST-rendered and stream-rendered prices differ.
 *
 * Everything here is a pure function over numbers with no allocation beyond the
 * returned string. `formatOdds` runs once per price cell per tick on a board
 * that can hold hundreds of cells.
 */

import type { SchemaOddsFormat } from '@/lib/api/schema';

/**
 * The display formats. Aliased from the generated OpenAPI type so the toggle and
 * the API can never come to disagree about the spelling of a format.
 */
export type OddsFormat = SchemaOddsFormat;

/** A fractional price in lowest terms: stake `d` to win `n`. */
export interface Fraction {
  readonly n: number;
  readonly d: number;
}

// -----------------------------------------------------------------------------
// Bounds — odds/convert.go
// -----------------------------------------------------------------------------

/** `odds.MinAmericanMagnitude`. Nothing in (-100, +100) is a price. */
export const MIN_AMERICAN_MAGNITUDE = 100;

/** `odds.MaxAmericanMagnitude`. */
export const MAX_AMERICAN_MAGNITUDE = 1_000_000;

/** `odds.MaxFractionalDenominator`. Covers the whole conventional ladder. */
export const MAX_FRACTIONAL_DENOMINATOR = 1_000;

/** `odds.MaxFractionalNumerator`. */
export const MAX_FRACTIONAL_NUMERATOR = 1_000_000_000_000;

/** `odds.DefaultDecimalPlaces`. */
export const DEFAULT_DECIMAL_PLACES = 2;

/** `odds.DefaultProbabilityPlaces`. */
export const DEFAULT_PROBABILITY_PLACES = 2;

/** `odds.evenDecimal` — the decimal price at which the American sign flips. */
const EVEN_DECIMAL = 2;

/** `odds.maxContinuedFractionTerms`. */
const MAX_CONTINUED_FRACTION_TERMS = 64;

/** `odds.fractionalTolerance`. See FractionalApprox for why it is this loose. */
const FRACTIONAL_TOLERANCE = 1e-12;

/**
 * The glyph rendered where a price cannot be shown — an unpriceable value, or a
 * selection no book has quoted. It is an em dash, NOT a zero and NOT a blank:
 * "no price" and "a price of nothing" are different facts and only one of them
 * is true here.
 */
export const NO_PRICE = '—';

// -----------------------------------------------------------------------------
// Validation — odds.Decimal.Validate / odds.Probability.Validate
// -----------------------------------------------------------------------------

/**
 * `odds.Decimal.Valid`: a legal decimal price is finite and strictly greater
 * than 1. A return of 1.0 is a stake refunded, which is not a price.
 */
export function isPriceableDecimal(value: number): boolean {
  return Number.isFinite(value) && value > 1;
}

/** `odds.Probability.Valid`: finite and inside the closed interval [0, 1]. */
export function isValidProbability(value: number): boolean {
  return Number.isFinite(value) && value >= 0 && value <= 1;
}

// -----------------------------------------------------------------------------
// Decimal -> American — odds.Decimal.American
// -----------------------------------------------------------------------------

/**
 * Go's `math.Round`: round half AWAY FROM ZERO. `Math.round` rounds half toward
 * +Infinity, so `Math.round(-2.5)` is -2 where Go gives -3. Every American
 * conversion in this file goes through here; using `Math.round` directly would
 * put a one-unit disagreement with the server on exactly the ties.
 */
function roundHalfAwayFromZero(value: number): number {
  return value < 0 ? -Math.round(-value) : Math.round(value);
}

/**
 * `odds.American.Canonical`: fold the redundant -100 onto +100.
 *
 * American odds are ambiguous at even money — +100 and -100 both mean "stake one
 * to win one", decimal 2.0. Collapsing the pair is what makes the round trip a
 * total identity.
 */
function canonicalAmerican(value: number): number {
  return value === -MIN_AMERICAN_MAGNITUDE ? MIN_AMERICAN_MAGNITUDE : value;
}

/**
 * `odds.Decimal.American`. Converts decimal odds to the nearest American price.
 *
 *   d >= 2  ->  +round((d - 1) * 100)     the profit on a stake of 100
 *   d <  2  ->  -round(100 / (d - 1))     the stake that profits 100
 *
 * The conversion is LOSSY: American prices are integers, and the real-valued
 * result is rounded half away from zero. That rounding cannot land inside the
 * illegal (-100, +100) band — the first branch is bounded below by exactly 100,
 * and d - 1 < 1 forces 100/(d-1) > 100 on the second.
 *
 * Returns `null` where Go returns `ErrAmericanOutOfRange`: an unpriceable
 * decimal, or a price beyond `MAX_AMERICAN_MAGNITUDE`. A caller rendering a
 * format toggle falls back to another format rather than failing — see
 * `formatOdds`.
 */
export function toAmerican(decimal: number): number | null {
  if (!isPriceableDecimal(decimal)) return null;

  const exact =
    decimal >= EVEN_DECIMAL ? (decimal - 1) * 100 : -100 / (decimal - 1);
  if (!Number.isFinite(exact)) return null;

  const rounded = roundHalfAwayFromZero(exact);
  if (
    rounded > MAX_AMERICAN_MAGNITUDE ||
    rounded < -MAX_AMERICAN_MAGNITUDE
  ) {
    return null;
  }
  return canonicalAmerican(rounded);
}

/**
 * `odds.RenderAmerican`. Renders an integer American price with its EXPLICIT
 * sign: "+150", "-110", "+100".
 *
 * The leading "+" is not decoration. The American convention gives opposite
 * meanings to the two signs, so an unsigned "150" is ambiguous.
 */
export function formatAmericanInt(american: number): string {
  return american > 0 ? `+${String(american)}` : String(american);
}

/**
 * Decimal -> a signed American string, or `null` when the price has no American
 * form. Composition of `toAmerican` and `formatAmericanInt`.
 */
export function renderAmerican(decimal: number): string | null {
  const american = toAmerican(decimal);
  return american === null ? null : formatAmericanInt(american);
}

// -----------------------------------------------------------------------------
// Decimal -> Fractional — odds.Decimal.FractionalApprox
// -----------------------------------------------------------------------------

/**
 * `odds.bestRationalApproximation`. The simple continued fraction of x, expanded
 * term by term, accumulating convergents with the standard recurrence
 * p_k = a_k*p_{k-1} + p_{k-2}, q_k = a_k*q_{k-1} + q_{k-2}.
 *
 * The expansion stops at the first of: the convergent is within `tol` of x; the
 * next convergent would exceed the numerator or denominator bound; the
 * remainder reaches exactly zero (x is rational and fully expanded, which shows
 * up here as the next remainder being +Infinity); or the term cap.
 *
 * One deliberate difference from the Go original: Go's `safeMulAdd` guards
 * int64 overflow, and this uses `Number.isSafeInteger` — the float64 analogue.
 * The difference never binds, because `MAX_FRACTIONAL_NUMERATOR` (1e12) is four
 * orders of magnitude below 2^53 and the bound check fires first.
 */
function bestRationalApproximation(
  x: number,
  maxDen: number,
  maxNum: number,
  tol: number,
): Fraction {
  let best: Fraction = { n: 0, d: 1 };
  if (!Number.isFinite(x) || x < 0) return best;

  // p_{-2}, p_{-1} and q_{-2}, q_{-1} seed the convergent recurrence.
  let numPrev2 = 0;
  let numPrev1 = 1;
  let denPrev2 = 1;
  let denPrev1 = 0;

  let remainder = x;
  for (let i = 0; i < MAX_CONTINUED_FRACTION_TERMS; i += 1) {
    const term = Math.floor(remainder);
    // Also the termination for a completed expansion: when x is rational the
    // remainder lands on an exact integer, the fractional part is zero, and
    // 1/0 makes the next remainder +Infinity — which fails this bound on the
    // following pass with `best` already holding the exact convergent.
    if (!Number.isFinite(term) || term < 0 || term > maxNum) break;

    const num = term * numPrev1 + numPrev2;
    const den = term * denPrev1 + denPrev2;
    if (
      !Number.isSafeInteger(num) ||
      !Number.isSafeInteger(den) ||
      num > maxNum ||
      den > maxDen ||
      den <= 0
    ) {
      break;
    }

    best = { n: num, d: den };
    if (Math.abs(num / den - x) <= tol) break;

    numPrev2 = numPrev1;
    numPrev1 = num;
    denPrev2 = denPrev1;
    denPrev1 = den;
    remainder = 1 / (remainder - term);
  }

  return best;
}

/**
 * `odds.Decimal.Fractional`. The best rational approximation of d - 1, in lowest
 * terms.
 *
 * Exact for every price whose fraction in lowest terms has a denominator
 * <= 1000, which is the entire conventional betting ladder; an approximation
 * otherwise. Returns `null` for a price shorter than roughly 1/1000, which has
 * no fractional form at all (`ErrFractionalNotRepresentable`).
 *
 * The traditional un-reduced ladder spellings some books post — 6/4 for 3/2,
 * 4/6 for 2/3 — are a book-specific presentation choice, not a property of the
 * price, and are deliberately not reproduced. Snapping a computed price onto a
 * display ladder would misrepresent the number.
 */
export function toFractional(decimal: number): Fraction | null {
  if (!isPriceableDecimal(decimal)) return null;

  const fraction = bestRationalApproximation(
    decimal - 1,
    MAX_FRACTIONAL_DENOMINATOR,
    MAX_FRACTIONAL_NUMERATOR,
    FRACTIONAL_TOLERANCE,
  );
  if (fraction.n <= 0 || fraction.d <= 0) return null;
  return fraction;
}

/**
 * `odds.Decimal.FractionalApprox`. The fraction plus the absolute error
 * |n/d - (d-1)| of the approximation, so a caller can decide between "5/2" and
 * "~5/2". Returns `null` where `toFractional` does.
 */
export function toFractionalApprox(
  decimal: number,
): { readonly fraction: Fraction; readonly error: number } | null {
  const fraction = toFractional(decimal);
  if (fraction === null) return null;
  const error = Math.abs(fraction.n / fraction.d - (decimal - 1));
  return { fraction, error };
}

/** `odds.RenderFractional`. "5/2". */
export function formatFraction(fraction: Fraction): string {
  return `${String(fraction.n)}/${String(fraction.d)}`;
}

/** Decimal -> a fractional string, or `null` when it has no fractional form. */
export function renderFractional(decimal: number): string | null {
  const fraction = toFractional(decimal);
  return fraction === null ? null : formatFraction(fraction);
}

// -----------------------------------------------------------------------------
// Decimal / probability rendering — odds.RenderDecimal, odds.RenderProbability
// -----------------------------------------------------------------------------

/**
 * `odds.RenderDecimal`. Fixed-precision decimal odds: 1.909090... at two places
 * is "1.91". A negative `places` asks for the shortest string that round-trips,
 * which is the right choice for diagnostics where truncation would hide a
 * discrepancy.
 *
 * `toFixed` and Go's `strconv.FormatFloat` both operate on the exact binary
 * value and therefore agree everywhere except on an exact representable tie
 * (1.125 at two places), where V8 rounds half away from zero and Go rounds half
 * to even. Ties are not reachable from the pricer's output at two places, and
 * the disagreement is one unit in the last displayed digit if they ever are.
 */
export function renderDecimal(
  decimal: number,
  places: number = DEFAULT_DECIMAL_PLACES,
): string {
  if (!Number.isFinite(decimal)) return NO_PRICE;
  if (places < 0) return String(decimal);
  return decimal.toFixed(places);
}

/**
 * `odds.Decimal.Probability`. The IMPLIED probability p = 1/d.
 *
 * THIS IS NOT A FAIR PROBABILITY. It has the book's vig still in it, and across
 * a market these sum to more than 1 by exactly the overround. Never label it
 * "fair", "true" or "no-vig" in a UI. The de-vigged number is a different
 * quantity and travels on the WebSocket payload as `fair.selections[].probability`.
 *
 * Returns `null` for an unpriceable decimal rather than a nonsense probability.
 */
export function impliedProbability(decimal: number): number | null {
  if (!isPriceableDecimal(decimal)) return null;
  return 1 / decimal;
}

/** `odds.RenderProbability`. 0.5238095 at two places is "52.38%". */
export function renderProbability(
  probability: number,
  places: number = DEFAULT_PROBABILITY_PLACES,
): string {
  if (!Number.isFinite(probability)) return NO_PRICE;
  if (places < 0) return `${String(probability * 100)}%`;
  return `${(probability * 100).toFixed(places)}%`;
}

/**
 * A percentage rendered from a fraction, for EV%, edge and vig readouts that
 * already arrive as percentages or as fractions of 1. Kept here so every
 * percentage in the product rounds the same way.
 */
export function renderPercent(fraction: number, places = 1): string {
  if (!Number.isFinite(fraction)) return NO_PRICE;
  return `${(fraction * 100).toFixed(places)}%`;
}

/** A signed percentage, for EV% and CLV where the sign is the information. */
export function renderSignedPercent(fraction: number, places = 1): string {
  if (!Number.isFinite(fraction)) return NO_PRICE;
  const value = (fraction * 100).toFixed(places);
  return fraction > 0 ? `+${value}%` : `${value}%`;
}

// -----------------------------------------------------------------------------
// The toggle — odds.Render
// -----------------------------------------------------------------------------

/**
 * `odds.Render`. THE function a price cell calls.
 *
 * Takes the canonical decimal price and the user's chosen format and returns a
 * string. It NEVER throws and never returns an empty string: a format with no
 * representation for this price falls back to decimal (Go's Render documents
 * that a format toggle should handle that by falling back rather than failing),
 * and a value that is not a price at all renders as `NO_PRICE`.
 *
 * Pure, allocation-light, and safe to call on every tick.
 */
export function formatOdds(decimal: number, format: OddsFormat): string {
  if (!isPriceableDecimal(decimal)) return NO_PRICE;

  switch (format) {
    case 'american': {
      const rendered = renderAmerican(decimal);
      return rendered ?? renderDecimal(decimal);
    }
    case 'fractional': {
      const rendered = renderFractional(decimal);
      return rendered ?? renderDecimal(decimal);
    }
    case 'decimal':
      return renderDecimal(decimal);
    default:
      return renderDecimal(decimal);
  }
}

/**
 * The human name of a format, for the toggle's own label. Deliberately short —
 * the toggle sits in a header, not in a settings page.
 */
export function oddsFormatLabel(format: OddsFormat): string {
  switch (format) {
    case 'american':
      return 'American';
    case 'decimal':
      return 'Decimal';
    case 'fractional':
      return 'Fractional';
    default:
      return 'American';
  }
}

/**
 * `odds.ParseFormat`. Case-insensitive, whitespace-trimmed, with the same
 * regional aliases the Go package accepts. Returns `null` for an unrecognised
 * name; the empty string is NOT treated as a default, because a caller that
 * wants one should apply it explicitly.
 */
export function parseOddsFormat(value: string): OddsFormat | null {
  switch (value.trim().toLowerCase()) {
    case 'american':
    case 'us':
      return 'american';
    case 'decimal':
    case 'eu':
    case 'euro':
    case 'european':
      return 'decimal';
    case 'fractional':
    case 'fraction':
    case 'uk':
      return 'fractional';
    default:
      return null;
  }
}

/** The formats in the order the header toggle cycles them. */
export const ODDS_FORMATS: readonly OddsFormat[] = [
  'american',
  'decimal',
  'fractional',
];

/** The next format in `ODDS_FORMATS`, wrapping. Backs a single-button toggle. */
export function nextOddsFormat(format: OddsFormat): OddsFormat {
  const index = ODDS_FORMATS.indexOf(format);
  const next = ODDS_FORMATS[(index + 1) % ODDS_FORMATS.length];
  return next ?? 'american';
}
