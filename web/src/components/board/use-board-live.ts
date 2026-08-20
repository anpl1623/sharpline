'use client';

/**
 * The board's view model, and its binding to the live stream.
 *
 * # The division of labour, which every file in this directory obeys
 *
 * REST OWNS THE TREE. Which events are on the board, which markets each event
 * carries, which selections each market has, and which book is quoting them all
 * come from `GET /board`, fetched server-side so the first paint is real prices
 * with no spinner and no client waterfall.
 *
 * THE STREAM OWNS MOVEMENT. A WebSocket delta updates a price IN PLACE, joined
 * on `(market_id, selection_id, book_slug)` — the same three identifiers appear
 * on both payloads, which is what makes the join exact rather than heuristic. A
 * delta for a market that is not on the current page is ignored; it may belong
 * to another page of the keyset, and inventing a row for it would put an event
 * on screen that the board's own query did not return.
 *
 * There is NO POLLING. The socket is the live channel; a refetch beside it would
 * deliver the same change twice, once with a delta rail and once without, and
 * the recency gradient the rail exists to produce would become noise.
 *
 * # Why the league channel, and not the event channel
 *
 * A league board subscribes to `league:{slug}` — one channel for the whole page,
 * and the same string the route is keyed by, so the URL and the subscription are
 * literally identical. The all-leagues board subscribes to the league channel of
 * every league present in the page rather than to one `event:{id}` per row: a
 * page holds up to 50 events but only a handful of leagues, and the gateway
 * bounds channels per connection (`limit_reached`), so per-event subscription is
 * the shape that stops working first. `EventSummary` carries a `league_id` and
 * not a slug, which is why the route hands the catalogue down.
 *
 * # What this module deliberately does NOT do
 *
 * It does not read the connection state and it does not own a live region. The
 * app shell mounts exactly one `aria-live` region for market movement
 * (`components/live/live-announcer.tsx`) and one permanent status rail
 * (`components/layout/status-rail.tsx`); a board that mounted a second of either
 * would announce every price move twice to a screen reader and report the
 * connection twice to everyone else.
 *
 * That is also a re-render argument. The stream's state object is replaced on
 * EVERY frame, so anything reading it re-renders at the frame rate. Keeping it
 * out of the board is what makes "a tick re-renders one cell" true.
 */

import type {
  SchemaBoardEntry,
  SchemaBookKind,
  SchemaEventSummary,
  SchemaMarket,
  SchemaMarketType,
  SchemaPrice,
  SchemaSelection,
  SchemaSelectionRole,
} from '@/lib/api/schema';
import { useDisplayTimeZone } from '@/lib/client-value';
import { leagueChannel } from '@/lib/ws/protocol';
import type { Channel } from '@/lib/ws/protocol';
import { useChannelSubscriptions } from '@/lib/ws/provider';

// -----------------------------------------------------------------------------
// The catalogue
// -----------------------------------------------------------------------------

/**
 * The catalogue the board needs and the board payload does not carry.
 *
 * `EventSummary` names a league by id, and a `Price` names a book by slug; a
 * league header that says `01J…` and a provenance note that cannot say whether a
 * book is synthetic are both worse than one extra request. The routes read
 * `/sports`, `/sports/{slug}/leagues` and `/books` server-side and hand the
 * result down, so the client makes no catalogue request at all.
 */
export interface BoardLeagueView {
  readonly id: string;
  readonly slug: string;
  readonly name: string;
  /** Null when the sport that owns this league could not be resolved. */
  readonly sportName: string | null;
}

export interface BoardBookView {
  readonly slug: string;
  readonly name: string;
  readonly kind: SchemaBookKind;
  /** The sharp book the pricer devigs against. Every EV number is relative to it. */
  readonly isReference: boolean;
}

export interface BoardCatalogue {
  readonly leaguesById: Readonly<Record<string, BoardLeagueView>>;
  readonly booksBySlug: Readonly<Record<string, BoardBookView>>;
  readonly bookCount: number;
  /** True when every book in the catalogue is the in-house stochastic maker. */
  readonly allSynthetic: boolean;
  /** True when at least one is. Drives a weaker sentence than `allSynthetic`. */
  readonly anySynthetic: boolean;
}

/**
 * A book's display name, or its slug when the catalogue does not know it.
 *
 * Falling back to the slug rather than to "Unknown" keeps a real identifier on
 * screen: a slug is the thing the API is keyed by and is diagnosable, where a
 * placeholder is not.
 */
