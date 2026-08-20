'use client';

/**
 * The React binding for the stream: one socket for the app, one slate, and hooks
 * with the granularity DESIGN.md requires.
 *
 * # The provider is not allowed to break the page
 *
 * There is no socket on the server, no socket before mount, and a failure to
 * connect does NOT blank anything. The board is rendered from REST first; the
 * stream only updates it in place. If the gateway is down the prices on screen
 * stay on screen, they stop moving, and the status rail says the stream is down
 * — which is the honest rendering of that situation and the one a viewer can act
 * on.
 *
 * # Granularity
 *
 *   useStreamStatus()   the whole connection state. One subscriber: the rail.
 *   useComputedMarket() one market's document. Re-renders on a real change only.
 *   usePriceCell()      one price. THE BOARD CELL HOOK. Nothing above it
 *                       re-renders when it ticks, which is what makes
 *                       "do not re-render the row" true rather than aspirational.
 *
 * # The stream is anonymous by default, deliberately
 *
 * Market data is public and the phase-7 surface is entirely public, so the
 * socket connects with no credential. `authenticate` exists because the client
 * supports a credential, but it is OFF by default: the access token rotates
 * every few minutes and the credential is presented in the handshake, so
 * attaching it would reconnect — and re-snapshot every subscribed channel — on
 * every rotation, buying nothing.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from 'react';
import type { ReactNode } from 'react';

import {
  IDLE_STREAM_STATE,
  WsClient,
  describeStream,
} from '@/lib/ws/client';
import type {
  StreamDescription,
  StreamState,
  WsClientOptions,
} from '@/lib/ws/client';
import { MarketSlate } from '@/lib/ws/store';
import type { DeltaDirection, PriceCell, SlateStats } from '@/lib/ws/store';
import type { Channel, ComputedMarket } from '@/lib/ws/protocol';
import { useAccessToken } from '@/lib/store/auth';

/**
 * How long a channel with no subscribers is kept before it is unsubscribed.
 *
 * React 19 Strict Mode mounts, unmounts and remounts every effect, and a route
 * change swaps one board for another that often wants the same channel. Both
 * would otherwise produce an unsubscribe immediately followed by a subscribe,
 * and the second one costs a full snapshot of the channel. A quarter of a second
 * of grace absorbs both.
 */
const CHANNEL_RELEASE_DELAY_MS = 250;

/**
 * The live-region cadence. ONE announcement per five seconds, batching every
 * market that moved in the window into a single sentence.
 *
 * Announcing per tick is the single worst thing this interface could do to a
 * screen reader user: a board with a hundred moving markets would produce a
 * continuous unintelligible stream and make the page unusable. Individual
 * changes are exposed through `aria-describedby` on focus instead, where they
 * are read on demand rather than pushed.
 */
export const ANNOUNCE_INTERVAL_MS = 5_000;

interface StreamContextValue {
  readonly client: WsClient;
  readonly slate: MarketSlate;
  readonly acquire: (channel: Channel) => void;
  readonly release: (channel: Channel) => void;
}

const StreamContext = createContext<StreamContextValue | null>(null);

export interface StreamProviderProps {
  readonly children: ReactNode;
  /**
   * Present the access token in the handshake. Off by default; see the file
   * comment. Phase 8 turns this on for a surface that actually needs identity.
   */
  readonly authenticate?: boolean;
  /** Escape hatch for tests. */
  readonly options?: WsClientOptions;
}

