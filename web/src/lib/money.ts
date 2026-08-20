/**
 * Money, in MINOR UNITS, end to end.
 *
 * CLAUDE.md §12: "All money and stake values are integer minor units. Floating
 * point never touches a balance." That rule is usually stated about the Go side,
 * but it is exactly as load-bearing here, and it is easier to break here: a
 * stake field is a `<input>` whose value is a string of major units, and the
 * obvious implementation — `Number(text) * 100` — is a floating-point multiply.
 * `Number('19.99') * 100` is `1998.9999999999998`, and `Math.round` on top of it
 * hides the defect for every value a developer happens to test.
 *
 * So the parse in this file NEVER multiplies a fraction. It splits the typed
 * string on the decimal point, pads the fractional part to exactly two digits as
 * TEXT, and does one integer multiply and one integer add. The output is an
 * integer number of minor units and nothing between the keystroke and the wire
 * is a float.
 *
 * The inverse direction is the same discipline in reverse: `formatMinor` divides
 * by integer division and takes a remainder, rather than dividing by 100 and
 * calling `toFixed`.
 *
 * # There is no currency symbol in this product, deliberately
 *
 * The API's `BalanceResponse.currency` is the literal string `"PLAY"` and the
 * OpenAPI document says why: "Labelling play money `USD` is the first step
 * toward a client treating it as money, which CLAUDE.md §0 forbids." A `$` in
 * front of a number does exactly that labelling, so nothing here emits one.
 * Amounts render as bare numerals; where a unit is needed, `PLAY` is the unit.
 *
 * # Every function here is pure and allocation-light
 *
 * A wager list renders dozens of amounts and the slip re-renders them on every
 * keystroke.
 */

/**
 * Minor units in one major unit.
 *
 * The API declares this as a CONSTANT `100` — `BalanceResponse.minor_units_per_major`
 * is typed `100`, not `number`, so the two cannot drift without a schema
 * regeneration failing to compile. It is restated here because parsing a typed
 * stake happens long before any balance has been fetched.
 */
export const MINOR_UNITS_PER_MAJOR = 100;

/** How many fractional digits a major unit has. `log10(MINOR_UNITS_PER_MAJOR)`. */
export const MINOR_DIGITS = 2;

/**
 * The largest stake this client will construct.
 *
 * Not a business rule — the server owns those — but a guard against a paste of
 * forty digits producing a value beyond `Number.MAX_SAFE_INTEGER`, where integer
 * arithmetic silently stops being exact and the "no floats touch a balance"
 * property quietly stops holding. A billion play units is far above anything the
 * ledger will see and far below 2^53.
 */
export const MAX_STAKE_MINOR = 1_000_000_000 * MINOR_UNITS_PER_MAJOR;

/** The unit's name, for a label or an accessible name. Never a symbol. */
export const MONEY_UNIT = 'PLAY';

/**
 * Whether a value is a usable amount of minor units: a finite, safe integer.
 *
 * Non-integers are rejected rather than rounded. A fractional minor unit is a
 * float that got into a money path, and rounding it here would erase the
 * evidence at the one point that could still report it.
 */
export function isMoneyMinor(value: number): boolean {
  return Number.isSafeInteger(value);
}

/**
 * Minor units as a plain decimal string: `1250` -> `"12.50"`.
 *
 * Integer division and remainder, never `value / 100`. The sign is taken off
 * first so that `-5` renders `"-0.05"` rather than `"-0.-5"`, which is what
 * naive `%` on a negative gives.
 */
export function formatMinor(minor: number): string {
  if (!Number.isFinite(minor)) return '—';
  const rounded = Math.trunc(minor);
  const negative = rounded < 0;
  const magnitude = Math.abs(rounded);
  const whole = Math.floor(magnitude / MINOR_UNITS_PER_MAJOR);
  const fraction = magnitude - whole * MINOR_UNITS_PER_MAJOR;
  const rendered = `${groupThousands(whole)}.${String(fraction).padStart(MINOR_DIGITS, '0')}`;
  return negative ? `-${rendered}` : rendered;
}

/**
 * The same, with an explicit `+` on a positive value.
 *
 * For net return and profit, where the sign IS the information: "3.40" and
 * "-3.40" beside each other in a list are two very different outcomes and the
 * unsigned positive reads as neutral.
 */
export function formatMinorSigned(minor: number): string {
  if (!Number.isFinite(minor)) return '—';
  const rendered = formatMinor(minor);
  return minor > 0 ? `+${rendered}` : rendered;
}