export function bookLabel(
  catalogue: BoardCatalogue,
  slug: string | null,
): string | null {
  if (slug === null) return null;
  return catalogue.booksBySlug[slug]?.name ?? slug;
}

/**
 * The sentence that says where these quotes come from.
 *
 * CLAUDE.md is explicit that a synthetic book's quote is a statement about a
 * random number generator, and that every surface rendering one must be able to
 * label it as such. This is the board's label. It is derived from the catalogue
 * rather than asserted, so the day a real provider is wired in the sentence
 * changes by itself.
 */
export function provenanceNote(catalogue: BoardCatalogue): string | null {
  if (catalogue.bookCount === 0) return null;
  if (catalogue.allSynthetic) {
    return `All ${String(catalogue.bookCount)} books are synthetic — their quotes are generated by this system’s stochastic market maker, not observed from a real bookmaker.`;
  }
  if (catalogue.anySynthetic) {
    return 'Some of these books are synthetic — their quotes are generated by this system rather than observed from a real bookmaker.';
  }
  return null;
}

/** Which book (or set of books) the prices on screen are drawn from. */
export function priceBasisNote(
  catalogue: BoardCatalogue,
  bookFilter: readonly string[],
): string {
  if (bookFilter.length === 1) {
    const only = bookFilter[0] ?? '';
    return `Prices from ${bookLabel(catalogue, only) ?? only}`;
  }
  if (bookFilter.length > 1) {
    return `Best price across ${String(bookFilter.length)} selected books`;
  }
  if (catalogue.bookCount > 0) {
    return `Best price across ${String(catalogue.bookCount)} books`;
  }
  return 'Best available price';
}

// -----------------------------------------------------------------------------
// Grouping
// -----------------------------------------------------------------------------

export interface BoardGroup {
  readonly leagueId: string;
  readonly leagueName: string;
  /** Null when the catalogue does not know this league — no channel, no link. */
  readonly leagueSlug: string | null;
  readonly sportName: string | null;
  readonly entries: readonly SchemaBoardEntry[];
}

/**
 * Groups the page into league blocks, preserving the API's ordering.
 *
 * Leagues appear in the order they first appear in the payload, and events keep
 * the order the API returned them in (soonest first). Nothing is re-sorted: the
 * board's ordering is a server decision that the keyset cursor is bound to, and
 * a client that re-sorts produces a "load more" whose new rows land in the
 * middle of the list.
 */
export function groupEntriesByLeague(
  entries: readonly SchemaBoardEntry[],
  catalogue: BoardCatalogue,
): readonly BoardGroup[] {
  const order: string[] = [];
  const buckets = new Map<string, SchemaBoardEntry[]>();

  for (const entry of entries) {
    const id = entry.event.league_id;
    let bucket = buckets.get(id);
    if (bucket === undefined) {
      bucket = [];
      buckets.set(id, bucket);
      order.push(id);
    }
    bucket.push(entry);
  }

  return order.map((id) => {
    const league = catalogue.leaguesById[id];
    return {
      leagueId: id,
      leagueName: league?.name ?? id,
      leagueSlug: league?.slug ?? null,
      sportName: league?.sportName ?? null,
      entries: buckets.get(id) ?? [],
    };
  });
}

/**
 * The channels the board holds.
 *
 * On the single-league route that is one channel, derived from the URL segment
 * itself — the route and its subscription are literally the same string, and the
 * subscription therefore survives an empty board, which is what a viewer waiting
 * for the first event of the evening needs.
 *
 * On the all-leagues route it is one channel per league PRESENT IN THE PAYLOAD,
 * derived from the UNFILTERED entries: a display filter must not silence the
 * stream, or turning on "live only" would stop the board updating exactly when
 * the viewer cares most.
 */
export function boardChannels(
  leagueSlug: string | null,
  entries: readonly SchemaBoardEntry[],
  catalogue: BoardCatalogue,
): readonly Channel[] {
  if (leagueSlug !== null) return [leagueChannel(leagueSlug)];

  const slugs = new Set<string>();
  for (const entry of entries) {
    const slug = catalogue.leaguesById[entry.event.league_id]?.slug;
    if (slug !== undefined) slugs.add(slug);
  }
  return [...slugs].sort().map(leagueChannel);
}

// -----------------------------------------------------------------------------
// Columns, markets and selections
// -----------------------------------------------------------------------------

/**
 * The board's columns, from DESIGN.md § Layout: `Game | Moneyline | Spread |
 * Total`. Player props and futures are not on the board and are not hidden
 * either — they live on the event page, which renders the full market tree.
 */
export const BOARD_COLUMNS = [
  'moneyline',
  'spread',
  'total',
] as const satisfies readonly SchemaMarketType[];

