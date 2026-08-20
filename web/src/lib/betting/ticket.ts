/**
 * Ticket shape rules: which kinds a set of selections can be, and what a round
 * robin expands into.
 *
 * Pure functions over value types, no React and no I/O — the same discipline
 * `@/lib/odds/format` holds to, and for the same reason: these decide whether
 * the one irreversible button in the product is enabled, so they must be
 * readable and testable in isolation.
 *
 * # These are the CLIENT's copy of bounds the server owns
 *
 * Every constant below names the `internal/domain/wager.go` constant it mirrors.
 * They are duplicated here to disable a control before a request is sent, NOT to
 * decide anything: `POST /wagers` re-validates all of it, inside the placement
 * transaction, and that is the only evaluation that decides. A client bound that
 * drifted looser than the server's produces a refusal the user can read; one
 * that drifted tighter hides a legal ticket, which is why each is quoted with
 * its Go source rather than picked.
 *
 * # Two ticket shapes this deployment does not price, and they are not defects
 *
 * `internal/betting/doc.go` refuses a SAME-GAME parlay and a TEASER, both
 * deliberately:
 *
 *   - Same-game legs need a correlation adjustment (CLAUDE.md §4). Pricing them
 *     as independent overprices the ticket in the customer's favour — a real
 *     house-edge defect, not a rounding one.
 *   - A teaser's price is a POSTED LADDER, and `odds/parlay.go` says plainly
 *     that deriving one without an empirical model of the sport's margins would
 *     be "fabricated data of exactly the kind the project forbids".
 *
 * So this module does not know a teaser ladder and does not invent one. The
 * teaser control offers points and the SERVER answers — with its own fixed
 * message, rendered verbatim — whether it can price the ticket. Building a
 * plausible ladder into the frontend would be inventing exactly the number the
 * backend refused to invent, one layer further from anyone who would notice.
 */

import type {
  SchemaMarketType,
  SchemaWagerKind,
  SchemaWagerStatus,
} from '@/lib/api/schema';

// -----------------------------------------------------------------------------
// Bounds — internal/domain/wager.go
// -----------------------------------------------------------------------------

/**
 * `domain.MaxWagerLegs`. US books top out around a couple of dozen; 25 is past
 * every real offering and far below anything a malformed slip could produce.
 */
export const MAX_WAGER_LEGS = 25;

/**
 * `domain.MaxRoundRobinLegs`, and the one bound that is load-bearing rather than
 * cosmetic: a round robin's ticket count is a BINOMIAL COEFFICIENT, so this
 * number sits inside an exponential. At 10 selections every size together is
 * 2^10 - 11 = 1013 tickets; at 20 it would be a million, which is not a large
 * bet, it is a denial of service against the settlement path.
 */
export const MAX_ROUND_ROBIN_LEGS = 10;

/**
 * `domain.MaxTeaserPoints`. Football teasers run 6, 6.5, 7 and — for a "super"
 * teaser — 13; basketball runs 4 to 5. 20 clears all of them and still catches a
 * percentage read as points.
 */
export const MAX_TEASER_POINTS = 20;

/** Lines are quoted on a half-point grid, so the points that move them are too. */
export const TEASER_POINT_STEP = 0.5;

/** The smallest teaser that is a teaser at all. */
export const MIN_TEASER_POINTS = TEASER_POINT_STEP;

/** The order the slip offers the kinds in: simplest first. */
export const WAGER_KINDS = [
  'straight',
  'parlay',
  'round_robin',
  'teaser',
] as const satisfies readonly SchemaWagerKind[];

// -----------------------------------------------------------------------------
// Labels
// -----------------------------------------------------------------------------

export function wagerKindLabel(kind: SchemaWagerKind): string {
  switch (kind) {
    case 'straight':
      return 'Straight';
    case 'parlay':
      return 'Parlay';
    case 'round_robin':
      return 'Round robin';
    case 'teaser':
      return 'Teaser';
    default:
      return 'Ticket';
  }
}

/**
 * A ticket's status, in words.
 *
 * `void` and `push` both return the stake and are NOT the same fact — `void` is
 * the book cancelling the bet, `push` is the bet graded as a tie — so they get
 * different words rather than one "returned".
 */
