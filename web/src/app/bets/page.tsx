/**
 * `/bets` — wager history and open positions.
 *
 * A server component that renders a client one and does no fetching of its own,
 * which is a deliberate departure from how `/board` and `/events/[id]` work.
 * Those read the API server-side so the first paint is real prices with no
 * spinner. This route cannot: every endpoint under it is scoped to the token's
 * own user, the access token lives IN MEMORY IN THE BROWSER and is never
 * persisted, and a server render has no credential to present.
 *
 * That is not a limitation to work around. Giving the server a way to read a
 * customer's wagers would mean a cookie-bearing API — a CSRF surface that would
 * have to be defended with a second mechanism — and the token placement is
 * argued at length in `lib/store/auth.ts`. The cost is a loading state on one
 * page, and it is paid here rather than in the auth design.
 */

import type { Metadata } from 'next';

import { WagerHistory } from '@/components/bets/wager-history';

export const metadata: Metadata = {
  title: 'Your bets',
  description:
    'Open positions and settled tickets. Play money — Sharpline is a simulation, not a licensed sportsbook.',
};

export default function BetsPage() {
  return (
    <div className="mx-auto flex w-full max-w-content flex-col gap-6 px-4 py-6">
      <h1 className="t-h1 text-ink">Your bets</h1>
      <WagerHistory />
    </div>
  );
}