export function StreamProvider({
  children,
  authenticate = false,
  options,
}: StreamProviderProps) {
  // Constructed lazily and exactly once. The constructor touches no browser
  // global, so this is safe during server rendering; `connect()` is the call
  // that needs a window and it guards itself.
  const [client] = useState(() => new WsClient(options ?? {}));
  const [slate] = useState(() => new MarketSlate());

  const refCounts = useRef(new Map<Channel, number>());
  const pendingRelease = useRef(new Map<Channel, ReturnType<typeof setTimeout>>());

  const accessToken = useAccessToken();

  // Wire the socket into the slate. Registered before `connect()` so no frame
  // can arrive between the two.
  useEffect(() => {
    // Captured here rather than read in the cleanup: the ref identity is stable
    // for the provider's lifetime, but reading `.current` inside a cleanup is
    // the pattern the exhaustive-deps rule cannot prove safe.
    const releaseTimers = pendingRelease.current;
    const offReset = client.on('reset', () => {
      // A new connection cannot be assumed to continue the previous one's
      // slate. Clearing and re-snapshotting is cheap; a market left behind from
      // a dead connection is a price that will never move again and has nothing
      // on screen to say so.
      slate.clear();
    });
    const offSnapshot = client.on('snapshot', (frame) => {
      slate.applySnapshot(frame);
    });
    const offDelta = client.on('delta', (frame) => {
      slate.applyDelta(frame);
    });

    client.connect();

    return () => {
      offReset();
      offSnapshot();
      offDelta();
      for (const timer of releaseTimers.values()) clearTimeout(timer);
      releaseTimers.clear();
      client.close();
    };
  }, [client, slate]);

  useEffect(() => {
    client.setAccessToken(authenticate ? accessToken : null);
  }, [client, authenticate, accessToken]);

  /**
   * Reference-counted channel acquisition, so two components watching the same
   * league do not fight over the subscription.
   */
  const acquire = useCallback(
    (channel: Channel) => {
      const pending = pendingRelease.current.get(channel);
      if (pending !== undefined) {
        clearTimeout(pending);
        pendingRelease.current.delete(channel);
      }
      const next = (refCounts.current.get(channel) ?? 0) + 1;
      refCounts.current.set(channel, next);
      if (next === 1) client.subscribe([channel]);
    },
    [client],
  );

  const release = useCallback(
    (channel: Channel) => {
      const current = refCounts.current.get(channel) ?? 0;
      const next = Math.max(0, current - 1);
      refCounts.current.set(channel, next);
      if (next > 0) return;
      if (pendingRelease.current.has(channel)) return;
      const timer = setTimeout(() => {
        pendingRelease.current.delete(channel);
        if ((refCounts.current.get(channel) ?? 0) > 0) return;
        refCounts.current.delete(channel);
        client.unsubscribe([channel]);
      }, CHANNEL_RELEASE_DELAY_MS);
      pendingRelease.current.set(channel, timer);
    },
    [client],
  );

  const value = useMemo<StreamContextValue>(
    () => ({ client, slate, acquire, release }),
    [client, slate, acquire, release],
  );

  return (
    <StreamContext.Provider value={value}>{children}</StreamContext.Provider>
  );
}

function useStreamContext(): StreamContextValue | null {
  return useContext(StreamContext);
}

/**
 * The socket, or null outside a provider.
 *
 * Returning null rather than throwing is deliberate: a component rendered in a
 * story, a test, or above the provider should degrade to its REST-rendered state
 * rather than crash the tree.
 */
export function useStreamClient(): WsClient | null {
  return useStreamContext()?.client ?? null;
}

/** The slate, or null outside a provider. */
export function useMarketSlate(): MarketSlate | null {
  return useStreamContext()?.slate ?? null;
}

// -----------------------------------------------------------------------------
// Status
// -----------------------------------------------------------------------------

