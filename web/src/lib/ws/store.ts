/**
 * The live market slate: what the board is currently showing, and what moved.
 *
 * It is a plain class, not a React store, because DESIGN.md forbids the obvious
 * React shape. Two constraints drive the whole design:
 *
 *   "Per-cell decay timers; do not re-render the row."
 *   "transform and opacity only. Never animate layout."
 *
 * A store that publishes one version number per tick makes every subscriber
 * re-render, which on a 123-market league board is the entire table re-rendering
 * several times a second. So notification is PER MARKET and PER CELL: a price
 * cell subscribes to exactly its own `(market, selection, book)` triple and is
 * notified only when THAT price changes. The row above it re-renders never.
 *
 * # A byte-identical price is not a change
 *
 * Most polls return identical data. The ingest stage already suppresses no-op
 * payloads by hashing, but a market can still be republished because some OTHER
 * book's quote moved. Comparing decimals exactly and skipping equal ones is what
 * keeps the delta rail meaning "this price moved" rather than "a frame arrived".
 * Lighting the board on republication is precisely the "noise, not information"
 * failure DESIGN.md names.
 *
 * # Direction is measured on implied probability
 *
 *   decimal FELL  -> probability ROSE -> 'in'   (amber; the price shortened, steam)
 *   decimal ROSE  -> probability FELL -> 'out'  (cyan; the price lengthened)
 *
 * Since probability is 1/decimal and decimal is always > 1, comparing decimals
 * and inverting the sense is exactly equivalent and avoids a division per tick.
 *
 * # A snapshot does not fire rails
 *
 * `applySnapshot` seeds baselines and emits no movement. A snapshot is a
 * RE-STATEMENT of a whole channel — on first subscribe, and again after a
 * reconnect or a server-initiated resync — not an observed movement. Attributing
 * rails to one would light every cell on the board at once on every reconnect,
 * and the recency gradient the decay exists to produce would be destroyed by the
 * one event most likely to happen while somebody is watching. Cells whose price
 * is unchanged keep their existing rail state untouched, so a reconnect does not
 * erase a decay that is still running.
 */

import type {
  BookAssessment,
  ComputedMarket,
  DeltaFrame,
  FairSelection,
  QuoteAssessment,
  SnapshotFrame,
} from '@/lib/ws/protocol';
import { marketIdOf } from '@/lib/ws/protocol';

/** Which way the implied probability moved. `null` means it did not. */
export type DeltaDirection = 'in' | 'out';

/**
 * One price cell's live state.
 *
 * The object identity is STABLE while the price is unchanged, so
 * `useSyncExternalStore` can hold it as a snapshot and React can skip the
 * re-render. A new object is created only when the decimal actually changes.
 */
export interface PriceCell {
  readonly marketId: string;
  readonly selectionId: string;
  readonly bookSlug: string;
  /** The canonical price. Everything on screen is converted from this. */
  readonly decimal: number;
  /** The price this one replaced, or null if this is the first observation. */
  readonly previousDecimal: number | null;
  /** Null on a first observation and on a snapshot-seeded value. */
  readonly direction: DeltaDirection | null;
  /** Epoch ms when the change was observed by THIS client. Drives the decay. */
  readonly changedAt: number | null;
  /** The book's implied probability, WITH ITS VIG. Not a fair probability. */
  readonly implied: number;
  /** The line this quote was made at, from this selection's own perspective. */
  readonly line: number | null;
  /** The provider's own instant. The staleness subtrahend. */
  readonly observedAt: string;
}

const KEY_SEPARATOR = '\u0000';

/**
 * The slate key for one price. A NUL separator rather than a colon because
 * `domain.validID` already excludes `:` for the channel grammar's sake and a
 * second overloaded separator is a second thing to get wrong.
 */
export function priceCellKey(
  marketId: string,
  selectionId: string,
  bookSlug: string,
): string {
  return `${marketId}${KEY_SEPARATOR}${selectionId}${KEY_SEPARATOR}${bookSlug}`;
}

type Notify = () => void;

/** A market that moved, for the throttled screen-reader announcer. */
export interface MarketMovement {
  readonly marketId: string;
  readonly at: number;
  /** How many individual prices moved on this market in this frame. */
  readonly cells: number;
}

