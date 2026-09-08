'use client';

/**
 * The states a wager list has that are not a list of wagers.
 *
 * # An empty history is CORRECT and must not look like a fault
 *
 * The same rule the board and the slip follow: no warning colour, no icon, no
 * apology. A new account has placed nothing, which is where every account
 * starts, and the panel's job is to name the filter it looked under and point at
 * the board.
 *
 * # NOTHING IS SEEDED, EVER
 *
 * No example ticket, no demo parlay, no "here's what a bet looks like". Every
 * number on this product comes from the pipeline, and a fabricated wager in an
 * empty history is worse than an empty panel because it is indistinguishable
 * from a real one in a screenshot.
 *
 * # Signed out is not empty
 *
 * A signed-out reader has no history rather than an empty one, and telling them
 * "you have not placed any bets" would be a claim about an account that has not
 * been identified. It gets its own state and a way in.
 */

import Link from 'next/link';

import { signInHref } from '@/components/auth/auth-card';
import { Button } from '@/components/ui';
import { developerDetail, userFacingMessage } from '@/lib/api/errors';
import { cn } from '@/lib/utils';

const PANEL =
  'flex max-w-prose flex-col items-start gap-3 rounded-card border border-rule bg-ground-1 p-6';

export interface BetsEmptyProps {
  /** Which view is empty: open positions, settled tickets, or everything. */
  readonly scope: 'open' | 'settled' | 'all';
}

export function BetsEmpty({ scope }: BetsEmptyProps) {
  if (scope === 'open') {
    return (
      <div className={PANEL}>
        <h2 className="t-h3 text-ink">No open positions</h2>
        <p className="t-body text-ink-2">
          Nothing is running. A ticket appears here from the moment it is booked
          until every leg on it has been graded.
        </p>
        <Button asChild size="sm" variant="outline">
          <Link href="/board">Go to the board</Link>
        </Button>
      </div>
    );
  }

  if (scope === 'settled') {
    return (
      <div className={PANEL}>
        <h2 className="t-h3 text-ink">Nothing has settled yet</h2>
        <p className="t-body text-ink-2">
          Won, lost, void, push and cashed-out tickets collect here. Legs grade
          independently and at different times, so a ticket moves across once its
          last leg is decided.
        </p>
      </div>
    );
  }

  return (
    <div className={PANEL}>
      <h2 className="t-h3 text-ink">You have not placed a bet yet</h2>
      <p className="t-body text-ink-2">
        Select a price on the board to put it on the slip. Everything here is
        play money — this is a simulation, not a licensed sportsbook, and no real
        money moves.
      </p>
      <Button asChild size="sm" variant="outline">
        <Link href="/board">Go to the board</Link>
      </Button>
    </div>
  );
}

export interface BetsSignedOutProps {
  /** The path to return to once a session exists. */
  readonly pathname: string;
}

export function BetsSignedOut({ pathname }: BetsSignedOutProps) {
  return (
    <div className={PANEL}>
      <h2 className="t-h3 text-ink">Sign in to see your bets</h2>
      <p className="t-body text-ink-2">
        Wagers belong to an account. There is no user parameter on this endpoint
        and there is not one on a single ticket either — a session sees its own
        tickets and nothing else.
      </p>
      <Button asChild variant="primary">
        <Link href={signInHref(pathname)}>Sign in</Link>
      </Button>
    </div>
  );
}

export interface BetsUnavailableProps {
  readonly error: unknown;
  readonly onRetry: () => void;
}

/**
 * The list could not be read.
 *
 * `request_id` goes in a collapsed developer detail and never in the sentence a
 * reader meets first — it is the handle for the log line, not an explanation.
 */
export function BetsUnavailable({ error, onRetry }: BetsUnavailableProps) {
  const detail = developerDetail(error);

  return (
    <div className={cn(PANEL, 'border-loss/40')}>
      <h2 className="t-h3 text-ink">Your bets could not be loaded</h2>
      <p className="t-body text-ink-2">{userFacingMessage(error)}</p>
      <Button type="button" size="sm" variant="outline" onClick={onRetry}>
        Try again
      </Button>
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
