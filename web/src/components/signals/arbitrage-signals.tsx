'use client';

/**
 * Live arbitrage.
 *
 * # This is the one saturated fill in the entire interface
 *
 * DESIGN.md § Signals: everything that is a READING — +EV, steam, a suspension,
 * a book kind — gets a tinted badge at 8% fill and a 40% border. Arbitrage gets
 * a full `money` ground with `on-money` ink, and it is the ONLY object in the
 * product that does. The reason is scarcity: an arbitrage is rare enough to earn
 * a shout, and making it the single loudest thing on screen means it is never
 * missed AND never imitated by anything less rare. Nothing else on this page
 * approaches it — every other figure here is `ink`, `ink-2` or `ink-muted`.
 *
 * The saturated fill is spent on the BADGE and on nothing else: not the row, not
 * the return figure, not the card border. A card-sized green fill would make the
 * badge redundant and would break the one-loud-object property the moment two
 * findings were on screen at once.
 *
 * # The staleness figures are the content, not the footnote
 *
 * The phase 4 gate measured 68 apparent arbitrages across 1,065 records with the
 * leg-age bound binding almost constantly: most cross-book "arbitrage" is one
 * book that has not moved yet. So `observed_spread_seconds`, the oldest leg's
 * age, and EVERY leg's own age are rendered on every finding, and the bounds the
 * read applied are rendered above the list. A feed that hid them would be a feed
 * that trains a reader to ignore the finding that is real.
 *
 * # Not paginated
 *
 * The endpoint has no cursor, deliberately: the bounds make the live set small
 * and it turns over in seconds, so paging would walk a list that no longer
 * exists. The control here is a refresh, and it is explicit rather than a poll —
 * a list that reordered itself while somebody was reading a row would be worse
 * than a stale one.
 */

import { useQuery } from '@tanstack/react-query';

import { Badge, Button } from '@/components/ui';
import { SignalsEmpty, SignalsUnavailable } from './signals-empty';
import { useLocalTimeZone } from '@/components/event/use-event-live';
import type { ArbitrageSignalParams } from '@/lib/api/client';
import { arbitrageSignalsQueryOptions } from '@/lib/api/queries';
import type {
  SchemaArbitrageSignal,
  SchemaArbitrageSignalList,
} from '@/lib/api/schema';
import {
  formatAgeSeconds,
  formatFractionAsPercent,
  formatMarketLine,
  formatPercentPoints,
} from '@/lib/analytics/format';
import { formatOdds } from '@/lib/odds/format';
import { marketTypeShortLabel, selectionRoleLabel } from '@/lib/odds/line';
import { useOddsFormat } from '@/lib/store/preferences';
import { formatAbsolute } from '@/lib/time';

export interface ArbitrageSignalsProps {
  readonly initialData: SchemaArbitrageSignalList;
  readonly params: ArbitrageSignalParams;
  readonly windowPhrase: string;
}

export function ArbitrageSignals({
  initialData,
  params,
  windowPhrase,
}: ArbitrageSignalsProps) {
  const format = useOddsFormat();
  const timeZone = useLocalTimeZone();

  const query = useQuery({
    ...arbitrageSignalsQueryOptions(params),
    initialData,
  });

  const bounds = query.data.bounds;
  const thresholds = [
    `oldest leg at most ${formatAgeSeconds(bounds.max_leg_age_seconds, 0)}`,
    `legs observed within ${formatAgeSeconds(bounds.max_spread_seconds, 0)} of each other`,
    `return at least ${formatPercentPoints(bounds.min_return_percent)}`,
    `at least ${String(bounds.min_distinct_books)} book${bounds.min_distinct_books === 1 ? '' : 's'}`,
  ];

  return (
    <div className="flex flex-col gap-4">
      {/* The bounds are ABOVE the list rather than under it. A reader looking at
          an empty or short feed needs to know what was filtered out before they
          conclude anything from its length. */}
      <div className="flex flex-col gap-2 rounded-card border border-rule bg-ground-1 p-4">
        <h2 className="t-label text-ink-muted">Bounds applied to this read</h2>
        <ul className="flex flex-wrap gap-x-4 gap-y-1">
          {thresholds.map((line) => (
            <li key={line} className="t-mono text-ink-2">
              {line}
            </li>
          ))}
        </ul>
        <p className="t-body text-ink-muted">
          Most apparent cross-book arbitrage is one book that has not moved yet.
          These bounds refuse it, and every finding below shows the ages that got
          it past them.
        </p>
      </div>

      {query.isError ? (
        <SignalsUnavailable
          error={query.error}
          what="Live arbitrage"
          onRetry={() => {
            void query.refetch();
          }}
        />
      ) : null}

      {query.data.data.length === 0 ? (
        <SignalsEmpty
          feed="arbitrage"
          windowPhrase={windowPhrase}
          thresholds={thresholds}
        />
      ) : (
        <ul className="flex flex-col gap-3">
          {query.data.data.map((signal) => (
            <li key={signal.id}>
              <ArbitrageCard
                signal={signal}
                format={format}
                timeZone={timeZone}
              />
            </li>
          ))}
        </ul>
      )}

      <div>
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={query.isFetching}
          onClick={() => {
            void query.refetch();
          }}
        >
          {query.isFetching ? 'Refreshing…' : 'Refresh'}
        </Button>
      </div>
    </div>
  );
}