export interface SlateStats {
  readonly markets: number;
  readonly cells: number;
  /** Monotonic count of markets that have moved since construction. */
  readonly movements: number;
  readonly lastMovementAt: number | null;
}

const EMPTY_IDS: readonly string[] = [];

export class MarketSlate {
  private readonly markets = new Map<string, ComputedMarket>();
  private readonly cells = new Map<string, PriceCell>();
  /** Which cell keys belong to a market, so a vanished quote can be removed. */
  private readonly marketCellKeys = new Map<string, Set<string>>();
  /** Which markets are on a channel, so a channel view can list them. */
  private readonly channelMarkets = new Map<string, Set<string>>();
  private readonly channelArrays = new Map<string, readonly string[]>();

  private readonly marketListeners = new Map<string, Set<Notify>>();
  private readonly cellListeners = new Map<string, Set<Notify>>();
  private readonly channelListeners = new Map<string, Set<Notify>>();
  private readonly movementListeners = new Set<(m: MarketMovement) => void>();
  private readonly statsListeners = new Set<Notify>();

  private movements = 0;
  private lastMovementAt: number | null = null;
  private stats: SlateStats = {
    markets: 0,
    cells: 0,
    movements: 0,
    lastMovementAt: null,
  };

  // ---------------------------------------------------------------------------
  // Reads
  // ---------------------------------------------------------------------------

  getMarket(marketId: string): ComputedMarket | undefined {
    return this.markets.get(marketId);
  }

  getCell(
    marketId: string,
    selectionId: string,
    bookSlug: string,
  ): PriceCell | undefined {
    return this.cells.get(priceCellKey(marketId, selectionId, bookSlug));
  }

  /**
   * The market ids currently on a channel, as a stable array.
   *
   * The array identity changes only when membership changes, so a component can
   * hold it as a `useSyncExternalStore` snapshot without looping.
   */
  getMarketIdsForChannel(channel: string): readonly string[] {
    const cached = this.channelArrays.get(channel);
    if (cached !== undefined) return cached;
    const set = this.channelMarkets.get(channel);
    if (set === undefined) return EMPTY_IDS;
    const array: readonly string[] = [...set];
    this.channelArrays.set(channel, array);
    return array;
  }

  /**
   * The selection's display name and devigged fair value.
   *
   * `fair.selections[].name` is THE ONLY PLACE a selection's name appears on the
   * streamed payload — book quotes carry an id and no name — so a stream-only
   * surface joins through here.
   */
  getFairSelection(
    marketId: string,
    selectionId: string,
  ): FairSelection | undefined {
    return this.markets
      .get(marketId)
      ?.fair.selections.find((s) => s.selection_id === selectionId);
  }

  /** One book's whole position on a market. */
  getBook(marketId: string, bookSlug: string): BookAssessment | undefined {
    return this.markets.get(marketId)?.books.find((b) => b.slug === bookSlug);
  }

  /** One book's quote on one selection, with its EV, edge and Kelly numbers. */
  getQuote(
    marketId: string,
    selectionId: string,
    bookSlug: string,
  ): QuoteAssessment | undefined {
    return this.getBook(marketId, bookSlug)?.quotes.find(
      (q) => q.selection_id === selectionId,
    );
  }

  getStats(): SlateStats {
    return this.stats;
  }

  // ---------------------------------------------------------------------------
  // Subscriptions
  // ---------------------------------------------------------------------------

  /** Notified when this market's document is replaced by a changed one. */
  subscribeToMarket(marketId: string, notify: Notify): () => void {
    return subscribeIn(this.marketListeners, marketId, notify);
  }

  /**
   * Notified when THIS price changes, and at no other time. This is the
   * subscription a board price cell uses; nothing above it re-renders.
   */
  subscribeToCell(
    marketId: string,
    selectionId: string,
    bookSlug: string,
    notify: Notify,
  ): () => void {
    return subscribeIn(
      this.cellListeners,
      priceCellKey(marketId, selectionId, bookSlug),
      notify,
    );
  }

  /** Notified when the set of markets on a channel changes. */
  subscribeToChannel(channel: string, notify: Notify): () => void {
    return subscribeIn(this.channelListeners, channel, notify);
  }