export function wagerStatusLabel(status: SchemaWagerStatus): string {
  switch (status) {
    case 'placed':
      return 'Placed';
    case 'open':
      return 'Open';
    case 'won':
      return 'Won';
    case 'lost':
      return 'Lost';
    case 'void':
      return 'Void';
    case 'push':
      return 'Push';
    case 'cashed_out':
      return 'Cashed out';
    default:
      return 'Unknown';
  }
}

/**
 * Whether a ticket is still running — it holds escrow and can still change.
 *
 * `placed` and `open` are the two running statuses; the other five are terminal
 * and a terminal wager never transitions again. Cash-out is offered on a running
 * ticket and on nothing else.
 */
export function isWagerRunning(status: SchemaWagerStatus): boolean {
  return status === 'placed' || status === 'open';
}

/** The statuses "open positions" means, as the API's repeatable filter. */
export const RUNNING_WAGER_STATUSES = [
  'placed',
  'open',
] as const satisfies readonly SchemaWagerStatus[];

/** Every terminal status, for the settled view. */
export const SETTLED_WAGER_STATUSES = [
  'won',
  'lost',
  'void',
  'push',
  'cashed_out',
] as const satisfies readonly SchemaWagerStatus[];

// -----------------------------------------------------------------------------
// Combinatorics
// -----------------------------------------------------------------------------

/**
 * C(n, k), computed multiplicatively so the intermediate values stay small.
 *
 * `n!/(k!(n-k)!)` overflows a float64 at n = 171 even when the answer is tiny;
 * the multiplicative form keeps every partial product bounded by the result.
 * Bounded by `MAX_ROUND_ROBIN_LEGS` in practice, where the largest value is
 * C(10, 5) = 252, so this is exact.
 */
export function binomial(n: number, k: number): number {
  if (!Number.isInteger(n) || !Number.isInteger(k)) return 0;
  if (k < 0 || k > n || n < 0) return 0;
  const smaller = Math.min(k, n - k);
  let result = 1;
  for (let step = 1; step <= smaller; step += 1) {
    result = (result * (n - smaller + step)) / step;
  }
  return Math.round(result);
}

/**
 * How many tickets a round robin expands into: the sum of C(n, k) over its
 * sizes.
 *
 * This is the number the customer is risking a stake on EACH of, which is the
 * whole reason it is rendered before placement rather than discovered after:
 * "a $5 round robin by 2s on four selections risks $30, not $5" is the API's own
 * example, and a slip that showed only the $5 would be lying by omission about
 * the one number that matters.
 */
export function combinationCount(
  legCount: number,
  sizes: readonly number[],
): number {
  let total = 0;
  for (const size of sizes) total += binomial(legCount, size);
  return total;
}

/**
 * The combination sizes a round robin over `legCount` selections may use.
 *
 * From 2 up to the leg count. `[legCount]` is a legal size and expands to
 * exactly one ticket — the same ticket a parlay would be — and it is offered
 * rather than hidden, because a customer building "by 2s and by 4s" on four
 * selections is asking for something coherent.
 */
export function roundRobinSizeOptions(legCount: number): readonly number[] {
  if (legCount < 2) return [];
  const sizes: number[] = [];
  for (let size = 2; size <= legCount; size += 1) sizes.push(size);
  return sizes;
}

/**
 * Sizes sorted ascending and de-duplicated — the canonical order the domain
 * stores, so `[3, 2, 3]` and `[2, 3]` describe the same round robin and produce
 * the same quote cache key.
 */
export function canonicalSizes(sizes: readonly number[]): readonly number[] {
  return [...new Set(sizes)].sort((a, b) => a - b);
}

// -----------------------------------------------------------------------------
// What a set of legs can be
// -----------------------------------------------------------------------------

/** The part of a slip leg these rules read. Deliberately not the whole leg. */
export interface TicketShapeLeg {
  readonly eventId: string;
  readonly marketType: SchemaMarketType;
  /** The line from this selection's own perspective, or null if it has none. */
  readonly line: number | null;
}

/**
 * Whether a leg can be teased: only a spread or a total, and only one that
 * carries a line.
 *
 * `internal/betting/errors.go` states the rule and the reason — "only a spread
 * or total selection can be teased", because moving a line the market does not
 * have "would be giving away the whole edge the teaser price is built on".
 */