export type BoardColumn = (typeof BOARD_COLUMNS)[number];

/**
 * The market for one column, or null when the event does not offer it.
 *
 * The FIRST market of the type wins. An event carrying alternate lines would
 * return several; the board shows the main one and the event page shows the
 * rest.
 */
export function marketForColumn(
  markets: readonly SchemaMarket[],
  column: BoardColumn,
): SchemaMarket | null {
  for (const market of markets) {
    if (market.type === column) return market;
  }
  return null;
}

/**
 * Selections in the order the board stacks them.
 *
 * Away above home, matching the event cell, because the two must line up: a
 * moneyline column whose top cell is the home side while the game cell reads
 * away-first is a board nobody can use. `draw` sits last so a three-way market
 * still renders both sides in their usual places, and the row simply grows —
 * `.board-row` sets a height, and a table row treats that as a minimum.
 *
 * `Array.prototype.sort` has been stable since ES2019, so selections that share
 * a rank keep the API's own ordering.
 */
const SELECTION_RANK: Readonly<Record<SchemaSelectionRole, number>> = {
  away: 0,
  home: 1,
  draw: 2,
  over: 0,
  under: 1,
  outright: 0,
};

export function orderedSelections(
  market: SchemaMarket,
): readonly SchemaSelection[] {
  return [...market.selections].sort(
    (a, b) => SELECTION_RANK[a.role] - SELECTION_RANK[b.role],
  );
}

/**
 * The price a cell renders: the best available, restricted to the selected books.
 *
 * With no filter this is the server's own `best_price`, which is computed
 * server-side precisely so that "best" means one thing on every surface. With a
 * filter the server's answer is over the wrong set, so the best of the filtered
 * quotes is taken here — highest decimal odds, which is the same rule the server
 * applies, over a subset.
 *
 * Returns null when no book has quoted the selection inside the freshness
 * window. That is a CORRECT answer and not a missing one, and the cell renders
 * it as an empty well rather than as a dash that looks like a price.
 */
export function displayPrice(
  selection: SchemaSelection,
  bookFilter: readonly string[],
): SchemaPrice | null {
  if (bookFilter.length === 0) return selection.best_price ?? null;

  let best: SchemaPrice | null = null;
  for (const price of selection.prices) {
    if (!bookFilter.includes(price.book_slug)) continue;
    if (best === null || price.decimal_odds > best.decimal_odds) best = price;
  }
  return best;
}

// -----------------------------------------------------------------------------
// The event cell's competitors
// -----------------------------------------------------------------------------

export interface BoardCompetitor {
  readonly side: 'away' | 'home';
  readonly name: string;
  /** Null unless the event carries a score — i.e. unless it is under way. */
  readonly score: number | null;
}

/**
 * The two competitors, away first, matching `orderedSelections`.
 *
 * Both are optional on the payload: an outright event has neither, and the
 * caller falls back to `event.name`. Nothing is invented for a missing side.
 */
export function orderedCompetitors(
  event: SchemaEventSummary,
): readonly BoardCompetitor[] {
  const out: BoardCompetitor[] = [];
  const away = event.away_competitor;
  const home = event.home_competitor;
  if (away !== undefined) {
    out.push({ side: 'away', name: away.name, score: event.score?.away ?? null });
  }
  if (home !== undefined) {
    out.push({ side: 'home', name: home.name, score: event.score?.home ?? null });
  }
  return out;
}

/** The live-only filter. Applied client-side: the board API has no status parameter. */
export function filterLiveOnly(
  entries: readonly SchemaBoardEntry[],
  liveOnly: boolean,
): readonly SchemaBoardEntry[] {
  if (!liveOnly) return entries;
  return entries.filter((entry) => entry.event.status === 'live');
}

// -----------------------------------------------------------------------------
// Hooks
// -----------------------------------------------------------------------------

/**
 * Holds the board's channels for as long as the board is mounted.
 *
 * Deliberately returns nothing. Subscription is a side effect with no state, so
 * this cannot re-render its caller — which is the property that lets the board
 * own the subscription without owning a render on every frame.
 */
export function useBoardChannels(channels: readonly Channel[]): void {
  useChannelSubscriptions(channels);
}

/**
 * The viewer's own time zone.
 *
 * The server has no idea what it is, so the first client render must agree with
 * the server's UTC or every kickoff time on the board hydrate-mismatches. One
 * implementation lives in `@/lib/client-value`; this re-export keeps it part of
 * the board's published surface.
 */
export { useDisplayTimeZone };
