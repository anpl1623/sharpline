/**
 * Handicap and total formatting.
 *
 * A line is a number on the wire (`number | null`, from `domain.Line`, which
 * marshals `null` for "this market has no line") and a string on screen, and the
 * translation is not obvious:
 *
 *   - A SPREAD is signed and the sign is the whole meaning: "-4.5" and "+4.5" are
 *     the two sides of the same market. It is always rendered with an explicit
 *     sign.
 *   - A TOTAL is unsigned; the over/under role carries the direction instead.
 *   - A MONEYLINE and a FUTURES market have NO line, and `null` there is correct
 *     rather than missing.
 *
 * The API is careful about whose line a number is: `markets.line` is the
 * market's CURRENT consensus line, while a price's `line` is the line THAT QUOTE
 * WAS MADE AT, from that selection's own perspective. They legitimately differ,
 * and a comparison grid that renders one where it means the other shows a price
 * against the wrong handicap. Prefer the price's own line wherever one exists.
 */

import type { SchemaMarketType, SchemaSelectionRole } from '@/lib/api/schema';

/**
 * Rendered in place of a spread of exactly zero. "Pick 'em" is the category's
 * universal spelling for a spread with no handicap, and "+0" reads as a
 * rounding artefact rather than as a market. It is a constant so a design review
 * can change the spelling in one place.
 */
export const PICK_EM_LABEL = 'PK';

/** Rendered where a market carries no line at all. */
export const NO_LINE = '';

/**
 * Whether a market type carries a line. Moneyline and futures never do; spread
 * and total always do; a player prop usually does (a yardage or a made-shots
 * threshold) but is not required to.
 */
export function marketTypeHasLine(type: SchemaMarketType): boolean {
  switch (type) {
    case 'spread':
    case 'total':
      return true;
    case 'player_prop':
      return true;
    case 'moneyline':
    case 'futures':
      return false;
    default:
      return false;
  }
}

/**
 * A line as a bare number, with trailing zeros trimmed: 4.5 -> "4.5", 3 -> "3",
 * 2.25 -> "2.25".
 *
 * Hand-formatted rather than run through `Intl.NumberFormat` on purpose: this
 * string is rendered during server-side rendering and again during hydration,
 * and a locale-sensitive formatter can put a comma where the server put a point.
 * Lines are quoted at half and quarter points, so two decimal places is the
 * whole domain; anything finer is a data error and is shown as it arrives.
 */
export function formatLineNumber(line: number): string {
  if (!Number.isFinite(line)) return NO_LINE;
  const magnitude = Math.abs(line);
  const fixed = magnitude.toFixed(2);
  return fixed.replace(/\.?0+$/, '');
}

/**
 * A spread, with its explicit sign: "-4.5", "+3", "PK".
 *
 * The sign is never dropped. A spread cell reading "4.5" cannot be read at all —
 * it is the difference between laying and taking the points.
 */
export function formatHandicap(line: number | null | undefined): string {
  if (line === null || line === undefined || !Number.isFinite(line)) {
    return NO_LINE;
  }
  if (line === 0) return PICK_EM_LABEL;
  const magnitude = formatLineNumber(line);
  return line > 0 ? `+${magnitude}` : `-${magnitude}`;
}

/**
 * A total with its side: "O 54.5", "U 54.5". The role carries the direction, so
 * the number itself is unsigned.
 */
export function formatTotal(
  role: SchemaSelectionRole,
  line: number | null | undefined,
): string {
  if (line === null || line === undefined || !Number.isFinite(line)) {
    return NO_LINE;
  }
  const magnitude = formatLineNumber(line);
  switch (role) {
    case 'over':
      return `O ${magnitude}`;
    case 'under':
      return `U ${magnitude}`;
    default:
      return magnitude;
  }
}

/**
 * THE function a board cell calls: the line for one selection of one market,
 * rendered per that market's type.
 *
 * Returns the empty string where a market has no line, which is a correct answer
 * and not a missing one — a moneyline cell shows a price and nothing above it.
 */
export function formatLine(
  type: SchemaMarketType,
  role: SchemaSelectionRole,
  line: number | null | undefined,
): string {
  if (line === null || line === undefined || !Number.isFinite(line)) {
    return NO_LINE;
  }
  switch (type) {
    case 'spread':
      return formatHandicap(line);
    case 'total':
      return formatTotal(role, line);
    case 'player_prop':
      // A player prop is an over/under on a threshold when it has roles to
      // match, and a bare threshold otherwise.
      return role === 'over' || role === 'under'
        ? formatTotal(role, line)
        : formatLineNumber(line);
    case 'moneyline':
    case 'futures':
      return NO_LINE;
    default:
      return formatLineNumber(line);
  }
}

/**
 * The market's own name for a column heading or a market-tree row: "Spread",
 * "Total", "Moneyline". The line is NOT folded in here — a heading that says
 * "Spread -4.5" is wrong the moment the two sides sit at different numbers,
 * which is the normal state of a market being moved.
 */
export function marketTypeLabel(type: SchemaMarketType): string {
  switch (type) {
    case 'moneyline':
      return 'Moneyline';
    case 'spread':
      return 'Spread';
    case 'total':
      return 'Total';
    case 'player_prop':
      return 'Player prop';
    case 'futures':
      return 'Futures';
    default:
      return 'Market';
  }
}

/**
 * The short heading the dense board uses, where a column is 3 characters wide
 * before it starts pushing price cells around.
 */
export function marketTypeShortLabel(type: SchemaMarketType): string {
  switch (type) {
    case 'moneyline':
      return 'ML';
    case 'spread':
      return 'Spread';
    case 'total':
      return 'Total';
    case 'player_prop':
      return 'Prop';
    case 'futures':
      return 'Futures';
    default:
      return 'Market';
  }
}

/**
 * The role's own name, for an accessible name and for a market-tree label:
 * "Over", "Under", "Home", "Away", "Draw".
 *
 * A SELECTION's display name — the team, the player, the runner — is never
 * derived from a role. It arrives on the payload (`Selection.name` on REST,
 * `fair.selections[].name` on the stream) and is the only correct source.
 */
export function selectionRoleLabel(role: SchemaSelectionRole): string {
  switch (role) {
    case 'home':
      return 'Home';
    case 'away':
      return 'Away';
    case 'draw':
      return 'Draw';
    case 'over':
      return 'Over';
    case 'under':
      return 'Under';
    case 'outright':
      return 'Outright';
    default:
      return 'Selection';
  }
}