export function isTeasable(leg: TicketShapeLeg): boolean {
  if (leg.line === null || !Number.isFinite(leg.line)) return false;
  return leg.marketType === 'spread' || leg.marketType === 'total';
}

/** Whether a kind is offered, and — when it is not — the one-line reason. */
export interface KindAvailability {
  readonly kind: SchemaWagerKind;
  readonly available: boolean;
  /** Null when available. A fixed sentence, never assembled from server text. */
  readonly reason: string | null;
}

/**
 * Which kinds this set of legs can be, and why not for the rest.
 *
 * The reasons are rendered on a disabled tab, so a customer with four legs
 * learns that "straight" needs exactly one rather than finding a control that
 * silently does nothing. Every rule here is an ARITY or a MARKET-TYPE rule —
 * things this client can know for certain from the slip alone. Nothing about
 * balance, limits, suspension or price movement is decided here; all of that is
 * server state, and guessing at it is how a button ends up disabled for a reason
 * that stopped being true a second ago.
 */
export function kindAvailability(
  legs: readonly TicketShapeLeg[],
): readonly KindAvailability[] {
  const count = legs.length;
  const teasable = legs.every(isTeasable);

  return WAGER_KINDS.map((kind): KindAvailability => {
    switch (kind) {
      case 'straight':
        return count === 1
          ? { kind, available: true, reason: null }
          : {
              kind,
              available: false,
              reason: 'A straight is one selection.',
            };

      case 'parlay':
        if (count < 2) {
          return {
            kind,
            available: false,
            reason: 'A parlay needs at least two selections.',
          };
        }
        if (count > MAX_WAGER_LEGS) {
          return {
            kind,
            available: false,
            reason: `A ticket takes at most ${String(MAX_WAGER_LEGS)} selections.`,
          };
        }
        return { kind, available: true, reason: null };

      case 'round_robin':
        if (count < 2) {
          return {
            kind,
            available: false,
            reason: 'A round robin needs at least two selections.',
          };
        }
        if (count > MAX_ROUND_ROBIN_LEGS) {
          return {
            kind,
            available: false,
            reason: `A round robin takes at most ${String(MAX_ROUND_ROBIN_LEGS)} selections — its ticket count grows exponentially.`,
          };
        }
        return { kind, available: true, reason: null };

      case 'teaser':
        if (count < 2) {
          return {
            kind,
            available: false,
            reason: 'A teaser needs at least two selections.',
          };
        }
        if (!teasable) {
          return {
            kind,
            available: false,
            reason: 'Only a spread or total selection can be teased.',
          };
        }
        if (count > MAX_WAGER_LEGS) {
          return {
            kind,
            available: false,
            reason: `A ticket takes at most ${String(MAX_WAGER_LEGS)} selections.`,
          };
        }
        return { kind, available: true, reason: null };

      default:
        return { kind, available: false, reason: null };
    }
  });
}

/**
 * The kind a slip of this size should fall back to when its current kind stops
 * being legal — adding a second leg to a straight, or removing the second leg
 * from a parlay.
 *
 * Straight below two selections, parlay at or above it. It never picks a round
 * robin or a teaser: both are deliberate choices with their own parameters, and
 * silently converting somebody's parlay into a round robin would multiply what
 * they are risking without them asking.
 */
export function fallbackKind(legCount: number): SchemaWagerKind {
  return legCount >= 2 ? 'parlay' : 'straight';
}

/**
 * Whether teaser points are a legal value: finite, strictly positive, at most
 * `MAX_TEASER_POINTS`, and on the half-point grid lines are quoted on.
 *
 * The grid check is `2 * points` being an integer rather than a modulo against
 * 0.5, because 0.5 is a dyadic fraction and exact in float64 while `x % 0.5` on
 * a value that arrived through a decimal string parse is not.
 */
export function isValidTeaserPoints(points: number): boolean {
  if (!Number.isFinite(points)) return false;
  if (points < MIN_TEASER_POINTS || points > MAX_TEASER_POINTS) return false;
  return Number.isInteger(points * 2);
}
