'use client';

/**
 * The slip's view model: the two request bodies, and the live-price watch that
 * decides whether the Place button is allowed to be pressed.
 *
 * # Two sources, one job each, and the line between them is the design
 *
 * THE WEBSOCKET DETECTS MOVEMENT. Every leg subscribes to `market:{id}` — the
 * channel exists precisely so a slip can watch one leg without carrying the
 * whole event — and its live price is compared against the frozen number the
 * customer clicked. A difference blocks submission immediately, at the cost of
 * nothing: the frame was already on the wire.
 *
 * THE QUOTE OWNS THE MONEY. `POST /slip/quote` returns the ticket price, the
 * potential payout, the potential profit and the cash balance, all computed
 * against the prices the request named. It is re-fetched when the CUSTOMER
 * changes something — a leg, the stake, the kind, an acceptance — and never on a
 * tick.
 *
 * The alternative, re-quoting on every price change, was rejected for a reason
 * that is not only about request volume: a payout figure that re-rendered on
 * every tick would be a number moving on screen beside prices that are also
 * moving, and DESIGN.md spends the entire motion budget on the delta rail
 * specifically so that one thing moves at a time. It would also be dishonest in
 * a subtler way — that payout would be computed from a price the customer has
 * not agreed to, displayed as though it were theirs.
 *
 * So while a move is pending, the payout on screen is the payout at the prices
 * the customer accepted, the interstitial says what changed, and the button is
 * off. Accepting writes the new number into the slip, which changes the quote's
 * cache key, which produces a payout at the price now agreed. Every figure on
 * screen has one provenance and can say what it is.
 *
 * # seen_ticket_decimal is sent on the PLACEMENT and not on the quote
 *
 * The field's only effect on a quote is to populate `ticket_movement`, and
 * feeding a quote's own answer back into its next request would make the query
 * key depend on the response — a loop that re-fetches for ever. On the
 * PLACEMENT the same field is load-bearing and is sent: it is the whole-ticket
 * price the customer was shown, and without it a parlay could be booked at a
 * price nobody agreed to, since the ticket price is not any leg's price.
 */

import { useEffect, useMemo, useState } from 'react';

import type {
  SchemaPlaceWagerRequest,
  SchemaPlacementLeg,
  SchemaPriceMovement,
  SchemaSlipLeg,
  SchemaSlipQuote,
  SchemaSlipQuoteRequest,
} from '@/lib/api/schema';
import {
  legEffectiveDecimal,
  legEffectiveLine,
  type SlipLeg,
  type SlipState,
} from '@/lib/store/slip';
import { marketChannel } from '@/lib/ws/protocol';
import type { Channel } from '@/lib/ws/protocol';
import { useChannelSubscriptions, useMarketSlate } from '@/lib/ws/provider';
import { priceCellKey } from '@/lib/ws/store';
import type { MarketSlate } from '@/lib/ws/store';

// -----------------------------------------------------------------------------
// Watching one leg
// -----------------------------------------------------------------------------

/**
 * What the live stream says about one leg, relative to the number the customer
 * agreed to.
 *
 * `movement` is named from the CUSTOMER'S side rather than as up/down, matching
 * the API's own vocabulary and for the same reason: "the odds went up" is
 * ambiguous in exactly the direction that matters. `lengthened` pays MORE per
 * unit staked than the customer saw; `shortened` pays less.
 */
export interface LegWatch {
  readonly selectionId: string;
  /** The price now, or null when the stream has not delivered this leg yet. */
  readonly currentDecimal: number | null;
  readonly currentLine: number | null;
  readonly movement: SchemaPriceMovement;
  /**
   * Whether the LINE moved. Reported separately from `movement` because it is a
   * different question: `movement` compares two prices, this compares two BETS.
   * A spread of -4 loses games that -3.5 wins, so a leg whose line moved needs
   * an explicit acceptance naming the new line even when the price improved.
   */
  readonly lineMoved: boolean;
  /**
   * A LONGER price at an UNCHANGED line. False whenever the line moved at all,
   * even if the price lengthened, because "better" is not defined across a line
   * move.
   */
  readonly improved: boolean;
  /** Whether anything the customer agreed to is no longer what is offered. */
  readonly moved: boolean;
}

