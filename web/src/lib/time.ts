/**
 * Time, staleness and instant formatting.
 *
 * Two rules govern this file, and both exist to stop a correct system from
 * looking broken.
 *
 * # 1. Staleness is measured against the SERVER's anchor, never the browser clock
 *
 * `BoardPage.as_of` says so in the OpenAPI document itself: "A client computes
 * staleness against this rather than against its own clock, so a skewed browser
 * clock cannot make a fresh board look stale." The same holds on the stream,
 * where every frame carries a `ts` and the anchor is the newest frame received.
 *
 * A laptop whose clock is four minutes fast would otherwise paint the entire
 * board as stale and fire the staleness treatment on every cell — a total false
 * positive on the project's headline SLO, produced entirely by the client.
 * `stalenessSeconds` therefore REQUIRES an anchor argument. There is no overload
 * that defaults to `Date.now()`.
 *
 * # 2. Formatted instants are deterministic across server and client
 *
 * These strings are produced during server-side rendering and again during
 * hydration. If the two disagree React logs a hydration error and swaps the DOM.
 * Two things can make them disagree: the LOCALE (the container has none, the
 * browser has the user's) and the TIME ZONE (the container is UTC, the browser
 * is not).
 *
 * Locale is pinned to `en-GB` everywhere in this file — a fixed, explicit
 * choice, never the ambient default. Time zone is an explicit parameter that
 * DEFAULTS TO UTC, so anything rendered on the server is deterministic by
 * construction. A component that wants the user's own zone passes it in after
 * mount, via `resolveLocalTimeZone()`, which is client-only and says so.
 */

/** The default zone for every formatter here. Deterministic on both sides. */
export const UTC = 'UTC';

/**
 * The fixed locale. Not the user's, deliberately: an ambient locale is the
 * second half of the hydration-mismatch failure this file exists to prevent, and
 * every string produced here is a machine-readable instant rather than prose.
 */
const LOCALE = 'en-GB';

// -----------------------------------------------------------------------------
// Parsing
// -----------------------------------------------------------------------------

/**
 * An RFC 3339 instant as epoch milliseconds, or `null` if it does not parse.
 *
 * Returns null rather than NaN so a caller cannot accidentally propagate a NaN
 * into a duration and render "NaNs". Every timestamp on both transports is
 * RFC 3339 with nanoseconds (Go's `time.Time` JSON encoding); `Date.parse`
 * truncates the sub-millisecond digits, which is finer than anything rendered.
 */
export function parseInstant(value: string | null | undefined): number | null {
  if (value === null || value === undefined || value === '') return null;
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? null : ms;
}

/** Any instant this module accepts as an anchor. */
export type Instant = string | number | Date;

/** Normalises an anchor to epoch milliseconds, or `null`. */
export function instantToMillis(value: Instant | null | undefined): number | null {
  if (value === null || value === undefined) return null;
  if (typeof value === 'number') return Number.isFinite(value) ? value : null;
  if (value instanceof Date) {
    const ms = value.getTime();
    return Number.isNaN(ms) ? null : ms;
  }
  return parseInstant(value);
}

// -----------------------------------------------------------------------------
// Staleness
// -----------------------------------------------------------------------------

/**
 * Seconds between a price's provider observation instant and the SERVER's
 * anchor.
 *
 * `observedAt` is the provider's own instant — the subtrahend in the staleness
 * SLO — and is NOT interchangeable with `ingested_at`. `asOf` is the server
 * anchor: `BoardPage.as_of` / `EventDetail.as_of` on REST, or the `ts` of the
 * newest WebSocket frame. Never pass `Date.now()`; see the file comment.
 *
 * Clamped at zero. A negative result means the anchor is older than the
 * observation, which is small clock skew between two of this system's own
 * services, and "-0.2s stale" is a worse thing to render than "0s".
 */
export function stalenessSeconds(
  observedAt: string | null | undefined,
  asOf: Instant | null | undefined,
): number | null {
  const observed = parseInstant(observedAt);
  const anchor = instantToMillis(asOf);
  if (observed === null || anchor === null) return null;
  return Math.max(0, (anchor - observed) / 1000);
}

/**
 * Seconds between two instants, unclamped. For durations that are legitimately
 * signed — time until kickoff, for instance, which is negative once a game is
 * under way.
 */
export function secondsBetween(
  from: Instant | null | undefined,
  to: Instant | null | undefined,
): number | null {
  const a = instantToMillis(from);
  const b = instantToMillis(to);
  if (a === null || b === null) return null;
  return (b - a) / 1000;
}

// -----------------------------------------------------------------------------
// Compact durations
// -----------------------------------------------------------------------------

const SECONDS_PER_MINUTE = 60;
const SECONDS_PER_HOUR = 3600;
const SECONDS_PER_DAY = 86_400;

/**
 * A duration in the tightest form that still reads: "3s", "2m", "1h", "4d".
 *
 * One unit, never two. This string lives in the mono status rail and beside
 * price cells, where every character costs density, and the reader is asking
 * "roughly how old" rather than "exactly how old".
 *
 * Truncates rather than rounds — 119 seconds is "1m", not "2m" — so the number
 * shown is never larger than the elapsed time. Understating staleness is the
 * dangerous direction, and truncation cannot do it.
 */
