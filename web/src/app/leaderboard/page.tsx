/**
 * `/leaderboard` — the public board, ranked on ROI and CLV.
 *
 * A SERVER component end to end. The board is public, nothing on it is
 * interactive beyond two links, and it holds no state — so it ships no
 * JavaScript at all, and the basis is a query parameter rather than a client
 * toggle. That also makes "the CLV board" a linkable thing, which a client-side
 * tab would not be.
 *
 * # It ranks on ROI and CLV and never on profit
 *
 * CLAUDE.md §6. The page says so in words rather than leaving the absence to be
 * noticed: a reader who finds no profit column and no explanation assumes an
 * oversight and trusts the board less, not more.
 *
 * # Nobody is named
 *
 * `users` holds an email address and nothing else, so a row carries a stable
 * one-way pseudonym derived from the account identifier. The identifier itself
 * never reaches this page.
 */

import type { Metadata } from 'next';

import { LeaderboardTable } from '@/components/analytics/leaderboard-table';
import { SignalsUnavailable } from '@/components/signals/signals-empty';
import { serverApi } from '@/lib/api/server';
import type { LeaderboardParams } from '@/lib/api/client';
import type {
  SchemaLeaderboardBasis,
  SchemaLeaderboardPage,
} from '@/lib/api/schema';

export const dynamic = 'force-dynamic';

export const metadata: Metadata = {
  title: 'Leaderboard',
  description:
    'Ranked on return on investment and closing line value — never on raw profit. Play money; Sharpline is a simulation, not a licensed sportsbook.',
};

const BASE_PATH = '/leaderboard';
const PAGE_LIMIT = 50;

/**
 * `?basis=clv` or `?basis=roi`. Anything else falls back to ROI rather than
 * being forwarded: the API would answer 400, and a mistyped bookmark should show
 * the default board rather than an error page.
 */
function parseBasis(raw: string | string[] | undefined): SchemaLeaderboardBasis {
  const value = Array.isArray(raw) ? raw[0] : raw;
  return value === 'clv' ? 'clv' : 'roi';
}

interface LeaderboardRouteProps {
  readonly searchParams: Promise<Record<string, string | string[] | undefined>>;
}

export default async function LeaderboardPage({
  searchParams,
}: LeaderboardRouteProps) {
  const resolved = await searchParams;
  const basis = parseBasis(resolved['basis']);

  // The two sample floors are the API's own defaults, not restated here. The
  // response echoes whatever was applied and the table renders it, so there is
  // one authoritative copy of each number and it is the one on screen.
  const params: LeaderboardParams = { basis, limit: PAGE_LIMIT };

  let page: SchemaLeaderboardPage;
  try {
    page = await serverApi.getLeaderboard(params);
  } catch (error) {
    return (
      <div className="mx-auto flex w-full max-w-content flex-col gap-6 px-4 py-6">
        <h1 className="t-h1 text-ink">Leaderboard</h1>
        <SignalsUnavailable
          error={error}
          what="The leaderboard"
          retryHref={`${BASE_PATH}?basis=${basis}`}
        />
      </div>
    );
  }

  return (
    <div className="mx-auto flex w-full max-w-content flex-col gap-6 px-4 py-6">
      <header className="flex flex-col gap-2">
        <h1 className="t-h1 text-ink">Leaderboard</h1>
        <p className="max-w-prose t-body text-ink-2">
          Ranked on return on investment and on closing line value. Both are on
          every row whichever one is sorted, because the interesting rows are the
          ones where they disagree — a customer with strong CLV and negative ROI
          has been taking good prices and losing anyway, which is a different
          story from a customer with the reverse.
        </p>
      </header>

      <LeaderboardTable page={page} basePath={BASE_PATH} />
    </div>
  );
}
