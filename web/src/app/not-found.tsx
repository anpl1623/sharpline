/**
 * 404. Terse, in register, no illustration.
 *
 * The mono line at the top is the engineering layer doing what it does
 * everywhere else in this product: stating the machine's actual answer. A 404
 * page that hides the status code in favour of a cartoon is withholding the one
 * fact the reader can act on.
 */

import Link from 'next/link';

import { Button } from '@/components/ui';

export default function NotFound() {
  return (
    <div className="mx-auto flex w-full max-w-content flex-col gap-6 px-6 py-24">
      <p className="t-mono text-ink-muted">HTTP 404</p>

      <h1 className="t-h1 text-ink">Nothing at this address</h1>

      <p className="max-w-prose t-body text-ink-2">
        This URL does not match a page, a league board, or an event in this
        application. A league board lives at{' '}
        <code className="t-mono text-ink-2">/board/&lt;league-slug&gt;</code> and
        an event at <code className="t-mono text-ink-2">/events/&lt;id&gt;</code>
        ; both slugs come from the catalogue rather than from a guess.
      </p>

      <div className="flex flex-wrap gap-3">
        <Button asChild>
          <Link href="/board">Go to the board</Link>
        </Button>
        <Button asChild variant="outline">
          <Link href="/">Home</Link>
        </Button>
      </div>
    </div>
  );
}
