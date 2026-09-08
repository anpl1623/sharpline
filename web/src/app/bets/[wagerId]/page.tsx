/**
 * `/bets/{wagerId}` — one ticket, with its cash-out.
 *
 * No server-side fetch, for the reason given at `/bets`: this endpoint is scoped
 * to the token's own user and the access token lives in memory in the browser.
 *
 * The metadata is deliberately GENERIC. Putting a ticket's stake or its
 * selections in a `<title>` would leak a customer's position into browser
 * history, into a screen-share, and into whatever indexes the tab strip — and
 * the whole reason this API answers 404 rather than 403 on somebody else's wager
 * is that a customer's tickets are not information to hand out.
 */

import type { Metadata } from 'next';

import { WagerDetail } from '@/components/bets/wager-detail';

export const metadata: Metadata = {
  title: 'Ticket',
  description:
    'One placed ticket. Play money — Sharpline is a simulation, not a licensed sportsbook.',
};

interface WagerPageProps {
  params: Promise<{ wagerId: string }>;
}

export default async function WagerPage({ params }: WagerPageProps) {
  const { wagerId } = await params;

  return (
    <div className="mx-auto flex w-full max-w-content flex-col gap-6 px-4 py-6">
      <h1 className="t-h1 text-ink">Ticket</h1>
      <WagerDetail wagerId={wagerId} />
    </div>
  );
}