  /**
   * Notified once per market that moved, with the market id.
   *
   * The throttled `aria-live` announcer accumulates these into a set over a five
   * second window and emits ONE sentence. It must never announce per tick — that
   * is the single worst thing this UI could do to a screen reader user.
   */
  subscribeToMovement(listener: (movement: MarketMovement) => void): () => void {
    this.movementListeners.add(listener);
    return () => {
      this.movementListeners.delete(listener);
    };
  }

  /** Notified when the slate's counts change. For the engineering status rail. */
  subscribeToStats(notify: Notify): () => void {
    this.statsListeners.add(notify);
    return () => {
      this.statsListeners.delete(notify);
    };
  }

  // ---------------------------------------------------------------------------
  // Writes
  // ---------------------------------------------------------------------------

  /**
   * Applies a whole channel snapshot. Seeds baselines; emits NO movement.
   * See the file comment for why.
   */
  applySnapshot(frame: SnapshotFrame): void {
    const channelChanged = this.ensureChannel(frame.channel, frame.markets);
    for (const market of frame.markets) {
      this.applyMarket(market, frame.channel, false);
    }
    if (channelChanged) this.notifyChannel(frame.channel);
    this.publishStats();
  }

  /** Applies one delta — an update or a tombstone. */
  applyDelta(frame: DeltaFrame): void {
    if (frame.removed !== undefined && frame.removed !== '') {
      this.removeMarket(frame.removed);
      this.publishStats();
      return;
    }
    if (frame.market === undefined) return;
    this.applyMarket(frame.market, frame.channel, true);
    this.publishStats();
  }

  /**
   * Drops everything. Called when a new socket opens: the previous connection's
   * slate cannot be trusted to be complete, and a stale market left on the board
   * is worse than an empty board that fills in a moment later.
   */
  clear(): void {
    const marketIds = [...this.markets.keys()];
    const cellKeys = [...this.cells.keys()];
    const channels = [...this.channelMarkets.keys()];

    this.markets.clear();
    this.cells.clear();
    this.marketCellKeys.clear();
    this.channelMarkets.clear();
    this.channelArrays.clear();

    for (const id of marketIds) notifyIn(this.marketListeners, id);
    for (const key of cellKeys) notifyIn(this.cellListeners, key);
    for (const channel of channels) this.notifyChannel(channel);
    this.publishStats();
  }

  // ---------------------------------------------------------------------------
  // Internals
  // ---------------------------------------------------------------------------

  private applyMarket(
    market: ComputedMarket,
    channel: string,
    observed: boolean,
  ): void {
    const marketId = marketIdOf(market);
    const previous = this.markets.get(marketId);
    this.markets.set(marketId, market);

    const now = Date.now();
    const liveKeys = new Set<string>();
    let moved = 0;

    for (const book of market.books) {
      for (const quote of book.quotes) {
        const key = priceCellKey(marketId, quote.selection_id, book.slug);
        liveKeys.add(key);

        const existing = this.cells.get(key);
        if (existing !== undefined && existing.decimal === quote.decimal) {
          // A byte-identical price. NOT a change: the rail must not fire, and
          // the existing cell object is left in place so a decay still running
          // is not restarted or cut short.
          continue;
        }

        const direction: DeltaDirection | null =
          existing === undefined || !observed
            ? null
            : quote.decimal < existing.decimal
              ? 'in'
              : 'out';

        this.cells.set(key, {
          marketId,
          selectionId: quote.selection_id,
          bookSlug: book.slug,
          decimal: quote.decimal,
          previousDecimal: existing?.decimal ?? null,
          direction,
          changedAt: direction === null ? null : now,
          implied: quote.implied,
          line: quote.line,
          observedAt: quote.observed_at,
        });
        notifyIn(this.cellListeners, key);
        if (direction !== null) moved += 1;
      }
    }

    // A book that stopped quoting a selection leaves a cell behind. Removing it
    // is what makes "no book has quoted this selection" render as an empty cell
    // rather than as a price frozen at whatever it was last seen at.
    const previousKeys = this.marketCellKeys.get(marketId);
    if (previousKeys !== undefined) {
      for (const key of previousKeys) {
        if (liveKeys.has(key)) continue;
        this.cells.delete(key);
        notifyIn(this.cellListeners, key);
      }
    }
    this.marketCellKeys.set(marketId, liveKeys);

    if (this.addToChannel(channel, marketId)) this.notifyChannel(channel);

    // The document identity changed, but a subscriber only cares if something in
    // it did. `source_fingerprint` is the normalizer's hash of the market state,
    // so an equal fingerprint with no moved cell means a republication of an
    // identical market and there is nothing to re-render for.
    const substantive =
      previous === undefined ||
      moved > 0 ||
      previous.source_fingerprint !== market.source_fingerprint;
    if (substantive) notifyIn(this.marketListeners, marketId);

    if (moved > 0) {
      this.movements += 1;
      this.lastMovementAt = now;
      const movement: MarketMovement = { marketId, at: now, cells: moved };
      for (const listener of [...this.movementListeners]) {
        try {
          listener(movement);
        } catch {
          // A listener that throws must not stop the slate from applying the
          // rest of the frame.
        }
      }
    }
  }

