/**
 * `/account/clv` — the signed-in customer's closing line value.
 *
 * A server component that renders a client one and fetches nothing itself, for
 * the reason `/bets` gives: this endpoint is scoped to the token's own user, the
 * access token lives in memory in the browser and is never persisted, and a
 * server render has no credential to present. Giving the server one would mean a
 * cookie-bearing API and a CSRF surface to defend, which is a much larger cost
 * than a loading state on one page.
 */

import type { Metadata } from 'next';

import { CLVPanel } from '@/components/analytics/clv-panel';

export const metadata: Metadata = {
  title: 'Your closing line value',
  description:
    'How the prices you took compare with the prices the market closed at, measured on devigged prices.',
};

export default function AccountCLVPage() {
  return (
    <div className="mx-auto flex w-full max-w-content flex-col gap-6 px-4 py-6">
      <header className="flex flex-col gap-2">
        <h1 className="t-h1 text-ink">Closing line value</h1>
        <p className="max-w-prose t-body text-ink-2">
          Whether the price you took was better than the price the market settled
          on just before kickoff. Both sides are devigged first with the same
          method, because a quoted price contains the market&rsquo;s estimate AND
          the book&rsquo;s margin, and CLV is a claim about the first only —
          comparing raw prices reports value lost on a line that never moved
          whenever a book merely widened its juice.
        </p>
      </header>

      <CLVPanel />
    </div>
  );
}
