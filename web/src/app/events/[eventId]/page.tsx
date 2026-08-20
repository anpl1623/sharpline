import { cache } from 'react';
import { notFound } from 'next/navigation';
import Link from 'next/link';
import type { Metadata } from 'next';

import { LiveEvent } from '@/components/event/live-event';
import { serverApi } from '@/lib/api/server';
import {
  developerDetail,
  isApiError,
  userFacingMessage,
} from '@/lib/api/errors';
import type { SchemaEventDetail } from '@/lib/api/schema';

/**
 * The all-leagues board.
 *
 * Spelled out rather than imported from `components/layout/league-nav`, which
 * owns the canonical helper: that module is `'use client'`, and every export of
 * a client module reaches a server component as a client REFERENCE rather than
 * as its value. A string constant imported across that boundary is not a
 * string. Keep the two in step by hand; there is no third option that does not
 * invent a module.
 */
const BOARD_HREF = '/board';

/**
 * One event: its full market tree, every quoting book, and the line movement
 * behind each selection.
 *
 * # A server component fetches; a client component keeps it live
 *
 * `serverApi` runs inside the network, with `cache: 'no-store'` and a three
 * second ceiling, so the first paint is real prices with no client waterfall
 * and no spinner. `LiveEvent` then holds `event:{id}` on the socket and updates
 * those prices in place. REST establishes the tree; the stream maintains it;
 * nothing polls (ADR 0009 D3).
 *
 * # The load is a value, not an exception
 *
 * `generateMetadata` and the page body both need this event, and both run in
 * the same render pass. `cache()` makes that one request. Returning a
 * discriminated union rather than letting the error escape means the memoised
 * value is the OUTCOME — so a 404 produces one upstream call and one decision,
 * instead of a thrown error that has to be caught identically in two places.
 */

interface EventPageProps {
  params: Promise<{ eventId: string }>;
}

type LoadResult =
  | { readonly kind: 'ok'; readonly detail: SchemaEventDetail }
  | { readonly kind: 'missing' }
  | {
      readonly kind: 'error';
      readonly message: string;
      readonly detail: string | null;
    };

const loadEvent = cache(async (eventId: string): Promise<LoadResult> => {
  try {
    const detail = await serverApi.getEvent(eventId);
    return { kind: 'ok', detail };
  } catch (error) {
    if (
      isApiError(error) &&
      (error.status === 404 || error.code === 'not_found')
    ) {
      return { kind: 'missing' };
    }
    return {
      kind: 'error',
      message: userFacingMessage(error),
      detail: developerDetail(error),
    };
  }
});

export async function generateMetadata({
  params,
}: EventPageProps): Promise<Metadata> {
  const { eventId } = await params;
  const result = await loadEvent(eventId);

  if (result.kind === 'missing') {
    return { title: 'Event not found' };
  }
  if (result.kind === 'error') {
    return { title: 'Event' };
  }

  const { event, league, sport } = result.detail;
  return {
    title: event.name,
    description: `Live odds on ${event.name} — ${league.name}, ${sport.name} — across every quoting book. Play-money simulation, not a licensed sportsbook.`,
  };
}

/**
 * There is deliberately NO `loading.tsx` in this segment.
 *
 * A `loading.tsx` wraps the segment in a Suspense boundary, and Next commits the
 * shell — headers, status 200 — before the awaited work below resolves. The body
 * that then streams in is the correct "no such event" page, so a human sees the
 * right thing, but the RESPONSE is a 200. A soft 404 is a real defect for a
 * crawler, an uptime monitor and the e2e suite alike, and it is not one the page
 * body can fix.
 *
 * The trade is a skeleton for a status code, and the status code wins easily
 * here: this fetch is one in-network hop with a 3s ceiling, so the window a
 * skeleton would have covered is a few milliseconds. `/board` keeps its
 * `loading.tsx` because that route has no 404 to get wrong.
 */
export default async function EventPage({ params }: EventPageProps) {
  const { eventId } = await params;
  const result = await loadEvent(eventId);

  if (result.kind === 'missing') {
    notFound();
  }

  if (result.kind === 'error') {
    return (
      <div className="mx-auto flex w-full max-w-content flex-col gap-4 px-4 py-12">
        <h1 className="t-h1 text-ink">This event could not be loaded</h1>
        <p className="t-body max-w-prose text-ink-2">{result.message}</p>
        {result.detail === null ? null : (
          <details className="max-w-prose">
            <summary className="t-ui text-ink-muted">Details</summary>
            {/* The request id is the only handle on what actually happened.
                It belongs in a developer-facing disclosure, never in the
                primary message. */}
            <p className="t-mono text-ink-muted">{result.detail}</p>
          </details>
        )}
        <p className="t-body text-ink-2">
          <Link href={BOARD_HREF} className="ui-transition hover:text-ink">
            Back to the board
          </Link>
        </p>
      </div>
    );
  }

  return <LiveEvent initialData={result.detail} />;
}
