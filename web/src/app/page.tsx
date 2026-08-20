/**
 * The landing poster — the one place in this product the design is allowed to be
 * a poster rather than a grid. DESIGN.md § Layout: "hybrid — grid-disciplined
 * for the app, poster for the landing page."
 *
 * # What is deliberately absent
 *
 * No feature grid. No emoji cards. No "get started in seconds". No testimonial,
 * no logo wall, no counter animating up to a number nobody measured. This is an
 * instrument, not a casino, and the landing page is the first place that claim is
 * either made or broken.
 *
 * # What is present, and why every line of it is checkable
 *
 * The four-line manifest below states what the system actually does today, in the
 * mono register — the engineering layer, used here on purpose, because a claim
 * about a pipeline set in the same face as the pipeline's own status rail is a
 * claim the reader can go and verify. Every line corresponds to code in this
 * repository and to a phase that is built:
 *
 *   ingest   phase 3 — provider adapters, normalization, change detection
 *   bus      phase 3 — Kafka in KRaft mode, `odds.normalized` log-compacted
 *   pricing  phase 4 — devig, no-vig fair value, EV, Kelly
 *   stream   phase 6 — snapshot-then-delta over one WebSocket, sequenced
 *
 * Kubernetes, Flink and the 10k-connection load test are NOT mentioned. They are
 * phases 10, 12 and 11, and a landing page that claims them today would be the
 * exact thing this project's README refuses to do.
 *
 * # The disclaimer is not negotiable
 *
 * CLAUDE.md §0. It lives in `disclaimer.tsx` so there is one copy of the wording,
 * and it appears here below the fold of nothing — it is on the page, in the flow,
 * at readable size.
 */

import Link from 'next/link';
import { ArrowRight } from 'lucide-react';

import { Disclaimer } from '@/components/layout/disclaimer';
import { Button } from '@/components/ui';

interface ManifestLine {
  readonly key: string;
  readonly statement: string;
}

const MANIFEST: readonly ManifestLine[] = [
  {
    key: 'ingest',
    statement:
      'Provider adapters normalize every payload and hash it, so an unchanged poll produces no traffic.',
  },
  {
    key: 'bus',
    statement:
      'Apache Kafka in KRaft mode. The normalized odds topic is log-compacted, so the current line is replayable from the log itself.',
  },
  {
    key: 'pricing',
    statement:
      'Devig, no-vig fair value, expected value and Kelly sizing, recomputed per market as prices arrive.',
  },
  {
    key: 'stream',
    statement:
      'One WebSocket. A snapshot, then deltas, with a sequence number on every frame so a dropped buffer is visible as a gap.',
  },
];

export default function Home() {
  return (
    <div className="mx-auto flex w-full max-w-content flex-col gap-16 px-6 py-16 md:py-24">
      <section className="flex flex-col gap-6">
        <p className="t-label text-ink-muted">Self-hosted · simulation</p>

        <h1 className="t-display text-ink">
          Live odds.
          <br />
          Computed here.
        </h1>

        <p className="max-w-prose t-body text-ink-2">
          Sharpline is a real-time sports odds platform. Odds are ingested,
          normalized and priced, then streamed to this browser and updated in
          place as they move — with every stage running in a container on
          hardware the author controls.
        </p>

        <div className="flex flex-wrap items-center gap-3">
          <Button asChild>
            <Link href="/board">
              Open the board
              <ArrowRight className="size-4" aria-hidden="true" />
            </Link>
          </Button>
        </div>
      </section>

      <section aria-labelledby="pipeline-heading" className="flex flex-col gap-4">
        <h2 id="pipeline-heading" className="t-label text-ink-muted">
          The path a price takes
        </h2>
        <dl className="flex flex-col border-t border-rule">
          {MANIFEST.map((line) => (
            <div
              key={line.key}
              className="flex flex-col gap-1 border-b border-rule py-3 md:flex-row md:gap-6"
            >
              <dt className="t-mono font-medium text-ink-muted md:w-24 md:shrink-0">
                {line.key}
              </dt>
              <dd className="max-w-prose t-mono text-ink-2">
                {line.statement}
              </dd>
            </div>
          ))}
        </dl>
      </section>

      <Disclaimer />
    </div>
  );
}
