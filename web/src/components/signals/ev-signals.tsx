'use client';

/**
 * The +EV finder's client half: the keyset pager over a server-rendered first
 * page.
 *
 * # No number on this table is money, and the rendering says so
 *
 * DESIGN.md's colour rule is one job per hue, and `money` (green) means a
 * currency amount: a stake, a payout, a P&L figure. An EXPECTED VALUE PERCENTAGE
 * IS NOT MONEY. It is a rate of return on a hypothetical unit stake, and this
 * endpoint deliberately does not choose a stake — there is no `*_minor` field
 * anywhere in its payload. So every figure below is rendered in `ink`, and the
 * only place green appears is the tinted `+EV` badge, which is a SIGNAL marker
 * (8% fill, 40% border) rather than a colour on a quantity. That is exactly the
 * treatment `fair-value-panel.tsx` already uses for the same fact on the live
 * stream, and the two surfaces agreeing matters more than either choice does.
 *
 * # Every filter is re-sent on every page
 *
 * The cursor is bound to the whole filter set. Unlike the board — where `book`
 * changes how a page is rendered — each filter here changes WHICH ROWS are in
 * the set, so a follow-up page fetched with a different one is a 400 rather than
 * a differently filtered page. The parameters arrive as props from the route so
 * that the byte-identical values the first page used are the ones the second
 * page sends; a client-side `new Date()` would not be.
 */

import { useInfiniteQuery } from '@tanstack/react-query';

import { Badge, Button } from '@/components/ui';
import { SignalsEmpty, SignalsUnavailable } from './signals-empty';
import { useLocalTimeZone } from '@/components/event/use-event-live';
import type { EVSignalParams } from '@/lib/api/client';
import { evSignalsInfiniteQueryOptions } from '@/lib/api/queries';
import type { SchemaEvSignal, SchemaEvSignalPage } from '@/lib/api/schema';
import {
  formatAgeSeconds,
  formatFractionAsPercent,
  formatMarketLine,
  formatPercentPoints,
  formatSignedPercentPoints,
} from '@/lib/analytics/format';
import { formatOdds } from '@/lib/odds/format';
import { marketTypeShortLabel } from '@/lib/odds/line';
import { useOddsFormat } from '@/lib/store/preferences';
import { formatAbsolute } from '@/lib/time';

export interface EVSignalsProps {
  /** The route's own fetch. Real findings, already rendered on the server. */
  readonly initialData: SchemaEvSignalPage;
  /** The exact parameters the first page was fetched with. */
  readonly params: EVSignalParams;
  /** "the last 6 hours" — for the empty state, which names what it looked at. */
  readonly windowPhrase: string;
}