const UNCHANGED: Omit<LegWatch, 'selectionId'> = {
  currentDecimal: null,
  currentLine: null,
  movement: 'unchanged',
  lineMoved: false,
  improved: false,
  moved: false,
};

/**
 * Lines are quoted on a quarter-point grid and every value on it is a dyadic
 * fraction, so a line that has not moved differs from its stored copy by
 * EXACTLY zero in float64 and an equality test is correct. The tolerance exists
 * only to absorb a value that arrived through a decimal string parse; the
 * smallest real move the domain can express is a quarter point, eight orders of
 * magnitude above it. This mirrors `domain.teaserLineTolerance` and its
 * reasoning.
 */
const LINE_TOLERANCE = 1e-9;

function linesDiffer(a: number | null, b: number | null): boolean {
  if (a === null && b === null) return false;
  if (a === null || b === null) return true;
  return Math.abs(a - b) > LINE_TOLERANCE;
}

/**
 * One leg's live state, read from the slate.
 *
 * A leg the stream has not delivered yet reports UNCHANGED rather than moved.
 * That is the correct reading of "no information": the price was on screen a
 * moment ago and nothing has contradicted it, and treating silence as a move
 * would block the button on every slip built before the socket caught up.
 */
export function legWatch(slate: MarketSlate | null, leg: SlipLeg): LegWatch {
  const live = slate?.getCell(leg.marketId, leg.selectionId, leg.bookSlug);
  if (live === undefined) {
    return { selectionId: leg.selectionId, ...UNCHANGED };
  }

  const agreedDecimal = legEffectiveDecimal(leg);
  const agreedLine = legEffectiveLine(leg);
  const currentDecimal = live.decimal;
  const currentLine = live.line;
  const lineMoved = linesDiffer(currentLine, agreedLine);

  const movement: SchemaPriceMovement =
    currentDecimal > agreedDecimal
      ? 'lengthened'
      : currentDecimal < agreedDecimal
        ? 'shortened'
        : 'unchanged';

  return {
    selectionId: leg.selectionId,
    currentDecimal,
    currentLine,
    movement,
    lineMoved,
    improved: !lineMoved && movement === 'lengthened',
    moved: lineMoved || movement !== 'unchanged',
  };
}

/**
 * Every leg's live state, recomputed when any of them ticks.
 *
 * # Why the whole slip re-renders here, when the board deliberately does not
 *
 * The board's rule is that a tick re-renders ONE CELL and never the row, because
 * a board holds hundreds of cells and re-rendering a row would re-render
 * everything on it. The slip is the opposite shape and has the opposite need: it
 * holds at most 25 legs, and the thing it has to decide — whether the one
 * irreversible button may be pressed — is a property of ALL of them at once. A
 * per-row subscription would leave the button reading a value assembled from
 * rows that each know only their own answer, which is how a button ends up
 * enabled for one render after a price moved.
 *
 * So the subscription is here, the aggregate is computed here, and each row
 * receives its own watch as a prop. The cost is a re-render of a small panel on
 * a tick; the payoff is that "is anything blocking" is answered in one place
 * from one set of values read at one instant.
 *
 * NOTHING ANIMATES as a result. Every money figure on the panel comes from the
 * cached quote, not from this, so a tick changes the interstitial and the button
 * and leaves the payout exactly where it was.
 */
