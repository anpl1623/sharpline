/**
 * The shell every signal feed shares: one heading, one sub-nav, and the sentence
 * that governs how all three are read.
 *
 * A layout rather than three copies, because the framing paragraph is the same
 * claim on every feed and a claim stated three times is a claim that will
 * eventually be stated three different ways.
 */

import type { ReactNode } from 'react';

import { SignalNav } from '@/components/signals/signal-nav';

export default function SignalsLayout({
  children,
}: {
  readonly children: ReactNode;
}) {
  return (
    <div className="mx-auto flex w-full max-w-content flex-col gap-6 px-4 py-6">
      <header className="flex flex-col gap-2">
        <h1 className="t-h1 text-ink">Signals</h1>
        <p className="max-w-prose t-body text-ink-2">
          Findings written by the pricer as it computes, not numbers calculated
          when this page loaded. Each one records the thresholds it was detected
          under, so a row stays interpretable after the detector is retuned — and
          an empty feed means a detector did not fire, which is the ordinary
          state of a market that is working.
        </p>
      </header>

      <SignalNav />

      {children}
    </div>
  );
}
