'use client';

/**
 * The states the CLV panel has that are not a list of graded legs.
 *
 * # An empty CLV history is CORRECT
 *
 * A row exists only when a leg has been GRADED and the market it was on produced
 * a complete closing snapshot. A new account has neither, and an account whose
 * bets are all still running has the first without the second. There is no
 * warning colour here and no apology.
 *
 * # Absence is meaningful, and this panel says which absence it is
 *
 * `settle` writes no row at all — never a row of nulls — when the closing
 * snapshot was incomplete, when the outcome set changed, when the close would
 * precede the take, or when every candidate closing quote fell inside a
 * suspension. A leg with no row is not a leg with zero CLV, and the copy below
 * is careful to say so rather than implying the customer scored nothing.
 *
 * # Signed out is not empty
 *
 * Telling an unidentified reader "you have no closing line value" would be a
 * claim about an account nobody has named.
 */

import Link from 'next/link';

import { signInHref } from '@/components/auth/auth-card';
import { Button } from '@/components/ui';
import { developerDetail, userFacingMessage } from '@/lib/api/errors';
import { cn } from '@/lib/utils';

const PANEL =
  'flex max-w-prose flex-col items-start gap-3 rounded-card border border-rule bg-ground-1 p-6';

export function CLVEmpty() {
  return (
    <div className={PANEL}>
      <h2 className="t-h3 text-ink">No graded legs in this window</h2>
      <p className="t-body text-ink-2">
        Closing line value is measured when a leg is graded, against the price
        the market settled on just before kickoff. A ticket that is still running
        has no closing line yet.
      </p>
      <p className="t-body text-ink-muted">
        Some graded legs never get a row at all — a market that never produced a
        complete closing snapshot cannot be scored, and a missing row is not a
        score of zero.
      </p>
      <Button asChild size="sm" variant="outline">
        <Link href="/bets">Your bets</Link>
      </Button>
    </div>
  );
}

export function CLVSignedOut({ pathname }: { readonly pathname: string }) {
  return (
    <div className={PANEL}>
      <h2 className="t-h3 text-ink">Sign in to see your closing line value</h2>
      <p className="t-body text-ink-2">
        CLV belongs to an account. There is no user parameter on this endpoint —
        a session sees its own legs and nothing else.
      </p>
      <Button asChild variant="primary">
        <Link href={signInHref(pathname)}>Sign in</Link>
      </Button>
    </div>
  );
}

export interface CLVUnavailableProps {
  readonly error: unknown;
  readonly onRetry: () => void;
}

export function CLVUnavailable({ error, onRetry }: CLVUnavailableProps) {
  const detail = developerDetail(error);

  return (
    <div className={cn(PANEL, 'border-loss/40')}>
      <h2 className="t-h3 text-ink">Your closing line value could not be loaded</h2>
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
