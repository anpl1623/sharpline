'use client';

/**
 * The event detail page's client-side plumbing: the channel subscription, the
 * imperative delta rail, and the one-line time-zone resolution every absolute
 * timestamp on this page goes through.
 *
 * # Why so little of this touches the stream's connection state
 *
 * `useStreamStatus()` notifies on EVERY frame — a delta, an ack, a pong. A
 * component that calls it re-renders several times a second, and everything
 * under it re-renders with it. On the board that would be catastrophic; on this
 * page it would still defeat DESIGN.md's "per-cell decay timers; do not
 * re-render the row".
 *
 * So the page subscribes to its channel here and reads nothing else from the
 * connection. Two consequences, both deliberate:
 *
 *   - Staleness on this page is anchored to a payload's OWN instant — the REST
 *     `as_of` that came with the tree, the comparison's `as_of`, or the
 *     server-computed `age_seconds` on a streamed record. Never to a ticking
 *     client anchor. A number that is consistent with the payload it sits
 *     beside is more useful than one that counts up, and it costs no renders.
 *   - The only component that reads the connection state is
 *     `fair-value-panel.tsx`, which is a leaf, is mounted on demand, and has to
 *     know: it renders numbers that exist only on the stream, and it must say
 *     so rather than show stale ones as if they were live.
 *
 * # There is no delta rail in this file, on purpose
 *
 * There is exactly ONE rail implementation in this repository and it lives in
 * `components/board/price-cell.tsx`. The event detail page renders that same
 * component. A second imperative restart here would be a second thing to keep
 * in step with globals.css and with DESIGN.md, for no gain.
 *
 * # Structure comes from REST, movement comes from the socket
 *
 * ADR 0009 D3. This module deliberately does NOT merge markets the stream knows
 * about into the tree: the tree's shape — which markets exist, which selections
 * hang off them, in which display order — is the REST payload's to state.
 * `liveMarketIds` is exposed so a panel can say whether the socket is carrying
 * its market, not so the tree can be rebuilt from the socket.
 */

import { useMemo } from 'react';

import { useDisplayTimeZone } from '@/lib/client-value';
import { eventChannel } from '@/lib/ws/protocol';
import type { Channel } from '@/lib/ws/protocol';
import { useChannelMarketIds, useChannelSubscription } from '@/lib/ws/provider';

export interface EventLive {
  /** `event:{id}` — the channel this page holds for its lifetime. */
  readonly channel: Channel;
  /** Market ids the socket is currently carrying on that channel. */
  readonly liveMarketIds: readonly string[];
}

/**
 * Holds `event:{eventId}` for the lifetime of the component and reports which
 * markets the socket is carrying.
 *
 * The subscription is reference counted by the provider, so mounting this twice
 * on the same event subscribes once.
 */
export function useEventLive(eventId: string): EventLive {
  const channel = useMemo(() => eventChannel(eventId), [eventId]);
  useChannelSubscription(channel);

  // Notifies only when the id SET changes — a price tick does not change
  // membership, so this does not re-render the page on movement.
  const liveMarketIds = useChannelMarketIds(channel);

  return useMemo<EventLive>(
    () => ({ channel, liveMarketIds }),
    [channel, liveMarketIds],
  );
}

/**
 * The reader's own time zone, resolved AFTER mount.
 *
 * Every formatter in `@/lib/time` is locale-pinned and defaults to UTC so a
 * server render and the first client render produce the same string. Reading
 * the browser's zone during render would make them differ and hydration would
 * warn — and worse, the first paint would show a time nobody asked for. So the
 * page renders UTC, then swaps to local on the next commit.
 */
export { useDisplayTimeZone as useLocalTimeZone };
