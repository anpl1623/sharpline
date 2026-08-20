'use client';

/**
 * The one client boundary at the root of the tree.
 *
 * Mount order is load-bearing and is stated in the data layer's own contract:
 * `QueryProvider` -> `StreamProvider`, with the two persisted stores rehydrated
 * once, high up.
 *
 *   - `QueryProvider` outermost because it owns a per-mount `QueryClient` and
 *     nothing below it may construct a second one.
 *   - `StreamProvider` inside it because the socket is the *live* channel over a
 *     REST-established board: a component reads its initial rows through TanStack
 *     Query and is then updated in place by the stream. It is also the layer that
 *     reads the auth store, so it must sit under nothing that could remount it.
 *   - `StoreHydration` innermost, and it is a component rather than two hook
 *     calls in `Providers` itself. Both hydration hooks flip a boolean after
 *     mount; isolating them here means that flip re-renders one component that
 *     returns its `children` element unchanged, so React bails out of the whole
 *     subtree instead of reconciling the entire app.
 *
 * The stream connects ANONYMOUSLY. `authenticate` is left at its default of
 * false: market data is public, the phase-7 surface is entirely public, and
 * presenting a rotating access token in the handshake would reconnect — and
 * re-snapshot every subscribed channel — every few minutes for no benefit.
 */

import type { ReactNode } from 'react';

import { QueryProvider } from '@/lib/query/provider';
import { useAuthHydration } from '@/lib/store/auth';
import { usePreferencesHydration } from '@/lib/store/preferences';
import { StreamProvider } from '@/lib/ws/provider';

function StoreHydration({ children }: { readonly children: ReactNode }) {
  // Both are no-ops on the server and read `localStorage` once after mount. The
  // stores ship with `skipHydration`, so the first client render matches the
  // server render exactly and only the second one carries stored preferences —
  // which is why the odds-format toggle cannot mismatch during hydration.
  usePreferencesHydration();
  useAuthHydration();

  return <>{children}</>;
}

export interface ProvidersProps {
  readonly children: ReactNode;
}

export function Providers({ children }: ProvidersProps) {
  return (
    <QueryProvider>
      <StreamProvider>
        <StoreHydration>{children}</StoreHydration>
      </StreamProvider>
    </QueryProvider>
  );
}
