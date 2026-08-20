'use client';

/**
 * The client half of the event detail page.
 *
 * The server component fetched the tree and handed it here; this holds
 * `event:{id}` for as long as the page is mounted, and every price cell below
 * subscribes to its own `(market, selection, book)` triple against the live
 * slate. Nothing in this component re-renders when a price moves — the cells do
 * that themselves, which is what makes DESIGN.md's "do not re-render the row"
 * true here as well as on the board.
 *
 * # There is no live region here
 *
 * The application mounts exactly ONE `aria-live` region, in the root layout
 * (`components/live/live-announcer.tsx`), throttled to one batched sentence
 * every five seconds. A second one on this page would double every
 * announcement, so this page deliberately mounts none.
 *
 * # If the socket is down, the page does not blank
 *
 * The tree stays on screen with the prices the server rendered. They stop
 * moving, the shell's status rail says the stream is down, and the fair value
 * panel says it has nothing live to show. That is the honest rendering of a
 * dead socket and the only one a reader can act on.
 */

import { EventHeader } from '@/components/event/event-header';
import { MarketTree } from '@/components/event/market-tree';
import { useEventLive } from '@/components/event/use-event-live';
import type { SchemaEventDetail } from '@/lib/api/schema';

export interface LiveEventProps {
  /** The server-rendered tree. Real prices on first paint, no client waterfall. */
  readonly initialData: SchemaEventDetail;
}

export function LiveEvent({ initialData }: LiveEventProps) {
  const { liveMarketIds } = useEventLive(initialData.event.id);

  return (
    <div className="mx-auto flex w-full max-w-content flex-col gap-8 px-4 py-6">
      <EventHeader
        sport={initialData.sport}
        league={initialData.league}
        event={initialData.event}
        asOf={initialData.as_of}
      />

      <MarketTree
        eventId={initialData.event.id}
        eventName={initialData.event.name}
        markets={initialData.markets}
        asOf={initialData.as_of}
        liveMarketIds={liveMarketIds}
      />
    </div>
  );
}