  private removeMarket(marketId: string): void {
    if (!this.markets.delete(marketId)) return;

    const keys = this.marketCellKeys.get(marketId);
    if (keys !== undefined) {
      for (const key of keys) {
        this.cells.delete(key);
        notifyIn(this.cellListeners, key);
      }
      this.marketCellKeys.delete(marketId);
    }

    for (const [channel, members] of this.channelMarkets) {
      if (!members.delete(marketId)) continue;
      this.channelArrays.delete(channel);
      this.notifyChannel(channel);
    }

    notifyIn(this.marketListeners, marketId);
  }

  /**
   * Reconciles a channel's membership against a full snapshot: markets that were
   * on the channel and are not in the snapshot have left it.
   */
  private ensureChannel(
    channel: string,
    markets: readonly ComputedMarket[],
  ): boolean {
    const next = new Set(markets.map(marketIdOf));
    const current = this.channelMarkets.get(channel);
    if (current === undefined) {
      this.channelMarkets.set(channel, next);
      this.channelArrays.delete(channel);
      return next.size > 0;
    }
    let changed = current.size !== next.size;
    for (const id of current) {
      if (!next.has(id)) changed = true;
    }
    this.channelMarkets.set(channel, next);
    if (changed) this.channelArrays.delete(channel);
    return changed;
  }

  private addToChannel(channel: string, marketId: string): boolean {
    let members = this.channelMarkets.get(channel);
    if (members === undefined) {
      members = new Set<string>();
      this.channelMarkets.set(channel, members);
    }
    if (members.has(marketId)) return false;
    members.add(marketId);
    this.channelArrays.delete(channel);
    return true;
  }

  private notifyChannel(channel: string): void {
    this.channelArrays.delete(channel);
    notifyIn(this.channelListeners, channel);
  }

  private publishStats(): void {
    const next: SlateStats = {
      markets: this.markets.size,
      cells: this.cells.size,
      movements: this.movements,
      lastMovementAt: this.lastMovementAt,
    };
    const current = this.stats;
    if (
      current.markets === next.markets &&
      current.cells === next.cells &&
      current.movements === next.movements &&
      current.lastMovementAt === next.lastMovementAt
    ) {
      return;
    }
    this.stats = next;
    for (const notify of [...this.statsListeners]) {
      try {
        notify();
      } catch {
        // See applyMarket.
      }
    }
  }
}

// -----------------------------------------------------------------------------
// Listener plumbing
// -----------------------------------------------------------------------------

function subscribeIn(
  registry: Map<string, Set<Notify>>,
  key: string,
  notify: Notify,
): () => void {
  let set = registry.get(key);
  if (set === undefined) {
    set = new Set<Notify>();
    registry.set(key, set);
  }
  const target = set;
  target.add(notify);
  return () => {
    target.delete(notify);
    if (target.size === 0) registry.delete(key);
  };
}

function notifyIn(registry: Map<string, Set<Notify>>, key: string): void {
  const set = registry.get(key);
  if (set === undefined) return;
  for (const notify of [...set]) {
    try {
      notify();
    } catch {
      // One broken subscriber must not stop the others being told.
    }
  }
}