export function useSlipWatches(legs: readonly SlipLeg[]): readonly LegWatch[] {
  const slate = useMarketSlate();
  const [revision, setRevision] = useState(0);

  // The identity of the watched SET, as one string. The legs array is rebuilt
  // by the store on every mutation — including ones that leave the watched
  // triples untouched, such as a stake keystroke — and resubscribing on each of
  // those would tear down and re-establish every cell subscription while
  // somebody is typing.
  const cellKeys = legs
    .map((leg) => priceCellKey(leg.marketId, leg.selectionId, leg.bookSlug))
    .join('\n');

  const triples = useMemo(
    () =>
      legs.map((leg) => ({
        marketId: leg.marketId,
        selectionId: leg.selectionId,
        bookSlug: leg.bookSlug,
      })),
    // Keyed on the joined identity rather than on `legs`, for the reason above.
    // The key is built from exactly the three fields the body reads, so it
    // cannot go stale relative to what it rebuilds.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [cellKeys],
  );

  useEffect(() => {
    if (slate === null || triples.length === 0) return;
    const notify = (): void => {
      setRevision((current) => current + 1);
    };
    const unsubscribes = triples.map((triple) =>
      slate.subscribeToCell(
        triple.marketId,
        triple.selectionId,
        triple.bookSlug,
        notify,
      ),
    );
    return () => {
      for (const unsubscribe of unsubscribes) unsubscribe();
    };
  }, [slate, triples]);

  return useMemo(
    () => legs.map((leg) => legWatch(slate, leg)),
    // `revision` is the whole point of this dependency list: the slate is
    // mutable and its identity never changes, so the counter is the only thing
    // that can tell this memo the values behind it did.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [legs, slate, revision],
  );
}

/**
 * Holds a `market:{id}` channel for every leg, for as long as the slip holds it.
 *
 * The market channel and not the event one: a slip with four legs on four games
 * would otherwise pull four whole events' worth of markets through the socket to
 * watch four prices, and the gateway bounds channels per connection. The board
 * subscribes by LEAGUE for the opposite reason — it wants everything on the page
 * and there are far fewer leagues than events. Both read the same slate, so a
 * leg whose market is already arriving on a league channel costs the slip
 * nothing extra beyond one subscribe.
 */
export function useSlipChannels(legs: readonly SlipLeg[]): void {
  const channels = useMemo<readonly Channel[]>(
    () => [...new Set(legs.map((leg) => leg.marketId))].sort().map(marketChannel),
    [legs],
  );
  useChannelSubscriptions(channels);
}

/**
 * Whether a move on this leg stops the slip being placed.
 *
 * An IMPROVED price at an unchanged line is waved through only when the customer
 * has asked for that in as many words. Every real book takes an improvement
 * without asking; this one does not unless told, and the API's own note explains
 * why the concession is explicit — "accept when the new price is longer" and
 * "accept when the new price is shorter" are one comparison operator apart, and
 * the difference between them is invisible in review and invisible in every test
 * where the line does not move.
 */
export function watchBlocks(watch: LegWatch, acceptBetterPrice: boolean): boolean {
  if (!watch.moved) return false;
  return !(acceptBetterPrice && watch.improved);
}

// -----------------------------------------------------------------------------
// Request bodies
// -----------------------------------------------------------------------------

/** The part of the slip a request is built from. Excludes every UI-only field. */
export type SlipRequestState = Pick<
  SlipState,
  | 'legs'
  | 'kind'
  | 'stakeMinor'
  | 'roundRobinSizes'
  | 'teaserPoints'
  | 'acceptBetterPrice'
  | 'acceptedTicketDecimal'
>;

function wireLegs(legs: readonly SlipLeg[]): SchemaSlipLeg[] {
  return legs.map((leg) => ({
    selection_id: leg.selectionId,
    book_id: leg.bookId,
    seen_decimal: legEffectiveDecimal(leg),
    seen_line: legEffectiveLine(leg),
  }));
}

