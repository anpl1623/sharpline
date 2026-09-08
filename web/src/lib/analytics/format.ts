/**
 * Formatting for the analytics surface, and the ONE rule that governs it.
 *
 * # Percent is not always a fraction, and mixing the two is silent
 *
 * `@/lib/odds/format` has `renderPercent` and `renderSignedPercent`, and BOTH
 * TAKE A FRACTION — they multiply by 100. That is right for the values they were
 * written for: an implied probability, an overround, a devig excess.
 *
 * Half the numbers on the phase 9 surface are already IN PERCENT when they leave
 * the API. `EVSignal.expected_value_percent`, `edge_percent`,
 * `ArbitrageSignal.return_percent`, `CLVEntry.percent_clv`,
 * `LeaderboardEntry.roi_percent` are all percentages on the wire, because the
 * database stores them that way and because the detector's own threshold is
 * expressed in the same unit.
 *
 * Passing one of those to `renderPercent` renders 3.2% as "320.00%". It is a
 * plausible-looking number, it appears on a surface whose whole purpose is to be
 * trusted, and nothing anywhere reports it. So the two units get two functions
 * with two names, and neither file's helper is used for the other's values.
 *
 * # Probability points are a third thing again
 *
 * Steam is measured in IMPLIED PROBABILITY POINTS — `delta_probability` of
 * 0.021 is 2.1 points. It is not a percentage of anything: it is an additive
 * change in a probability, and rendering it with a `%` sign invites a reader to
 * think one book's price moved by 2%. `formatProbabilityPoints` renders it in
 * points with the unit in the label rather than in the numeral.
 *
 * # Everything here is deterministic across a server render and a hydration
 *
 * `toFixed` and string concatenation only — no `Intl.NumberFormat`, for the
 * reason `@/lib/time` and `@/lib/money` both give: an ambient locale can put a
 * `.` where the server put a `,`.
 */

/** What every formatter here renders for a value that is not a number. */
export const NO_VALUE = '—';

/**
 * A value that is ALREADY a percentage: `3.2` renders as `"3.20%"`.
 *
 * Never call `renderPercent` from `@/lib/odds/format` on one of these.
 */
export function formatPercentPoints(value: number, places = 2): string {
  if (!Number.isFinite(value)) return NO_VALUE;
  return `${value.toFixed(places)}%`;
}

/**
 * The same with an explicit sign, for the values where the SIGN is the
 * information: CLV and ROI are both routinely negative, and an unsigned positive
 * beside a negative reads as neutral.
 */
export function formatSignedPercentPoints(value: number, places = 2): string {
  if (!Number.isFinite(value)) return NO_VALUE;
  const rendered = value.toFixed(places);
  return value > 0 ? `+${rendered}%` : `${rendered}%`;
}

/**
 * An implied-probability change in POINTS: `0.021` renders as `"2.10 pts"`.
 *
 * The unit is in the string because "2.10" alone on a steam row would be read as
 * a price, and "2.10%" would be read as a percentage change in the price. It is
 * neither: it is an additive move in a probability.
 */
export function formatProbabilityPoints(value: number, places = 2): string {
  if (!Number.isFinite(value)) return NO_VALUE;
  const rendered = (value * 100).toFixed(places);
  return `${value > 0 ? '+' : ''}${rendered} pts`;
}

/**
 * A magnitude in points, unsigned — for a threshold or a filter label, where the
 * direction is carried elsewhere or is not part of the claim.
 */
export function formatPointsMagnitude(value: number, places = 2): string {
  if (!Number.isFinite(value)) return NO_VALUE;
  return `${(value * 100).toFixed(places)} pts`;
}

/**
 * A velocity: probability points per minute.
 *
 * Signed, and the unit is spelled out. Steam is detected on probability
 * velocity and NEVER on decimal-odds velocity, because decimal is non-linear in
 * probability — a 0.10 decimal move is 4.5 points at 1.50 and 0.1 points at
 * 10.00 — so a threshold in decimal means a different thing at every price. The
 * unit in the label is what stops a reader assuming otherwise.
 */
export function formatVelocity(value: number, places = 2): string {
  if (!Number.isFinite(value)) return NO_VALUE;
  const rendered = (value * 100).toFixed(places);
  return `${value > 0 ? '+' : ''}${rendered} pts/min`;
}

/**
 * A fraction of one rendered as a percentage: `0.49` -> `"49.0%"`.
 *
 * For a stake split and a beat rate — genuinely fractional quantities, unlike
 * everything above. It is a thin wrapper rather than a call to
 * `renderPercent` so that a reader of a component can see, at the call site,
 * which of the three units they are looking at.
 */
export function formatFractionAsPercent(value: number, places = 1): string {
  if (!Number.isFinite(value)) return NO_VALUE;
  return `${(value * 100).toFixed(places)}%`;
}

/**
 * A staleness figure in seconds, rendered for a reader.
 *
 * NEGATIVE IS RENDERED, not clamped. The API returns a negative age when a
 * provider's clock runs ahead of ours, and it does that deliberately — clamping
 * would hide clock skew inside the one number whose job is to expose staleness.
 * A reader seeing "-3s" learns something true; a reader seeing "0s" does not.
 */
export function formatAgeSeconds(value: number, places = 1): string {
  if (!Number.isFinite(value)) return NO_VALUE;
  if (Math.abs(value) < 60) return `${value.toFixed(places)}s`;
  const minutes = value / 60;
  if (Math.abs(minutes) < 60) return `${minutes.toFixed(1)}m`;
  return `${(minutes / 60).toFixed(1)}h`;
}

/**
 * A line rendered from a market type ALONE, with no selection role.
 *
 * `formatLine` in `@/lib/odds/line` is the function a board cell calls, and it
 * needs the role: a total renders as "O 47.5" or "U 47.5", and which one it is
 * depends on the side. The analytics payloads do not carry a role on the object
 * that carries the line — an `EVSignal`, a `CLVEntry` and an `ArbitrageSignal`
 * each name a selection or a market rather than describing a side — so calling
 * it here would mean inventing a role, and inventing a role would render half
 * the totals as the wrong side of the number.
 *
 * So this renders what is actually known:
 *
 *   - a SPREAD keeps its sign, because a spread's sign is its meaning and the
 *     payload's value is already in the frame the object is about (the
 *     selection's frame everywhere except `ArbitrageSignal.line`, which is the
 *     market's own home-frame line and is labelled as such at its call site);
 *   - a TOTAL and a PLAYER PROP render as the bare threshold, with no O/U
 *     prefix, because the prefix would be a guess;
 *   - a MONEYLINE and a FUTURES render as the empty string, which is a correct
 *     answer rather than a missing one.
 */
export function formatMarketLine(
  marketType: string,
  line: number | null | undefined,
): string {
  if (line === null || line === undefined || !Number.isFinite(line)) return '';
  switch (marketType) {
    case 'spread':
      return line > 0 ? `+${line}` : String(line);
    case 'total':
    case 'player_prop':
      return String(line);
    default:
      return '';
  }
}