/** The whole connection state, for the status rail and the mobile pip. */
export function useStreamStatus(): StreamState {
  const client = useStreamClient();

  const subscribe = useCallback(
    (notify: () => void) => (client === null ? () => {} : client.on('state', notify)),
    [client],
  );
  const getSnapshot = useCallback(
    () => (client === null ? IDLE_STREAM_STATE : client.getState()),
    [client],
  );
  const getServerSnapshot = useCallback(() => IDLE_STREAM_STATE, []);

  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

/**
 * The DESIGN.md pip tone and its accessible name. The name is verbatim from the
 * design system's table — render it as the pip's accessible name, do not
 * paraphrase it.
 */
export function useStreamDescription(): StreamDescription {
  const state = useStreamStatus();
  return useMemo(() => describeStream(state), [state]);
}

/** Slate counts, for the engineering layer. */
export function useSlateStats(): SlateStats {
  const slate = useMarketSlate();
  const empty = useMemo<SlateStats>(
    () => ({ markets: 0, cells: 0, movements: 0, lastMovementAt: null }),
    [],
  );

  const subscribe = useCallback(
    (notify: () => void) => (slate === null ? () => {} : slate.subscribeToStats(notify)),
    [slate],
  );
  const getSnapshot = useCallback(
    () => (slate === null ? empty : slate.getStats()),
    [slate, empty],
  );
  const getServerSnapshot = useCallback(() => empty, [empty]);

  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

// -----------------------------------------------------------------------------
// Subscriptions
// -----------------------------------------------------------------------------

/**
 * Holds one channel for the lifetime of the component.
 *
 * Reference-counted, so mounting two components on `league:syn-sgl` subscribes
 * once and unsubscribes once. Pass null to hold nothing — useful for a page
 * whose channel is not known until its data loads.
 */
export function useChannelSubscription(channel: Channel | null | undefined): void {
  const context = useStreamContext();
  const acquire = context?.acquire;
  const release = context?.release;

  useEffect(() => {
    if (channel === null || channel === undefined || channel === '') return;
    if (acquire === undefined || release === undefined) return;
    acquire(channel);
    return () => {
      release(channel);
    };
  }, [channel, acquire, release]);
}

/**
 * Holds a set of channels.
 *
 * The array is joined into a dependency key, so a caller may build it inline
 * without memoising: `useChannelSubscriptions(ids.map(eventChannel))` does not
 * resubscribe on every render.
 */
export function useChannelSubscriptions(
  channels: readonly Channel[] | null | undefined,
): void {
  const context = useStreamContext();
  const acquire = context?.acquire;
  const release = context?.release;
  const key = (channels ?? []).join('\n');

  useEffect(() => {
    if (acquire === undefined || release === undefined) return;
    const list = key === '' ? [] : key.split('\n');
    for (const channel of list) acquire(channel);
    return () => {
      for (const channel of list) release(channel);
    };
  }, [key, acquire, release]);
}

// -----------------------------------------------------------------------------
// Data
// -----------------------------------------------------------------------------

/** One market's live document, or undefined if the slate does not hold it. */
export function useComputedMarket(
  marketId: string | null | undefined,
): ComputedMarket | undefined {
  const slate = useMarketSlate();

  const subscribe = useCallback(
    (notify: () => void) => {
      if (slate === null || marketId === null || marketId === undefined) {
        return () => {};
      }
      return slate.subscribeToMarket(marketId, notify);
    },
    [slate, marketId],
  );
  const getSnapshot = useCallback(() => {
    if (slate === null || marketId === null || marketId === undefined) {
      return undefined;
    }
    return slate.getMarket(marketId);
  }, [slate, marketId]);
  const getServerSnapshot = useCallback(() => undefined, []);

  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

/**
 * THE BOARD CELL HOOK. One price, its direction, and when it changed.
 *
 * Subscribes to exactly one `(market, selection, book)` triple, so a tick on
 * this price re-renders this cell and nothing else — not the row, not the table.
 * Returns undefined until the stream has delivered a price for it, which is the
 * correct state for a board rendered from REST that the socket has not caught up
 * with yet: the cell keeps showing its REST price.
 */
export function usePriceCell(
  marketId: string | null | undefined,
  selectionId: string | null | undefined,
  bookSlug: string | null | undefined,
): PriceCell | undefined {
  const slate = useMarketSlate();

  const subscribe = useCallback(
    (notify: () => void) => {
      if (slate === null || !marketId || !selectionId || !bookSlug) {
        return () => {};
      }
      return slate.subscribeToCell(marketId, selectionId, bookSlug, notify);
    },
    [slate, marketId, selectionId, bookSlug],
  );
  const getSnapshot = useCallback(() => {
    if (slate === null || !marketId || !selectionId || !bookSlug) {
      return undefined;
    }
    return slate.getCell(marketId, selectionId, bookSlug);
  }, [slate, marketId, selectionId, bookSlug]);
  const getServerSnapshot = useCallback(() => undefined, []);

  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

/**
 * What a price cell actually renders.
 *
 * The board is established from REST and maintained by the stream, so a cell has
 * two possible sources and must prefer the live one WITHOUT flickering back to
 * the REST value when the stream has not caught up. This resolves that once, so
 * four surfaces do not each resolve it slightly differently.
 */
export interface LivePrice {
  /** The canonical decimal price, or null when nothing has quoted it. */
  readonly decimal: number | null;
  /** Null when the price has not moved since this client started watching. */
  readonly direction: DeltaDirection | null;
  /** Epoch ms of the last observed change. Drives the delta rail's decay. */
  readonly changedAt: number | null;
  /** The provider instant, for staleness against the server's anchor. */
  readonly observedAt: string | null;
  readonly line: number | null;
  /** True when the value came from the stream rather than the REST fallback. */
  readonly live: boolean;
}

export interface LivePriceInput {
  readonly marketId: string | null | undefined;
  readonly selectionId: string | null | undefined;
  readonly bookSlug: string | null | undefined;
  /** The REST price for this cell, shown until the stream delivers its own. */
  readonly fallbackDecimal?: number | null | undefined;
  readonly fallbackObservedAt?: string | null | undefined;
  readonly fallbackLine?: number | null | undefined;
}

/**
 * The stream value for a cell, falling back to the REST value.
 *
 * Subscribes to exactly one price, so a tick re-renders this cell alone.
 */
export function useLivePrice(input: LivePriceInput): LivePrice {
  const cell = usePriceCell(
    input.marketId,
    input.selectionId,
    input.bookSlug,
  );

  if (cell !== undefined) {
    return {
      decimal: cell.decimal,
      direction: cell.direction,
      changedAt: cell.changedAt,
      observedAt: cell.observedAt,
      line: cell.line,
      live: true,
    };
  }

  return {
    decimal: input.fallbackDecimal ?? null,
    direction: null,
    changedAt: null,
    observedAt: input.fallbackObservedAt ?? null,
    line: input.fallbackLine ?? null,
    live: false,
  };
}

/** The market ids the slate currently holds for a channel. */
export function useChannelMarketIds(
  channel: Channel | null | undefined,
): readonly string[] {
  const slate = useMarketSlate();
  const empty = useMemo<readonly string[]>(() => [], []);

  const subscribe = useCallback(
    (notify: () => void) => {
      if (slate === null || channel === null || channel === undefined) {
        return () => {};
      }
      return slate.subscribeToChannel(channel, notify);
    },
    [slate, channel],
  );
  const getSnapshot = useCallback(() => {
    if (slate === null || channel === null || channel === undefined) {
      return empty;
    }
    return slate.getMarketIdsForChannel(channel);
  }, [slate, channel, empty]);
  const getServerSnapshot = useCallback(() => empty, [empty]);

  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

// -----------------------------------------------------------------------------
// The throttled announcer
// -----------------------------------------------------------------------------

export interface MovedAnnouncement {
  /** "14 markets moved", or the empty string when nothing has moved yet. */
  readonly message: string;
  /**
   * Increments with every announcement.
   *
   * Use it as the live region's React `key`. Assistive technology does not
   * reliably re-announce a region whose text is replaced with the SAME text, and
   * two consecutive five-second windows can easily both read "14 markets moved".
   * Keying the node forces a fresh element and a fresh announcement.
   */
  readonly key: number;
}

const NO_ANNOUNCEMENT: MovedAnnouncement = { message: '', key: 0 };

/**
 * The throttled `aria-live="polite"` sentence: at most one per five seconds,
 * batching every market that moved in the window.
 *
 * Silent when nothing moved — an announcement that fires on an idle board is
 * noise, and the region must not repeat itself into a screen reader for a page
 * that is doing nothing.
 */
export function useMarketsMovedAnnouncement(): MovedAnnouncement {
  const slate = useMarketSlate();
  const [announcement, setAnnouncement] =
    useState<MovedAnnouncement>(NO_ANNOUNCEMENT);

  useEffect(() => {
    if (slate === null) return;

    const moved = new Set<string>();
    const unsubscribe = slate.subscribeToMovement((movement) => {
      moved.add(movement.marketId);
    });

    const timer = setInterval(() => {
      if (moved.size === 0) return;
      const count = moved.size;
      moved.clear();
      setAnnouncement((previous) => ({
        message:
          count === 1 ? '1 market moved' : `${String(count)} markets moved`,
        key: previous.key + 1,
      }));
    }, ANNOUNCE_INTERVAL_MS);

    return () => {
      unsubscribe();
      clearInterval(timer);
    };
  }, [slate]);

  return announcement;
}

export type {
  DeltaDirection,
  PriceCell,
  SlateStats,
  StreamDescription,
  StreamState,
};