export function EVSignals({ initialData, params, windowPhrase }: EVSignalsProps) {
  const format = useOddsFormat();
  const timeZone = useLocalTimeZone();

  const query = useInfiniteQuery({
    ...evSignalsInfiniteQueryOptions(params),
    initialData: { pages: [initialData], pageParams: [undefined] },
  });

  const rows = query.data.pages.flatMap((page) => page.data);

  if (rows.length === 0) {
    return (
      <SignalsEmpty
        feed="ev"
        windowPhrase={windowPhrase}
        thresholds={[
          `min expected value ${formatPercentPoints(params.minEvPercent ?? 0)}`,
          params.league === undefined ? 'every league' : `league ${params.league}`,
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

      <div className="board-scroll">
        <table className="w-full border-collapse">
          <caption className="sr-only">
            Offered prices scored as positive expected value against the sharp
            reference book, best expected value first. Every figure is a rate,
            not an amount of money.
          </caption>
          <thead>
            <tr className="border-b border-rule">
              <Th align="left">Market</Th>
              <Th align="left">Book</Th>
              <Th align="right">Price</Th>
              <Th align="right">Fair price</Th>
              <Th align="right">EV</Th>
              <Th align="right">Edge</Th>
              <Th align="right">Kelly</Th>
              <Th align="right">Quote age</Th>
            </tr>
          </thead>
          <tbody>
            {rows.map((signal) => (
              <EVRow
                key={`${signal.selection_id}:${signal.book_id}:${signal.quote_observed_at}`}
                signal={signal}
                format={format}
                timeZone={timeZone}
              />
            ))}
          </tbody>
        </table>
      </div>

      {/* Reads `has_more`, never the row count. A short page is not the last
          page on any keyset endpoint in this API. */}
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

function Th({
  align,
  children,
}: {
  readonly align: 'left' | 'right';
  readonly children: React.ReactNode;
}) {
  return (
    <th
      scope="col"
      className={`t-label px-2 py-2 text-ink-muted ${align === 'left' ? 'text-left' : 'text-right'}`}
    >
      {children}
    </th>
  );
}

function EVRow({
  signal,
  format,
  timeZone,
}: {
  readonly signal: SchemaEvSignal;
  readonly format: ReturnType<typeof useOddsFormat>;
  readonly timeZone: string;
}) {
  const line = formatMarketLine(signal.market_type, signal.line);

  return (
    <tr className="border-b border-rule">
      <th scope="row" className="px-2 py-2 text-left align-top">
        <span className="t-ui block text-ink">
          {marketTypeShortLabel(signal.market_type)}
          {line === '' ? '' : ` ${line}`}
        </span>
        {/* The identifiers are the engineering register: mono, muted, and
            present because a reader following a finding back to the board needs
            them. They are never the headline. */}
        <span className="t-mono block break-all text-ink-muted">
          {signal.selection_id}
        </span>
      </th>

      <td className="px-2 py-2 text-left align-top">
        <span className="t-mono block text-ink-2">{signal.book_id}</span>
        {/* An EV number is meaningless without the book it was measured
            against, so the reference is on every row rather than being a fact a
            reader is assumed to already hold. */}
        <span className="t-mono block text-ink-muted">
          {signal.book_id === signal.reference_book_id
            ? 'under-round vs itself'
            : `vs ${signal.reference_book_id}`}
        </span>
        <span className="t-mono block text-ink-muted">
          devig {signal.devig_method}
        </span>
      </td>

      <td className="t-price-sm px-2 py-2 text-right align-top text-ink">
        {formatOdds(signal.offered_decimal, format)}
      </td>

      <td className="t-price-sm px-2 py-2 text-right align-top text-ink-2">
        {formatOdds(signal.fair_decimal, format)}
      </td>

      <td className="px-2 py-2 text-right align-top">
        <span className="flex flex-col items-end gap-1">
          {/* `ink`, not `money`. An expected value is a rate, not an amount —
              the tinted badge below is the signal marker and it is the only
              green on this table. */}
          <span className="t-price-sm text-ink">
            {formatSignedPercentPoints(signal.expected_value_percent)}
          </span>
          <Badge variant="money">+EV</Badge>
        </span>
      </td>

      <td className="t-mono px-2 py-2 text-right align-top text-ink-2">
        {formatPercentPoints(signal.edge_percent)}
      </td>

      <td className="px-2 py-2 text-right align-top">
        <span className="t-mono block text-ink-2">
          {formatFractionAsPercent(signal.kelly, 2)}
        </span>
        {/* Full Kelly is famously too aggressive to bet, so the fraction the
            detector applied travels beside it rather than being implied. */}
        <span className="t-mono block text-ink-muted">
          {`×${signal.kelly_fraction.toFixed(2)} → ${formatFractionAsPercent(signal.fractional_kelly, 2)}`}
        </span>
      </td>

      <td className="px-2 py-2 text-right align-top">
        <span className="t-mono block text-ink-2">
          {formatAgeSeconds(signal.quote_age_seconds)}
        </span>
        <span className="t-mono block text-ink-muted">
          {formatAbsolute(signal.quote_observed_at, timeZone)}
        </span>
      </td>
    </tr>
  );
}
