/**
 * The states a signal feed has that are not a list of findings.
 *
 * # AN EMPTY FEED IS CORRECT AND MUST NOT LOOK LIKE A FAULT
 *
 * This is the most important panel in the phase 9 frontend and it is worth being
 * explicit about why. A +EV finder with nothing in it, an arbitrage feed with
 * nothing in it, and a leaderboard with nobody on it are all NORMAL. `pricer`
 * writes a finding when a detector fires; a detector that has not fired has
 * nothing to say. On a fresh deployment every one of these lists is empty for
 * hours, and a feed that constantly reported arbitrage would be a feed with a
 * bug in it.
 *
 * So: no warning colour, no icon, no apology. Each state names the window and
 * the thresholds it looked under — because those are the only things a reader
 * can change — and says plainly that nothing was found.
 *
 * # NOTHING IS SEEDED, EVER
 *
 * There is no example arbitrage, no illustrative +EV row, no "here is what a
 * steam move looks like". Every number on this product came through the
 * pipeline, and a fabricated finding in an empty feed is worse than an empty
 * panel because it is indistinguishable from a real one in a screenshot — which
 * is precisely the screenshot somebody would take.
 *
 * This module carries no `'use client'` directive and no hook, so a route can
 * render `SignalsUnavailable` directly from the server when the fetch failed.
 */

import Link from 'next/link';

import { developerDetail, userFacingMessage } from '@/lib/api/errors';
import { cn } from '@/lib/utils';

const PANEL =
  'flex max-w-prose flex-col items-start gap-3 rounded-card border border-rule bg-ground-1 p-6';

const ACTION = [
  'inline-flex h-9 items-center justify-center whitespace-nowrap rounded-price',
  'border border-rule bg-transparent px-3 t-ui text-ink-2 ui-transition',
  'hover:border-rule-hi hover:text-ink',
].join(' ');

export interface SignalsEmptyProps {
  /** Which feed is empty. */
  readonly feed: 'ev' | 'arbitrage' | 'steam';
  /** "the last 6 hours" — the window that was actually asked for. */
  readonly windowPhrase: string;
  /** The thresholds this read applied, already rendered. One line each. */
  readonly thresholds: readonly string[];
}

const COPY: Record<
  SignalsEmptyProps['feed'],
  { readonly heading: string; readonly body: string }
> = {
  ev: {
    heading: 'No positive expected value found',
    body:
      'Nothing offered by any book beat the sharp reference book’s no-vig price ' +
      'in this window. That is the ordinary state of a market that is working: ' +
      'a book whose prices were routinely beatable would not stay in business.',
  },
  arbitrage: {
    heading: 'No live arbitrage',
    body:
      'No market’s best prices summed to under one implied probability inside ' +
      'the staleness bounds below. This is the expected answer almost all of ' +
      'the time — most apparent cross-book arbitrage is one book that has not ' +
      'moved yet, and the bounds exist to refuse exactly that.',
  },
  steam: {
    heading: 'No steam moves',
    body:
      'No book-led line move cleared the detector’s thresholds in this window. ' +
      'The threshold that decides is the size of the move: ordinary drift is ' +
      'small, and books move together whether it is steam or drift.',
  },
};

export function SignalsEmpty({ feed, windowPhrase, thresholds }: SignalsEmptyProps) {
  const copy = COPY[feed];

  return (
    <div className={PANEL}>
      <h2 className="t-h3 text-ink">{copy.heading}</h2>
      <p className="t-body text-ink-2">{copy.body}</p>
      <p className="t-ui text-ink-muted">Looked at {windowPhrase}.</p>
      {thresholds.length === 0 ? null : (
        <ul className="flex flex-col gap-0.5">
          {thresholds.map((line) => (
            <li key={line} className="t-mono text-ink-muted">
              {line}
            </li>
          ))}
        </ul>
      )}
      <Link href="/board" className={ACTION}>
        Go to the board
      </Link>
    </div>
  );
}

export interface SignalsUnavailableProps {
  /** The thrown value. Rendered through the API error vocabulary, never raw. */
  readonly error: unknown;
  /** What could not be loaded, for the heading. */
  readonly what: string;
  /** A link, so it works with JavaScript disabled. */
  readonly retryHref?: string | undefined;
  /** Or a callback, when the caller is already a client component. */
  readonly onRetry?: (() => void) | undefined;
}

/**
 * The feed could not be fetched.
 *
 * `request_id` goes in a collapsed developer detail and never in the sentence a
 * reader meets first — it is the handle for the log line, not an explanation.
 * The distinction between this and `SignalsEmpty` is the whole point of having
 * two components: "there is nothing to report" and "we could not find out" are
 * different facts and a reader must never have to guess which one they are
 * looking at.
 */
export function SignalsUnavailable({
  error,
  what,
  retryHref,
  onRetry,
}: SignalsUnavailableProps) {
  const detail = developerDetail(error);

  return (
    <div className={cn(PANEL, 'border-loss/40')}>
      <h2 className="t-h3 text-ink">{what} could not be loaded</h2>
      <p className="t-body text-ink-2">{userFacingMessage(error)}</p>
      {retryHref === undefined ? null : (
        <Link href={retryHref} className={ACTION}>
          Try again
        </Link>
      )}
      {onRetry === undefined ? null : (
        <button type="button" className={ACTION} onClick={onRetry}>
          Try again
        </button>
      )}
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
