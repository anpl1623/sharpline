/**
 * `/signals/steam` — correlated, book-led line movement.
 *
 * Server-rendered first page, client pager, exactly like the +EV feed. The
 * ordering is RECENCY and the route does not offer a control to change it: a
 * steam move is actionable while the follower books are still catching up, so a
 * "biggest first" sort would look like a better product and be a worse one.
 */

import type { Metadata } from 'next';

import { SignalsUnavailable } from '@/components/signals/signals-empty';
import { SteamSignals } from '@/components/signals/steam-signals';
import { serverApi } from '@/lib/api/server';
import type { SteamSignalParams } from '@/lib/api/client';
import type { SchemaSteamSignalPage } from '@/lib/api/schema';

export const dynamic = 'force-dynamic';

export const metadata: Metadata = {
  title: 'Steam',
  description:
    'Correlated line moves led by one book and followed by others, detected on implied-probability velocity.',
};

const PAGE_LIMIT = 50;

/**
 * Two is the floor a correlated move can be defined at — a lead and one
 * follower. It is the API's own default and is sent explicitly so the value this
 * route used is the value the follow-up pages use.
 */
const MIN_PARTICIPATING_BOOKS = 2;

export default async function SteamSignalsPage() {
  const params: SteamSignalParams = {
    minParticipatingBooks: MIN_PARTICIPATING_BOOKS,
    limit: PAGE_LIMIT,
  };

  let page: SchemaSteamSignalPage;
  try {
    page = await serverApi.listSteamSignals(params);
  } catch (error) {
    return (
      <SignalsUnavailable
        error={error}
        what="The steam feed"
        retryHref="/signals/steam"
      />
    );
  }

  return (
    <section className="flex flex-col gap-4">
      <p className="max-w-prose t-body text-ink-muted">
        Steam is a jump one book takes first and others repeat within seconds.
        What separates it from ordinary drift is the SIZE of the move, not the
        correlation across books — every book watches the same market, so they
        move together whatever it does. The correlation is here to rule out a
        move one book made alone. The feed is ordered by how recent a move is
        rather than by how big it was, and everything is measured in implied
        probability points, never in decimal odds.
      </p>
      <SteamSignals
        initialData={page}
        params={params}
        windowPhrase="the last 2 hours"
      />
    </section>
  );
}
