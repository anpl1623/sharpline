'use client';

/**
 * The TanStack Query client, configured for live data.
 *
 * # THERE IS NO POLLING, AND THAT IS THE POINT
 *
 * `refetchInterval` is false everywhere and there is no `refetchIntervalInBackground`.
 * The WebSocket is the live channel: a REST poll running beside it would fetch
 * the same change the socket is already delivering, and the two would render the
 * same update twice — once as a delta with a rail, once as a wholesale replace
 * without one. The rail would fire on some updates and not others for reasons no
 * viewer could see, and DESIGN.md's recency gradient would be noise.
 *
 * REST establishes the board; the stream keeps it. A poll is a third source of
 * truth nobody asked for.
 *
 * # staleTime 0
 *
 * Odds are live. A cached board is not a slightly-old board, it is a wrong one,
 * and it is wrong in the one way a viewer cannot detect by looking: the prices
 * are plausible. Catalogue reads — sports, leagues, books — set their own longer
 * staleTime in `queries.ts`, because those genuinely do not tick.
 *
 * # retry 1, and never on a 4xx
 *
 * A 400 or a 404 will fail identically on the next attempt; retrying it wastes a
 * round trip and delays the error state. `ApiError.isRetryable` encodes the rule
 * so it is not spelled twice.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { useState } from 'react';
import type { ReactNode } from 'react';

import { isApiError } from '@/lib/api/errors';

/** How many times a retryable failure is retried. One. */
const MAX_RETRIES = 1;

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // Live data. See the file comment.
        staleTime: 0,
        // Five minutes of cache after the last observer unmounts, so navigating
        // away from a board and straight back renders instantly and then
        // revalidates, rather than flashing a skeleton.
        gcTime: 5 * 60 * 1_000,
        // The board is the kind of surface a viewer leaves open in a tab. Coming
        // back to it must not show yesterday's prices while the socket
        // reconnects.
        refetchOnWindowFocus: true,
        refetchOnReconnect: true,
        // NO POLLING. The WebSocket is the live channel.
        refetchInterval: false,
        retry: (failureCount, error) => {
          if (failureCount >= MAX_RETRIES) return false;
          if (isApiError(error)) return error.isRetryable;
          return true;
        },
      },
      mutations: {
        retry: false,
      },
    },
  });
}

export interface QueryProviderProps {
  readonly children: ReactNode;
}

export function QueryProvider({ children }: QueryProviderProps) {
  // One client per mount, created lazily. A module-level client would be shared
  // across requests during server rendering, which leaks one user's data into
  // another's response.
  const [client] = useState(createQueryClient);

  return (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
}
