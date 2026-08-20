'use client';

/**
 * Values that only exist in a browser, read without a setState-in-effect.
 *
 * Two things on this surface are knowable only on the client — the reader's
 * time zone, and the wall-clock instant a chart's history window is measured
 * back from. Both were originally read in a mount effect that called
 * `setState`, which React's compiler lint correctly rejects: it renders once
 * with a placeholder, commits, then re-renders, and on a board that is a whole
 * extra pass over every row for a value that never changes again.
 *
 * `useSyncExternalStore` is the sanctioned answer. It takes a server snapshot
 * and a client snapshot, uses the server's for the SSR pass *and* the hydration
 * pass — so the two agree and hydration cannot warn — then re-renders once with
 * the client's. Same one extra render, but React schedules it rather than an
 * effect, and there is no state to keep in sync.
 *
 * `getSnapshot` must return a value that is `Object.is`-stable across calls or
 * React loops forever. The time zone is a string and is naturally stable; the
 * mount instant is memoised in a ref per component instance.
 */

import { useCallback, useRef, useSyncExternalStore } from 'react';

import { UTC, resolveLocalTimeZone } from '@/lib/time';

const noop = (): void => {};

/**
 * A store that never emits. These values are read once and are constant for the
 * lifetime of the document, so there is nothing to subscribe to.
 */
function subscribeNever(): () => void {
  return noop;
}

function serverTimeZone(): string {
  return UTC;
}

/**
 * The reader's own IANA time zone, or `UTC` on the server and during hydration.
 *
 * Every formatter in `@/lib/time` is locale-pinned and defaults to UTC exactly
 * so the server render and the hydration render produce the same string. This
 * hook is what swaps them to local afterwards.
 */
export function useDisplayTimeZone(): string {
  return useSyncExternalStore(subscribeNever, resolveLocalTimeZone, serverTimeZone);
}

function serverInstant(): null {
  return null;
}

/**
 * The wall-clock instant this component first rendered in a browser, or `null`
 * on the server and during hydration.
 *
 * Stable for the life of the component: a value recomputed every render would
 * change a query key every render and refetch forever.
 */
export function useMountInstant(): number | null {
  const captured = useRef<number | null>(null);

  const clientInstant = useCallback((): number => {
    captured.current ??= Date.now();
    return captured.current;
  }, []);

  return useSyncExternalStore(subscribeNever, clientInstant, serverInstant);
}