function ArbitrageCard({
  signal,
  format,
  timeZone,
}: {
  readonly signal: SchemaArbitrageSignal;
  readonly format: ReturnType<typeof useOddsFormat>;
  readonly timeZone: string;
}) {
  // The MARKET's own line, in the home frame — unlike every other line on
  // this surface, which is in its selection's frame. Each leg below carries its
  // own line in its own frame.
  const line = formatMarketLine(signal.market_type, signal.line);

  return (
    <article className="flex flex-col gap-3 rounded-card border border-rule bg-ground-1 p-4">
      <header className="flex flex-wrap items-center gap-2">
        {/* THE ONE SATURATED FILL. Nothing else on this page may approach it. */}
        <Badge variant="arb">
          {`arbitrage ${formatPercentPoints(signal.return_percent)}`}
        </Badge>
        <span className="t-ui text-ink">
          {marketTypeShortLabel(signal.market_type)}
          {line === '' ? '' : ` ${line}`}
        </span>
        <span className="t-mono break-all text-ink-muted">
          {signal.market_id}
        </span>
        {/* One book quoting an under-round market is the STRONGER finding: there
            is no execution risk from a second book moving between bets. It is
            labelled rather than hidden, because a reader would otherwise assume
            a single-book row was a bug. */}
        {signal.distinct_books === 1 ? (
          <Badge variant="neutral">single book</Badge>
        ) : (
          <Badge variant="neutral">
            {`${String(signal.distinct_books)} books`}
          </Badge>
        )}
      </header>

      <dl className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <Fact term="Implied sum">{signal.implied_sum.toFixed(4)}</Fact>
        <Fact term="Return per unit staked">
          {formatPercentPoints(signal.return_percent)}
        </Fact>
        <Fact term="Leg observations spread">
          {formatAgeSeconds(signal.observed_spread_seconds)}
        </Fact>
        <Fact term="Oldest leg age">
          {formatAgeSeconds(signal.oldest_leg_age_seconds)}
        </Fact>
      </dl>

      <div className="board-scroll">
        <table className="w-full border-collapse">
          <caption className="sr-only">
            {`The legs of an arbitrage on market ${signal.market_id}, with each leg's book, price, stake share and age.`}
          </caption>
          <thead>
            <tr className="border-b border-rule">
              <th scope="col" className="t-label px-2 py-1 text-left text-ink-muted">
                Selection
              </th>
              <th scope="col" className="t-label px-2 py-1 text-left text-ink-muted">
                Book
              </th>
              <th scope="col" className="t-label px-2 py-1 text-right text-ink-muted">
                Price
              </th>
              <th scope="col" className="t-label px-2 py-1 text-right text-ink-muted">
                Stake share
              </th>
              <th scope="col" className="t-label px-2 py-1 text-right text-ink-muted">
                Age
              </th>
            </tr>
          </thead>
          <tbody>
            {signal.legs.map((leg) => (
              <tr key={leg.leg_index} className="border-b border-rule last:border-b-0">
                <th scope="row" className="px-2 py-1 text-left">
                  <span className="t-ui block text-ink">
                    {selectionRoleLabel(leg.role)}
                  </span>
                  <span className="t-mono block break-all text-ink-muted">
                    {leg.selection_id}
                  </span>
                </th>
                <td className="t-mono px-2 py-1 text-left text-ink-2">
                  {leg.book_id}
                </td>
                <td className="t-price-sm px-2 py-1 text-right text-ink">
                  {formatOdds(leg.decimal_odds, format)}
                </td>
                {/* A FRACTION of the total outlay, not an amount. This API does
                    not choose a bankroll and neither does this table. */}
                <td className="t-mono px-2 py-1 text-right text-ink-2">
                  {formatFractionAsPercent(leg.stake_fraction, 1)}
                </td>
                <td className="t-mono px-2 py-1 text-right text-ink-2">
                  {formatAgeSeconds(leg.age_seconds)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <p className="t-mono text-ink-muted">
        {`existed from ${formatAbsolute(signal.observed_at, timeZone)} · detector bounds ${formatAgeSeconds(signal.max_leg_age_seconds, 0)} age / ${formatAgeSeconds(signal.max_observed_spread_seconds, 0)} spread`}
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
      {/* `ink`, never `money`. A return per unit staked is a rate; the green is
          spent on the badge above and nowhere else. */}
      <dd className="t-mono text-ink">{children}</dd>
    </div>
  );
}
