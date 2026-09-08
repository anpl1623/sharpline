'use client';

/**
 * Wager history and open positions — one page, three views over the same
 * keyset-paginated endpoint.
 *
 * # Open positions is a FILTER, not a second endpoint
 *
 * `?status=placed&status=open` is "everything still running", and the filter's
 * values union. Splitting open positions onto their own route would put two
 * pagers over one ordering and give the same ticket two URLs, which is how a
 * "cash out" control ends up on a page whose copy of the ticket is a minute old.
 *
 * # A short page is not the last page
 *
 * The status filter is applied to the page the server SCANNED, so a filtered
 * page can hold fewer than `limit` rows while `has_more` is still true. The
 * "Load more" control below reads `has_more` and never the row count — stopping
 * at a short page is the bug this endpoint's documentation exists to warn about.
 *
 * # There is no total and there will not be one
 *
 * No "12 of 340", no page numbers. Counting an unbounded, continuously-written
 * set costs a full scan on every page and the number is stale before it is
 * serialised. The pager says whether there is more and nothing else.
 *
 * # Nothing here is on the socket
 *
 * A placed ticket's prices are FROZEN and its status changes when settlement
 * says so, which is a Kafka event this browser is not subscribed to. So the list
 * is a REST read that the customer refreshes, and it does not pretend otherwise
 * with a spinner that never resolves. The one thing that does update it is this
 * client's own actions — placing and cashing out both invalidate it.
 */

import { useState } from 'react';
import { usePathname } from 'next/navigation';
import { useInfiniteQuery, useQuery } from '@tanstack/react-query';

import { Button } from '@/components/ui';
import {
  balanceQueryOptions,
  wagersInfiniteQueryOptions,
} from '@/lib/api/queries';
import type { SchemaWagerStatus } from '@/lib/api/schema';
import {
  RUNNING_WAGER_STATUSES,
  SETTLED_WAGER_STATUSES,
} from '@/lib/betting/ticket';
import { useAccessToken, useAuth } from '@/lib/store/auth';
import { BalanceSummary } from './balance-summary';
import { BetsEmpty, BetsSignedOut, BetsUnavailable } from './bets-empty';
import { WagerCard } from './wager-card';

type View = 'open' | 'settled' | 'all';

const VIEWS: readonly { readonly id: View; readonly label: string }[] = [
  { id: 'open', label: 'Open' },
  { id: 'settled', label: 'Settled' },
  { id: 'all', label: 'All' },
];

function statusesFor(view: View): readonly SchemaWagerStatus[] | undefined {
  switch (view) {
    case 'open':
      return RUNNING_WAGER_STATUSES;
    case 'settled':
      return SETTLED_WAGER_STATUSES;
    // Omitted rather than sent as every status: an absent filter is a different
    // request from one that happens to name all seven, and it is the one the
    // server's own fast path is written for.
    case 'all':
    default:
      return undefined;
  }
}

/** The page size. Big enough that a filtered page is rarely empty. */
const PAGE_LIMIT = 25;

export function WagerHistory() {
  const pathname = usePathname();
  const accessToken = useAccessToken();

  // The same three-state resolution the account chip uses: the server renders
  // signed out because it has no storage, and the client only learns who is
  // signed in after the store rehydrates and redeems its refresh token. Showing
  // the signed-out panel during either step would be a visible lie that flips to
  // a list of somebody's bets a moment later.
  const hydrated = useAuth((state) => state.hydrated);
  const status = useAuth((state) => state.status);
  const hasStoredSession = useAuth(
    (state) => state.refreshToken !== null && state.refreshToken !== '',
  );
  const signedIn = accessToken !== null && accessToken !== '';
  const resolving =
    !hydrated ||
    status === 'authenticating' ||
    status === 'refreshing' ||
    (!signedIn && hasStoredSession);

  const [view, setView] = useState<View>('open');

  const balance = useQuery(balanceQueryOptions(accessToken));
  const wagers = useInfiniteQuery(
    wagersInfiniteQueryOptions(accessToken, {
      status: statusesFor(view),
      limit: PAGE_LIMIT,
    }),
  );

  if (resolving) {
    return (
      <p className="t-ui text-ink-muted" role="status">
        Checking your session…
      </p>
    );
  }

  if (!signedIn) return <BetsSignedOut pathname={pathname} />;

  const tickets = wagers.data?.pages.flatMap((page) => page.data) ?? [];
  const failed = wagers.isError && tickets.length === 0;

  return (
    <div className="flex flex-col gap-6">
      <BalanceSummary balance={balance.data} loading={balance.isLoading} />

      <div
        role="group"
        aria-label="Which bets to show"
        className="flex flex-wrap items-center gap-1"
      >
        {VIEWS.map((option) => {
          const active = option.id === view;
          return (
            <Button
              key={option.id}
              type="button"
              size="sm"
              variant={active ? 'default' : 'ghost'}
              aria-pressed={active}
              onClick={() => {
                setView(option.id);
              }}
            >
              {option.label}
            </Button>
          );
        })}
      </div>

      {failed ? (
        <BetsUnavailable
          error={wagers.error}
          onRetry={() => {
            void wagers.refetch();
          }}
        />
      ) : wagers.isPending ? (
        <p className="t-ui text-ink-muted" role="status">
          Loading your bets…
        </p>
      ) : tickets.length === 0 ? (
        <BetsEmpty scope={view} />
      ) : (
        <ul className="flex flex-col gap-3">
          {tickets.map((wager) => (
            <li key={wager.id}>
              <WagerCard wager={wager} />
            </li>
          ))}
        </ul>
      )}

      {/* Reads `has_more` through `hasNextPage`, never the row count. */}
      {wagers.hasNextPage ? (
        <div>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={wagers.isFetchingNextPage}
            onClick={() => {
              void wagers.fetchNextPage();
            }}
          >
            {wagers.isFetchingNextPage ? 'Loading…' : 'Load more'}
          </Button>
        </div>
      ) : null}
    </div>
  );
}
