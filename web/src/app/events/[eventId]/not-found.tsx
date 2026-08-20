import Link from 'next/link';

/**
 * The all-leagues board.
 *
 * Spelled out rather than imported from `components/layout/league-nav`, which
 * owns the canonical helper: that module is `'use client'`, and every export of
 * a client module reaches a server component as a client REFERENCE rather than
 * as its value. A string constant imported across that boundary is not a
 * string.
 */
const BOARD_HREF = '/board';

/**
 * The API answered 404 for this event id.
 *
 * Two things can produce it and the copy names both, because they call for
 * different actions: a mistyped or stale identifier, and an event that has
 * aged out of the window the API serves. Neither is a system failure and
 * neither should read like one.
 */
export default function EventNotFound() {
  return (
    <div className="mx-auto flex w-full max-w-content flex-col gap-4 px-4 py-12">
      <h1 className="t-h1 text-ink">No such event</h1>
      <p className="t-body max-w-prose text-ink-2">
        The API has no event with that identifier. Either the link is stale, or
        the event has aged out of the window this system serves — a settled
        game leaves the board once its markets are settled.
      </p>
      <p className="t-body text-ink-2">
        <Link href={BOARD_HREF} className="ui-transition hover:text-ink">
          Back to the board
        </Link>
      </p>
    </div>
  );
}
