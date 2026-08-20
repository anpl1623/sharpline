/**
 * `/signals/ev` — the positive expected value finder.
 *
 * A SERVER component that fetches the first page through the in-network service
 * name with `cache: 'no-store'`, for the same reason `/board` does: the first
 * paint is real findings, with no spinner and no client waterfall. `EVSignals`
 * takes that page and pages on from there.
 *
 * # The parameters are decided HERE and passed down
 *
 * A cursor is bound to the whole filter set on this endpoint, so the follow-up
 * page must send byte-identical values. Deciding them in the route and handing
 * them to the client component is what makes that true; a client-side default
 * would drift from the server's on the second page and earn a 400.
 */

import type { Metadata } from 'next';

import { EVSignals } from '@/components/signals/ev-signals';
import { SignalsUnavailable } from '@/components/signals/signals-empty';
import { serverApi } from '@/lib/api/server';
import type { EVSignalParams } from '@/lib/api/client';
import type { SchemaEvSignalPage } from '@/lib/api/schema';

export const dynamic = 'force-dynamic';

export const metadata: Metadata = {
  title: 'Positive EV',
  description:
    'Offered prices that beat the sharp reference book’s no-vig fair value, computed by Sharpline’s own pricing pipeline.',
};

/**
 * The API's own default page size, sent explicitly so the value this route used
 * is the value the client's follow-up pages use.
 */
const PAGE_LIMIT = 50;

/**
 * The reader's threshold, applied ON TOP of whatever the detector was configured
 * to emit. Zero means "everything that was written", which is the honest default
 * for a surface whose whole job is to show what the pipeline found — a non-zero
 * default here would hide findings without saying so.
 */
const MIN_EV_PERCENT = 0;

export default async function EVSignalsPage() {
  const params: EVSignalParams = {
    minEvPercent: MIN_EV_PERCENT,
    limit: PAGE_LIMIT,
  };

  let page: SchemaEvSignalPage;
  try {
    page = await serverApi.listEVSignals(params);
  } catch (error) {
    return (
      <SignalsUnavailable
        error={error}
        what="The +EV finder"
        retryHref="/signals/ev"
      />
    );
  }

  return (
    <section className="flex flex-col gap-4">
      <p className="max-w-prose t-body text-ink-muted">
        Every figure here is a RATE, not an amount: an expected value percentage
        is a return per unit staked, and this endpoint deliberately does not
        choose a stake. The fair price each one is measured against comes from
        one book — the sharp reference book — devigged with the method named on
        the row.
      </p>
      <EVSignals
        initialData={page}
        params={params}
        windowPhrase="the last 6 hours"
      />
    </section>
  );
}
