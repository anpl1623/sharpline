/**
 * The two states the table itself cannot render: nothing to show, and nothing
 * fetched.
 *
 * # An empty board is CORRECT, and must not look like a fault
 *
 * `starting_before` is an upper bound on kickoff. Ask for the next three hours
 * at four in the morning and the honest answer is that no tradeable event starts
 * in that window. There is nothing broken about that, so there is no warning
 * colour, no icon, and no apology here — the state names the window it looked
 * in and offers the one control that changes the answer.
 *
 * The state this is NOT is "the stream is down but the REST prices are still on
 * screen". That is a connection fact, it belongs to the status rail, and it
 * never blanks the table: the prices stay, they stop moving, and the rail says
 * so. Conflating the two would teach a viewer that an empty board means a broken
 * system.
 *
 * This module carries no `'use client'` directive and no hook, so the routes can
 * render `BoardUnavailable` directly from the server when the fetch itself
 * failed.
 */

import Link from 'next/link';

import { developerDetail, userFacingMessage } from '@/lib/api/errors';
import { cn } from '@/lib/utils';

const PANEL = 'mx-4 my-8 flex max-w-prose flex-col items-start gap-3 border border-rule rounded-card bg-ground-1 p-6';

const ACTION = [
  'inline-flex h-9 items-center justify-center whitespace-nowrap rounded-price',
  'border border-rule bg-transparent px-3 t-ui text-ink-2 ui-transition',
  'hover:border-rule-hi hover:text-ink',
].join(' ');

export interface BoardEmptyProps {
  /** "the next 24 hours" — the window that was actually asked for. */
  readonly windowPhrase: string;
  readonly liveOnly: boolean;
  /** Events the API returned before the client-side live-only filter. */
  readonly loadedCount: number;
  /** The same board with the live-only filter cleared. */
  readonly showAllHref: string;
  /** The next wider window, when there is one. */
  readonly widerHref: string | null;
  readonly widerLabel: string | null;
  /** How many books the viewer has narrowed to. Zero means every book. */
  readonly bookFilterCount: number;
  /** Set on the single-league route, so the copy can name what was searched. */
  readonly leagueName: string | null;
}

export function BoardEmpty({
  windowPhrase,
  liveOnly,
  loadedCount,
  showAllHref,
  widerHref,
  widerLabel,
  bookFilterCount,
  leagueName,
}: BoardEmptyProps) {
  const scope = leagueName === null ? '' : ` in ${leagueName}`;

  // The API returned events; the live-only filter is what emptied the board.
  // Saying "no events" here would be false, and the fix is a different one.
  if (liveOnly && loadedCount > 0) {
    return (
      <div className={PANEL}>
        <h2 className="t-h3 text-ink">Nothing is live right now</h2>
        <p className="t-body text-ink-2">
          {loadedCount === 1
            ? `1 event${scope} starts within ${windowPhrase}, and it has not kicked off yet.`
            : `${String(loadedCount)} events${scope} start within ${windowPhrase}, and none of them are under way yet.`}
        </p>
        <Link href={showAllHref} className={ACTION}>
          Show all events
        </Link>
      </div>
    );
  }

  return (
    <div className={PANEL}>
      <h2 className="t-h3 text-ink">No events in this window</h2>
      <p className="t-body text-ink-2">
        {liveOnly
          ? `No event${scope} is live and starting within ${windowPhrase}.`
          : `No tradeable event${scope} starts within ${windowPhrase}.`}{' '}
        The board only lists events that have not finished, so a quiet window is
        a quiet window rather than a fault.
      </p>
      {bookFilterCount > 0 ? (
        <p className="t-ui text-ink-muted">
          Prices are currently narrowed to{' '}
          {bookFilterCount === 1 ? '1 book' : `${String(bookFilterCount)} books`}.
          Clearing that filter will not add events, but it will widen the prices
          on the ones that are here.
        </p>
      ) : null}
      <div className="flex flex-wrap gap-2">
        {widerHref === null || widerLabel === null ? null : (
          <Link href={widerHref} className={ACTION}>
            Look {widerLabel} ahead
          </Link>
        )}
        {liveOnly ? (
          <Link href={showAllHref} className={ACTION}>
            Show all events
          </Link>
        ) : null}
      </div>
    </div>
  );
}

export interface BoardUnavailableProps {
  /** The thrown value. Rendered through the API error vocabulary, never raw. */
  readonly error: unknown;
  /** Where to try again. A link, so it works with JavaScript disabled. */
  readonly retryHref: string;
}

/**
 * The board could not be fetched.
 *
 * `request_id` goes in a collapsed developer detail and never in the sentence a
 * viewer reads first — it is the handle for the log line, not an explanation.
 */
export function BoardUnavailable({ error, retryHref }: BoardUnavailableProps) {
  const detail = developerDetail(error);

  return (
    <div className={cn(PANEL, 'border-loss/40')}>
      <h2 className="t-h3 text-ink">The board could not be loaded</h2>
      <p className="t-body text-ink-2">{userFacingMessage(error)}</p>
      <Link href={retryHref} className={ACTION}>
        Try again
      </Link>
      {detail === null ? null : (
        <details className="w-full">
          <summary className="t-ui cursor-pointer text-ink-muted">
            Technical detail
          </summary>
          <p className="t-mono mt-2 break-all text-ink-muted">{detail}</p>
        </details>
      )}
    </div>
  );
}
