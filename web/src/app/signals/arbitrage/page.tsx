/**
 * `/signals/arbitrage` — live arbitrage, with the staleness discipline visible.
 *
 * The route sends NO bounds of its own and takes the API's defaults, which are
 * the DETECTOR's own bounds — 120 seconds of leg age and 30 seconds of observed
 * spread, from `pricing.DefaultArbitrageConfig()`. Restating them here would put
 * a second copy of a number that already has one authoritative home, and a copy
 * that drifted low would silently hide findings.
 *
 * Whatever was applied comes back on `bounds` and is rendered above the list, so
 * a reader looking at a short feed can see what was filtered out.
 */

import type { Metadata } from 'next';

import { ArbitrageSignals } from '@/components/signals/arbitrage-signals';
import { SignalsUnavailable } from '@/components/signals/signals-empty';
import { serverApi } from '@/lib/api/server';
import type { ArbitrageSignalParams } from '@/lib/api/client';
import type { SchemaArbitrageSignalList } from '@/lib/api/schema';

export const dynamic = 'force-dynamic';

export const metadata: Metadata = {
  title: 'Arbitrage',
  description:
    'Markets whose best available prices sum to under one implied probability, with every leg’s age exposed.',
};

const PAGE_LIMIT = 50;

export default async function ArbitrageSignalsPage() {
  const params: ArbitrageSignalParams = { limit: PAGE_LIMIT };

  let list: SchemaArbitrageSignalList;
  try {
    list = await serverApi.listArbitrageSignals(params);
  } catch (error) {
    return (
      <SignalsUnavailable
        error={error}
        what="Live arbitrage"
        retryHref="/signals/arbitrage"
      />
    );
  }

  return (
    <section className="flex flex-col gap-4">
      <p className="max-w-prose t-body text-ink-muted">
        An arbitrage is a market whose best prices across the books quoting it
        sum to under one implied probability. Almost all apparent ones are not:
        they are one book that has not moved yet, and two prices observed ninety
        seconds apart were never simultaneously available. The ages below are how
        you tell the difference.
      </p>
      <ArbitrageSignals
        initialData={list}
        params={params}
        windowPhrase="the last 15 minutes"
      />
    </section>
  );
}