export function formatCompactDuration(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) {
    return '';
  }
  const absolute = Math.abs(seconds);
  if (absolute < SECONDS_PER_MINUTE) return `${String(Math.floor(absolute))}s`;
  if (absolute < SECONDS_PER_HOUR) {
    return `${String(Math.floor(absolute / SECONDS_PER_MINUTE))}m`;
  }
  if (absolute < SECONDS_PER_DAY) {
    return `${String(Math.floor(absolute / SECONDS_PER_HOUR))}h`;
  }
  return `${String(Math.floor(absolute / SECONDS_PER_DAY))}d`;
}

/**
 * A price's age, ready to render: "4s", "2m". Empty string when either instant
 * is missing, so a cell renders nothing rather than a placeholder.
 */
export function formatStaleness(
  observedAt: string | null | undefined,
  asOf: Instant | null | undefined,
): string {
  return formatCompactDuration(stalenessSeconds(observedAt, asOf));
}

/**
 * A duration spelled out for a screen reader: "4 seconds", "2 minutes".
 * The compact form is for the eye; an accessible name needs the unit in words.
 */
export function formatDurationWords(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) {
    return '';
  }
  const absolute = Math.abs(seconds);
  const plural = (value: number, unit: string): string =>
    `${String(value)} ${unit}${value === 1 ? '' : 's'}`;
  if (absolute < SECONDS_PER_MINUTE) {
    return plural(Math.floor(absolute), 'second');
  }
  if (absolute < SECONDS_PER_HOUR) {
    return plural(Math.floor(absolute / SECONDS_PER_MINUTE), 'minute');
  }
  if (absolute < SECONDS_PER_DAY) {
    return plural(Math.floor(absolute / SECONDS_PER_HOUR), 'hour');
  }
  return plural(Math.floor(absolute / SECONDS_PER_DAY), 'day');
}

// -----------------------------------------------------------------------------
// Absolute instants
// -----------------------------------------------------------------------------

const formatterCache = new Map<string, Intl.DateTimeFormat>();

function formatter(
  key: string,
  options: Intl.DateTimeFormatOptions,
): Intl.DateTimeFormat {
  const cached = formatterCache.get(key);
  if (cached !== undefined) return cached;
  const created = new Intl.DateTimeFormat(LOCALE, options);
  formatterCache.set(key, created);
  return created;
}

/**
 * The user's own IANA time zone, or `UTC` when it cannot be determined.
 *
 * CLIENT ONLY. Calling this during server-side rendering returns the container's
 * zone, which is not the user's, and a component that formats with it on the
 * server and again on the client will hydrate-mismatch. Call it inside an effect
 * and hold the result in state.
 */
export function resolveLocalTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || UTC;
  } catch {
    return UTC;
  }
}

/** "19:05" — 24 hour, zero padded. */
export function formatTimeOfDay(
  value: string | null | undefined,
  timeZone: string = UTC,
): string {
  const ms = parseInstant(value);
  if (ms === null) return '';
  return formatter(`tod:${timeZone}`, {
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
    timeZone,
  }).format(ms);
}

/** "Wed 19 Aug" — the board's day separator. */
export function formatDay(
  value: string | null | undefined,
  timeZone: string = UTC,
): string {
  const ms = parseInstant(value);
  if (ms === null) return '';
  return formatter(`day:${timeZone}`, {
    weekday: 'short',
    day: 'numeric',
    month: 'short',
    timeZone,
  }).format(ms);
}

/** "Wed 19 Aug, 19:05" — a kickoff time on an event row. */
export function formatDayAndTime(
  value: string | null | undefined,
  timeZone: string = UTC,
): string {
  const day = formatDay(value, timeZone);
  const time = formatTimeOfDay(value, timeZone);
  if (day === '' || time === '') return '';
  return `${day}, ${time}`;
}

/**
 * "2026-08-19 19:05:03 UTC" — the full instant, for a tooltip and for the
 * engineering layer. Sortable, unambiguous, and it names the zone, because an
 * instant with no zone on it is not an instant.
 */
export function formatAbsolute(
  value: string | null | undefined,
  timeZone: string = UTC,
): string {
  const ms = parseInstant(value);
  if (ms === null) return '';
  const parts = formatter(`abs:${timeZone}`, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
    timeZone,
  }).formatToParts(ms);

  const find = (type: Intl.DateTimeFormatPartTypes): string =>
    parts.find((part) => part.type === type)?.value ?? '';

  const date = `${find('year')}-${find('month')}-${find('day')}`;
  const time = `${find('hour')}:${find('minute')}:${find('second')}`;
  return `${date} ${time} ${timeZone}`;
}

/**
 * The ISO instant itself, unchanged, for a `<time dateTime>` attribute. A
 * machine-readable instant belongs in the attribute and a human-readable one in
 * the text; rendering the same formatted string into both throws away the only
 * unambiguous value available.
 */
export function toDateTimeAttribute(
  value: string | null | undefined,
): string | undefined {
  const ms = parseInstant(value);
  if (ms === null) return undefined;
  return new Date(ms).toISOString();
}
