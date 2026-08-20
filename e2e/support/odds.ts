// ---------------------------------------------------------------------------
// Reading a rendered price off the board.
// ---------------------------------------------------------------------------
// The suite never invents a price; it reads whatever the pipeline put on screen
// and asserts that it is *shaped* like a price in one of the three display
// formats web/src/lib/odds/format.ts can produce. That is the strongest
// assertion available without mock data: the value came from the provider, and
// this file only checks that it survived the journey as a number a bettor could
// read.
//
// Two hazards this file exists to absorb:
//
//  1. The digit roll renders BOTH the outgoing and incoming numeral for 180ms
//     (globals.css `.digit-roll-previous` is absolutely positioned and
//     aria-hidden, `.digit-roll-current` is the real one). Reading the cell's
//     text mid-roll yields "+118+124". We therefore read the current numeral
//     specifically when the roll markup is present.
//
//  2. A board cell legitimately carries more than the price — a spread or total
//     line sits with it ("-4.5" above "-110"). So the check is "at least one
//     whitespace-separated token parses as odds", not "the whole cell parses".
// ---------------------------------------------------------------------------

import type { Locator } from '@playwright/test';

/** format.ts NO_PRICE — an em dash. A cell may legitimately render this. */
export const NO_PRICE = '—';

/** `formatAmericanInt`: '+150' / '-110'. Magnitude is always >= 100. */
const AMERICAN = /^[+-]\d{3,7}$/;

/** `renderDecimal`: '2.50'. Always positive, always >= 1. */
const DECIMAL = /^\d+(?:\.\d+)?$/;

/** `formatFraction`: '3/2'. */
const FRACTIONAL = /^\d+\/\d+$/;

export type PriceFormat = 'american' | 'decimal' | 'fractional';

/** Collapse whitespace so multi-line cell text compares predictably. */
export function normaliseText(raw: string | null): string {
  return (raw ?? '').replace(/\s+/gu, ' ').trim();
}

/** True when the token is the explicit "no price" glyph, or an ASCII stand-in. */
export function isNoPrice(token: string): boolean {
  return token === NO_PRICE || token === '–' || token === '-' || token === '';
}

/** Which display format the token is, or null if it is not a price at all. */
export function priceFormatOf(token: string): PriceFormat | null {
  if (AMERICAN.test(token)) return 'american';
  if (FRACTIONAL.test(token)) return 'fractional';
  if (DECIMAL.test(token)) {
    // A decimal price is >= 1.0 by definition (stake included). Reject bare
    // integers that are obviously a line or a clock rather than a price.
    const value = Number.parseFloat(token);
    return Number.isFinite(value) && value >= 1 ? 'decimal' : null;
  }
  return null;
}

/** True if any token inside the text parses as odds in any display format. */
export function containsRenderedPrice(text: string): boolean {
  return tokensOf(text).some((token) => priceFormatOf(token) !== null);
}

/** The display formats present in the text, deduplicated. */
export function priceFormatsIn(text: string): PriceFormat[] {
  const seen = new Set<PriceFormat>();
  for (const token of tokensOf(text)) {
    const format = priceFormatOf(token);
    if (format !== null) seen.add(format);
  }
  return [...seen];
}

function tokensOf(text: string): string[] {
  return normaliseText(text)
    .split(/[\s,]+/u)
    .filter((token) => token.length > 0);
}

/**
 * The numeral text of one price cell.
 *
 * Preference order, most specific first:
 *   [data-testid="price-value"]  — the numeral, if the board labels it
 *   .digit-roll-current          — globals.css contract; excludes the outgoing
 *                                  numeral during a roll
 *   the cell itself              — line + price together, which the token-wise
 *                                  parser above handles
 */
export async function priceTextOf(cell: Locator): Promise<string> {
  const value = cell.locator('[data-testid="price-value"]');
  if ((await value.count()) > 0) {
    return normaliseText(await value.first().innerText());
  }
  const current = cell.locator('.digit-roll-current');
  if ((await current.count()) > 0) {
    return normaliseText(await current.first().innerText());
  }
  return normaliseText(await cell.innerText());
}

/** Read up to `limit` price cells, in document order. */
export async function readPriceTexts(cells: Locator, limit: number): Promise<string[]> {
  const all = await cells.all();
  const sample = all.slice(0, limit);
  const out: string[] = [];
  for (const cell of sample) {
    out.push(await priceTextOf(cell));
  }
  return out;
}
