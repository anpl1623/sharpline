'use client';

/**
 * The steam feed: correlated, book-led line movement, most recent window first.
 *
 * # Amber is steam, and that is a decision DESIGN.md already took
 *
 * § Color: "amber = heat maps onto steam, a headline feature". `delta-in` is a
 * RISE in implied probability — a shortening price — and `delta-out` is a fall.
 * A steam move that shortens a price is therefore `delta-in` (amber) and one
 * that drifts is `delta-out` (cyan), which is the same mapping the board's delta
 * rail uses on the same fact. Two surfaces disagreeing about what amber means
 * would be worse than either choice.
 *
 * Both are TINTED badges (8% fill, 40% border), never a fill: the saturated
 * treatment is spent on arbitrage and on nothing else.
 *
 * COLOUR IS NEVER THE ONLY CARRIER. Each badge spells the direction as a word,
 * and the numeral carries its own sign, so a colourblind reader and a screen
 * reader both get the fact without the hue.
 *
 * # The units are probability points, and the labels say so
 *
 * Steam is detected on IMPLIED PROBABILITY velocity, never on decimal odds:
 * decimal is non-linear in probability, so a fixed decimal threshold means a
 * different thing at 1.50 than at 10.00. Every figure below carries `pts` or
 * `pts/min` in the string rather than a bare numeral, because "2.10" beside a
 * price reads as a price and "2.10%" reads as a percentage change in one.
 *
 * # Ranked by recency, and this component must not "improve" that
 *
 * A steam alert is actionable while the follower books are still catching up —
 * that lag IS the opportunity — so an hour-old larger move is worth less than a
 * fresh smaller one. Magnitude is a filter and never the sort. Sorting this list
 * by size would look like a better product and would be a worse one.
 */

import { useInfiniteQuery } from '@tanstack/react-query';

import { Badge, Button } from '@/components/ui';
import { SignalsEmpty, SignalsUnavailable } from './signals-empty';
import { useLocalTimeZone } from '@/components/event/use-event-live';
import type { SteamSignalParams } from '@/lib/api/client';
import { steamSignalsInfiniteQueryOptions } from '@/lib/api/queries';
import type { SchemaSteamSignal, SchemaSteamSignalPage } from '@/lib/api/schema';
import {
  formatAgeSeconds,
  formatPointsMagnitude,
  formatProbabilityPoints,
  formatVelocity,
} from '@/lib/analytics/format';
import { marketTypeShortLabel } from '@/lib/odds/line';
import { formatAbsolute } from '@/lib/time';

export interface SteamSignalsProps {
  readonly initialData: SchemaSteamSignalPage;
  readonly params: SteamSignalParams;
  readonly windowPhrase: string;
}

export function SteamSignals({
  initialData,
  params,
  windowPhrase,
}: SteamSignalsProps) {
  const timeZone = useLocalTimeZone();

  const query = useInfiniteQuery({
    ...steamSignalsInfiniteQueryOptions(params),
    initialData: { pages: [initialData], pageParams: [undefined] },
  });

  const rows = query.data.pages.flatMap((page) => page.data);

  if (rows.length === 0) {
    return (
      <SignalsEmpty
        feed="steam"
        windowPhrase={windowPhrase}
        thresholds={[
          `min magnitude ${formatPointsMagnitude(params.minMagnitude ?? 0)}`,
          `at least ${String(params.minParticipatingBooks ?? 2)} participating books`,
        ]}
      />
    );
  }

  return (
    <div className="flex flex-col gap-4">
      {query.isError ? (
        <SignalsUnavailable
          error={query.error}
          what="The next page"
          onRetry={() => {
            void query.refetch();
          }}
        />
      ) : null}

      <ul className="flex flex-col gap-3">
        {rows.map((signal) => (
          <li
            key={`${signal.market_id}:${signal.selection_id}:${signal.window_end}`}
          >
            <SteamCard signal={signal} timeZone={timeZone} />
          </li>
        ))}
      </ul>

      {query.hasNextPage ? (
        <div>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={query.isFetchingNextPage}
            onClick={() => {
              void query.fetchNextPage();
            }}
          >
            {query.isFetchingNextPage ? 'Loading…' : 'Load more'}
          </Button>
        </div>
      ) : null}
    </div>
  );
}

function SteamCard({
  signal,
  timeZone,
}: {
  readonly signal: SchemaSteamSignal;
  readonly timeZone: string;
}) {
  const shortening = signal.direction === 'shorten';

  return (
    <article className="flex flex-col gap-3 rounded-card border border-rule bg-ground-1 p-4">
      <header className="flex flex-wrap items-center gap-2">
        {/* Tinted, in the direction's own hue, with the direction ALSO spelled
            out — colour is redundant here, never load-bearing alone. */}
        <Badge variant={shortening ? 'delta-in' : 'delta-out'}>
          {shortening ? '↑ shortening' : '↓ drifting'}
        </Badge>
        <span className="t-ui text-ink">
          {marketTypeShortLabel(signal.market_type)}
        </span>
        <span className="t-mono break-all text-ink-muted">
          {signal.selection_id}
        </span>
        <Badge variant="neutral">
          {`${String(signal.participating_books)} books`}
        </Badge>
      </header>

      <dl className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <Fact term="Move">
          {formatProbabilityPoints(signal.delta_probability)}
        </Fact>
        <Fact term="Velocity">
          {formatVelocity(signal.velocity_probability_per_minute)}
        </Fact>
        {/* NOT the field that separates steam from drift, however much it
            looks like it should be: every book prices the same underlying
            market, so drift is correlated too and this number sits near 1 on
            quiet windows. It rules out a move ONE book made alone — a tick
            rounding, a book's own bias. The discriminator is the magnitude
            against threshold_magnitude. See internal/analytics/steam/doc.go. */}
        <Fact term="Cross-book correlation">
          {signal.cross_book_correlation.toFixed(3)}
        </Fact>
        <Fact term="Window">
          {`${formatAgeSeconds(signal.window_seconds, 0)} hopping every ${formatAgeSeconds(signal.hop_seconds, 0)}`}
        </Fact>
      </dl>

      <div className="flex flex-col gap-1">
        <h3 className="t-label text-ink-muted">Who moved, and when</h3>
        <p className="t-mono text-ink-2">
          {`${signal.lead_book_id} led at ${formatAbsolute(signal.lead_moved_at, timeZone)}`}
        </p>
        <ul className="flex flex-col gap-0.5">
          {signal.followers.map((follower) => (
            <li key={follower.book_id} className="t-mono text-ink-muted">
              {`${follower.book_id} followed +${formatAgeSeconds(follower.lag_seconds)} · ${formatProbabilityPoints(follower.delta_probability)}`}
            </li>
          ))}
        </ul>
      </div>

      <p className="t-mono text-ink-muted">
        {`window [${formatAbsolute(signal.window_start, timeZone)}, ${formatAbsolute(signal.window_end, timeZone)}) · devig ${signal.devig_method} · thresholds ${formatPointsMagnitude(signal.threshold_magnitude)} / ${formatVelocity(signal.threshold_velocity)} / r ${signal.threshold_correlation.toFixed(2)}`}
      </p>
    </article>
  );
}

function Fact({
  term,
  children,
}: {
  readonly term: string;
  readonly children: React.ReactNode;
}) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="t-label text-ink-muted">{term}</dt>
      <dd className="t-mono text-ink">{children}</dd>
    </div>
  );
}
