'use client';

/**
 * The route error boundary.
 *
 * Two audiences, one screen, which is the same split the whole product runs on:
 *
 *   - The sentence a person can act on comes from `userFacingMessage`, which
 *     never leaks a stack and never returns an empty string.
 *   - The engineering layer — error code, HTTP status, REQUEST ID, and Next's
 *     own server digest — sits below it in the mono register, inside a
 *     disclosure. It is never the primary message, and it is never hidden
 *     either: a request id the user can quote is the difference between a bug
 *     report and a shrug.
 *
 * `reset()` re-renders the segment. It is offered because a transport failure or
 * a 5xx is genuinely worth one more try; it is not offered as the only option,
 * because a 404-shaped failure will fail identically forever and the reader
 * needs a way out.
 */

import { useEffect } from 'react';
import Link from 'next/link';

import { Button } from '@/components/ui';
import {
  developerDetail,
  isApiError,
  userFacingMessage,
} from '@/lib/api/errors';

/* The shape Next passes an error boundary, spelled inline rather than exported:
 * a route file's named exports are validated against a fixed set. */
export default function AppError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    // Server-rendered failures arrive here already digested; logging keeps the
    // client-side ones out of a silent void.
    console.error(error);
  }, [error]);

  const detail = developerDetail(error);
  const requestId = isApiError(error) ? error.requestId : null;
  const digest = error.digest ?? null;

  return (
    <div className="mx-auto flex w-full max-w-content flex-col gap-6 px-6 py-24">
      <p className="t-mono text-ink-muted">ERROR</p>

      <h1 className="t-h1 text-ink">This page did not load</h1>

      <p className="max-w-prose t-body text-ink-2">
        {userFacingMessage(error)}
      </p>

      {detail === null && requestId === null && digest === null ? null : (
        <details className="max-w-prose rounded-card border border-rule bg-ground-1 p-4">
          <summary className="cursor-default t-label text-ink-muted">
            Detail
          </summary>
          <dl className="mt-3 grid grid-cols-[6rem_minmax(0,1fr)] gap-x-4 gap-y-2">
            {detail === null ? null : (
              <>
                <dt className="t-mono font-medium text-ink-muted">error</dt>
                <dd className="t-mono break-all text-ink-2">{detail}</dd>
              </>
            )}
            {requestId === null ? null : (
              <>
                <dt className="t-mono font-medium text-ink-muted">request</dt>
                <dd className="t-mono break-all text-ink-2">{requestId}</dd>
              </>
            )}
            {digest === null ? null : (
              <>
                <dt className="t-mono font-medium text-ink-muted">digest</dt>
                <dd className="t-mono break-all text-ink-2">{digest}</dd>
              </>
            )}
          </dl>
        </details>
      )}

      <div className="flex flex-wrap gap-3">
        <Button
          onClick={() => {
            reset();
          }}
        >
          Try again
        </Button>
        <Button asChild variant="outline">
          <Link href="/board">Go to the board</Link>
        </Button>
      </div>
    </div>
  );
}
