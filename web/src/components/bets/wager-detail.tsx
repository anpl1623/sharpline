'use client';

/**
 * One ticket, in full, with its cash-out.
 *
 * # A wager that is not yours is a 404, and this page renders it as one
 *
 * The ownership comparison happens in the adapter, on the row as it is read, and
 * a mismatch becomes the same not-found the missing row produces — so there is
 * no branch anywhere above it that could tell the two apart. That is the one
 * place this API answers 404 for something that exists, and the concealment is
 * the point: a 403 on another customer's wager id would confirm the id exists,
 * which is a wager-enumeration oracle over every customer of the book.
 *
 * This component therefore does NOT distinguish "no such ticket" from "not
 * yours", and must not try to. Both render the same sentence.
 *
 * # The cash-out lives here and not in the list
 *
 * Quoting is a request, and a page of open tickets each holding a live quote
 * would ask the book for a dozen prices nobody requested. The list links here;
 * here the customer has asked for this ticket, so asking for its price is the
 * next thing they might want.
 */

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { useQuery } from '@tanstack/react-query';

import { Button } from '@/components/ui';
import { isApiError, userFacingMessage } from '@/lib/api/errors';
import { wagerQueryOptions } from '@/lib/api/queries';
import { useAccessToken, useAuth } from '@/lib/store/auth';
import { BetsSignedOut } from './bets-empty';
import { WagerCard } from './wager-card';

export interface WagerDetailProps {
  readonly wagerId: string;
}

export function WagerDetail({ wagerId }: WagerDetailProps) {
  const pathname = usePathname();
  const accessToken = useAccessToken();

  const hydrated = useAuth((state) => state.hydrated);
  const status = useAuth((state) => state.status);
  const hasStoredSession = useAuth(
    (state) => state.refreshToken !== null && state.refreshToken !== '',
  );
  const signedIn = accessToken !== null && accessToken !== '';
  const resolving =
    !hydrated ||
    status === 'authenticating' ||
    status === 'refreshing' ||
    (!signedIn && hasStoredSession);

  const wager = useQuery(wagerQueryOptions(accessToken, wagerId));

  if (resolving) {
    return (
      <p className="t-ui text-ink-muted" role="status">
        Checking your session…
      </p>
    );
  }

  if (!signedIn) return <BetsSignedOut pathname={pathname} />;

  if (wager.isPending) {
    return (
      <p className="t-ui text-ink-muted" role="status">
        Loading this ticket…
      </p>
    );
  }

  if (wager.isError) {
    const missing = isApiError(wager.error) && wager.error.status === 404;
    return (
      <div className="flex max-w-prose flex-col items-start gap-3 rounded-card border border-rule bg-ground-1 p-6">
        <h2 className="t-h3 text-ink">
          {missing ? 'No such ticket' : 'This ticket could not be loaded'}
        </h2>
        <p className="t-body text-ink-2">
          {missing
            ? 'This account has no ticket with that identifier.'
            : userFacingMessage(wager.error)}
        </p>
        <Button asChild size="sm" variant="outline">
          <Link href="/bets">Back to your bets</Link>
        </Button>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <WagerCard wager={wager.data} detail />
      <div>
        <Button asChild size="sm" variant="ghost">
          <Link href="/bets">Back to your bets</Link>
        </Button>
      </div>
    </div>
  );
}
