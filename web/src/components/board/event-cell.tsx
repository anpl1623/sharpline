'use client';

/**
 * The game cell — the row's identity, and its row header.
 *
 * Two competitors and one line of meta, in the order the market columns stack
 * them: AWAY ABOVE HOME. That correspondence is the whole reason the board is
 * readable; a moneyline column whose first cell is the home side while this cell
 * reads away-first is a board nobody can use, so both orders come from the same
 * place (`orderedCompetitors` / `orderedSelections`).
 *
 * Below 768px the two names stack, which is exactly what the 56px row height in
 * DESIGN.md's mobile pass buys. At or above it they sit on one line separated by
 * an `@`, and the row returns to 36px.
 *
 * Nothing here is invented. An event with no competitors — an outright — renders
 * its own name, because that is the only name the payload carries for it.
 */

import Link from 'next/link';

import type { SchemaEventSummary } from '@/lib/api/schema';
import { formatDayAndTime, toDateTimeAttribute } from '@/lib/time';
import { orderedCompetitors } from './use-board-live';
import type { BoardCompetitor } from './use-board-live';

export interface EventCellProps {
  readonly event: SchemaEventSummary;
  /** UTC until the viewer's own zone has been resolved after mount. */
  readonly timeZone: string;
}

export function EventCell({ event, timeZone }: EventCellProps) {
  const competitors = orderedCompetitors(event);
  const away = competitors[0];
  const home = competitors[1];

  return (
    <Link
      href={`/events/${event.id}`}
      className="flex min-w-0 flex-col justify-center gap-[1px] rounded-price"
    >
      <span className="flex min-w-0 flex-col md:flex-row md:items-baseline md:gap-1">
        {away === undefined ? (
          <span className="t-ui truncate text-ink">{event.name}</span>
        ) : (
          <>
            <Side competitor={away} />
            {home === undefined ? null : (
              <>
                <span className="sr-only"> at </span>
                <span aria-hidden="true" className="hidden shrink-0 text-ink-faint md:inline">
                  @
                </span>
                <Side competitor={home} />
              </>
            )}
          </>
        )}
      </span>
      <EventMeta event={event} timeZone={timeZone} />
    </Link>
  );
}

function Side({ competitor }: { readonly competitor: BoardCompetitor }) {
  return (
    <span className="flex min-w-0 items-baseline gap-1">
      <span className="t-ui truncate text-ink">{competitor.name}</span>
      {competitor.score === null ? null : (
        <span className="t-price-sm shrink-0 tabular text-ink-2">
          <span className="sr-only">score </span>
          {competitor.score}
        </span>
      )}
    </span>
  );
}

/**
 * The clock, as `mm:ss`.
 *
 * Local to this file on purpose: `formatCompactDuration` renders one unit ("7m")
 * because it is built for staleness, and a game clock that says "7m" where the
 * scoreboard says 07:14 is a different fact rendered in the same place.
 */
function formatClock(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined) return '';
  if (!Number.isFinite(seconds) || seconds < 0) return '';
  const total = Math.floor(seconds);
  const minutes = Math.floor(total / 60);
  const rest = total % 60;
  return `${String(minutes).padStart(2, '0')}:${String(rest).padStart(2, '0')}`;
}

function EventMeta({
  event,
  timeZone,
}: {
  readonly event: SchemaEventSummary;
  readonly timeZone: string;
}) {
  if (event.status === 'live') {
    const clock = event.clock;
    // The period's sport-specific name — quarter, half, inning — is not on the
    // payload, so it is not guessed at. `P2` is the neutral rendering of the
    // number the API actually sends.
    const period =
      clock === undefined || clock.period === null || clock.period === undefined
        ? ''
        : `P${String(clock.period)}`;
    const elapsed = formatClock(clock?.elapsed_seconds);
    const detail = [period, elapsed].filter((part) => part !== '').join(' · ');

    return (
      <span className="t-label flex min-w-0 items-baseline gap-1 truncate">
        <span className="text-ink">Live</span>
        {detail === '' ? null : <span className="text-ink-muted">{detail}</span>}
      </span>
    );
  }

  if (event.status === 'suspended') {
    return <span className="t-label truncate text-info">Suspended</span>;
  }

  const rendered = formatDayAndTime(event.scheduled_start, timeZone);
  if (rendered === '') return null;

  return (
    <time
      dateTime={toDateTimeAttribute(event.scheduled_start)}
      className="t-label block truncate text-ink-muted"
    >
      {rendered}
    </time>
  );
}