/**
 * Thousands separators, hand-written rather than through `Intl.NumberFormat`.
 *
 * The same reason `formatLineNumber` avoids it: these strings are produced
 * during server rendering and again during hydration, and a locale-sensitive
 * formatter can put a `.` where the server put a `,` — which for money is not a
 * cosmetic mismatch, it is a factor of a thousand rendered differently on two
 * paints of the same number.
 */
function groupThousands(whole: number): string {
  const digits = String(whole);
  if (digits.length <= 3) return digits;
  let out = '';
  for (let index = digits.length; index > 0; index -= 3) {
    const start = Math.max(0, index - 3);
    const chunk = digits.slice(start, index);
    out = out === '' ? chunk : `${chunk},${out}`;
  }
  return out;
}

/**
 * What a money `<input>` shows for a stored amount: `1250` -> `"12.50"`, `0` ->
 * `""`.
 *
 * Zero renders as EMPTY rather than as `"0.00"`. A stake field pre-filled with
 * zero is a field the user has to clear before typing, and an empty stake and a
 * stake of nothing are the same fact here.
 *
 * No thousands separators: a grouped value cannot be typed back in, and the
 * field's own text has to survive a round trip through `parseMinor`.
 */
export function formatMinorForInput(minor: number): string {
  if (minor === 0) return '';
  const magnitude = Math.abs(Math.trunc(minor));
  const whole = Math.floor(magnitude / MINOR_UNITS_PER_MAJOR);
  const fraction = magnitude - whole * MINOR_UNITS_PER_MAJOR;
  const rendered = `${String(whole)}.${String(fraction).padStart(MINOR_DIGITS, '0')}`;
  return minor < 0 ? `-${rendered}` : rendered;
}

/** What `parseMinor` accepts: digits, at most one point, at most two decimals. */
const STAKE_PATTERN = /^(\d*)(?:\.(\d{0,2}))?$/;

/**
 * The result of parsing a typed amount.
 *
 * `null` for `minor` means the text is not a number yet — an empty field, or a
 * lone `"."` mid-typing. That is DIFFERENT from a rejected keystroke, which
 * returns `accepted: false` and leaves the field's value alone.
 */
export interface ParsedStake {
  /** True when the text is something this field is willing to hold. */
  readonly accepted: boolean;
  /** The amount in minor units, or null when the text is not yet a number. */
  readonly minor: number | null;
}

const REJECTED: ParsedStake = { accepted: false, minor: null };

/**
 * Parses a typed major-unit amount into minor units WITHOUT a floating-point
 * multiply. See the file comment — this is the whole reason this module exists.
 *
 * The text is matched against `STAKE_PATTERN`, which admits at most two
 * fractional digits. A third one is REJECTED rather than truncated: truncating
 * would silently change the amount the user believes they typed, and rejecting
 * makes the field behave the way every money field does — the keystroke simply
 * does not land.
 *
 * A partial value mid-typing (`""`, `"12."`, `"."`) is ACCEPTED with a null
 * amount, so the field does not fight the user between the point and the cents.
 */
export function parseMinor(text: string): ParsedStake {
  const trimmed = text.trim();
  if (trimmed === '') return { accepted: true, minor: null };

  const match = STAKE_PATTERN.exec(trimmed);
  if (match === null) return REJECTED;

  const wholeText = match[1] ?? '';
  const fractionText = match[2];

  // A lone "." or ".5" with nothing before it: a legal thing to be typing, not
  // yet a legal thing to stake.
  if (wholeText === '' && (fractionText === undefined || fractionText === '')) {
    return { accepted: true, minor: null };
  }

  // Guard before the multiply, not after: `Number` on a forty-digit string is
  // already imprecise, so the length has to be rejected on the TEXT.
  if (wholeText.length > 12) return REJECTED;

  const whole = wholeText === '' ? 0 : Number(wholeText);
  const fraction =
    fractionText === undefined || fractionText === ''
      ? 0
      : Number(fractionText.padEnd(MINOR_DIGITS, '0'));

  const minor = whole * MINOR_UNITS_PER_MAJOR + fraction;
  if (!Number.isSafeInteger(minor) || minor > MAX_STAKE_MINOR) return REJECTED;

  return { accepted: true, minor };
}

/**
 * An amount spoken for a screen reader: `"12.50 play"`.
 *
 * Rendered separately from the visual string because the visual one is a bare
 * numeral with no unit — correct on a screen where the column heading says
 * "Stake", and ambiguous when read out of that context.
 */
export function spokenMinor(minor: number): string {
  return `${formatMinor(minor)} ${MONEY_UNIT.toLowerCase()}`;
}