/**
 * The quote body, or `null` when the slip is not yet something that can be
 * priced.
 *
 * Returning null rather than a partial body is what keeps a guaranteed-`400` off
 * the wire: the API requires a strictly positive stake, at least one size on a
 * round robin and points on a teaser, and a slip mid-construction has none of
 * them. The button's disabled reason comes from the slip itself in that state,
 * not from a rejected request.
 *
 * `seen_decimal` here is the EFFECTIVE price — the acceptance if there is one,
 * otherwise the frozen original — so a quote taken after an acceptance is priced
 * against the number the customer actually agreed to.
 */
export function slipQuoteRequest(
  state: SlipRequestState,
): SchemaSlipQuoteRequest | null {
  if (state.legs.length === 0) return null;
  if (state.stakeMinor <= 0) return null;
  if (state.kind === 'round_robin' && state.roundRobinSizes.length === 0) {
    return null;
  }
  if (state.kind === 'teaser' && state.teaserPoints === null) return null;

  return {
    kind: state.kind,
    legs: wireLegs(state.legs),
    stake_minor: state.stakeMinor,
    ...(state.kind === 'round_robin'
      ? { round_robin_sizes: [...state.roundRobinSizes] }
      : {}),
    ...(state.kind === 'teaser' && state.teaserPoints !== null
      ? { teaser_points: state.teaserPoints }
      : {}),
  };
}

/**
 * The placement body, or `null` when there is nothing placeable to send.
 *
 * `quote` is REQUIRED and not optional, because `seen_ticket_decimal` comes from
 * it. That field is what stops a parlay being booked at a price nobody agreed to
 * — a parlay's ticket price is not any leg's price, and same-game legs carry a
 * correlation adjustment that makes it not even the product of them — so a
 * placement assembled without a quote on screen would be a placement with no
 * whole-ticket consent in it. Making the argument non-optional is how that stops
 * being a thing a caller can skip.
 *
 * On a round robin the field is omitted: its combinations are independent
 * tickets at different prices, `SlipQuote.decimal_odds` is null there for that
 * reason, and the API says it is never sent.
 *
 * An acceptance is emitted only when there is one. `accepted_decimal: null` and
 * an absent key mean the same thing to the server, but sending the key on every
 * leg would make "the customer accepted a move" and "there was nothing to
 * accept" indistinguishable in a request log.
 */
export function placeWagerRequest(
  state: SlipRequestState,
  quote: SchemaSlipQuote,
): SchemaPlaceWagerRequest | null {
  // The placement's preconditions are exactly the quote's — a leg, a positive
  // stake, sizes on a round robin, points on a teaser — so they are checked by
  // asking the quote builder rather than restated. Two copies of the same list
  // is how a slip ends up quotable and unplaceable, or the reverse.
  if (slipQuoteRequest(state) === null) return null;

  const legs: SchemaPlacementLeg[] = state.legs.map((leg) => ({
    selection_id: leg.selectionId,
    book_id: leg.bookId,
    seen_decimal: leg.seenDecimal,
    seen_line: leg.seenLine,
    ...(leg.acceptedDecimal === null
      ? {}
      : { accepted_decimal: leg.acceptedDecimal }),
    ...(leg.acceptedLine === null ? {} : { accepted_line: leg.acceptedLine }),
  }));

  const ticketDecimal = quote.decimal_odds ?? null;
  const sendsTicketPrice = state.kind !== 'round_robin' && ticketDecimal !== null;

  return {
    kind: state.kind,
    legs,
    stake_minor: state.stakeMinor,
    accept_better_price: state.acceptBetterPrice,
    ...(state.kind === 'round_robin'
      ? { round_robin_sizes: [...state.roundRobinSizes] }
      : {}),
    ...(state.kind === 'teaser' && state.teaserPoints !== null
      ? { teaser_points: state.teaserPoints }
      : {}),
    ...(sendsTicketPrice ? { seen_ticket_decimal: ticketDecimal } : {}),
    ...(state.acceptedTicketDecimal !== null && state.kind !== 'round_robin'
      ? { accepted_ticket_decimal: state.acceptedTicketDecimal }
      : {}),
  };
}
