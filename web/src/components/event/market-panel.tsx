'use client';

/**
 * One market on the event detail page: its line, its status, its selections,
 * and — on demand — the three analytic views that hang off it.
 *
 * # The tree is REST; the prices in it are the socket
 *
 * Which markets exist, which selections hang off them and in what order all
 * come from the REST payload and only from there (ADR 0009 D3). The prices
 * inside are `components/board/price-cell.tsx` — the SAME component the board
 * renders — so a price behaves identically on both surfaces: one delta rail,
 * one accessible name, one digit roll, one subscription granularity.
 *
 * # The line is not in the heading
 *
 * A heading reading "Spread -4.5" is wrong the moment the two sides sit at
 * different numbers, which is the normal state of a market being moved. The
 * market's CONSENSUS line is stated once in the engineering line below the
 * heading; each selection's own line is rendered by its price cell, from that
 * selection's own price.
 *
 * # Everything analytic is behind one disclosure
 *
 * The comparison grid costs a request per market, the fair value panel needs a
 * live stream record, and the chart needs a history query. Rendering all three
 * for every market on an event would be a dozen requests for panels nobody has
 * looked at. One `aria-expanded` button, three tabs, and Radix unmounts the
 * inactive ones — so a tab that is not open has not fetched.
 */

import { useId, useMemo, useState } from 'react';

import { LineMovementChart } from '@/components/chart/line-movement-chart';
import { BookComparison } from '@/components/event/book-comparison';
import { FairValuePanel } from '@/components/event/fair-value-panel';
import { useLocalTimeZone } from '@/components/event/use-event-live';
import { PriceCell } from '@/components/board/price-cell';
import {
  Badge,
  Button,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from '@/components/ui';
import type {
  SchemaBook,
  SchemaMarket,
  SchemaMarketStatus,
  SchemaSelection,
} from '@/lib/api/schema';
import {
  formatLineNumber,
  marketTypeHasLine,
  marketTypeLabel,
  selectionRoleLabel,
} from '@/lib/odds/line';
import {
  formatAbsolute,
  formatCompactDuration,
  stalenessSeconds,
} from '@/lib/time';

export interface MarketPanelProps {
  readonly eventId: string;
  readonly eventName: string;
  readonly market: SchemaMarket;
  /** `EventDetail.as_of` — the instant every figure on this page is measured against. */
  readonly asOf: string;
  /** Whether the socket is currently carrying this market on the event channel. */
  readonly live: boolean;
  /** The book catalogue, for turning a slug into a name. */
  readonly books: ReadonlyMap<string, SchemaBook>;
}

function statusVariant(status: SchemaMarketStatus): 'neutral' | 'info' {
  return status === 'suspended' ? 'info' : 'neutral';
}

/** Every book slug that quotes anything on this market, in a stable order. */
function quotingBookSlugs(selections: readonly SchemaSelection[]): readonly string[] {
  const slugs = new Set<string>();
  for (const selection of selections) {
    for (const price of selection.prices) slugs.add(price.book_slug);
  }
  return [...slugs].sort();
}

export function MarketPanel({
  eventId,
  eventName,
  market,
  asOf,
  live,
  books,
}: MarketPanelProps) {
  const timeZone = useLocalTimeZone();
  const panelId = useId();
  const [open, setOpen] = useState(false);
  const [chartSelectionId, setChartSelectionId] = useState<string | null>(null);

  const bookSlugs = useMemo(
    () => quotingBookSlugs(market.selections),
    [market.selections],
  );

  const label = marketTypeLabel(market.type);
  const marketLabel = `${label} — ${eventName}`;
  const line = market.line ?? null;
  // Narrowed here rather than through a boolean flag: `hasLine` would not carry
  // the null check into `formatLineNumber` under `strictNullChecks`.
  const lineText =
    marketTypeHasLine(market.type) && line !== null
      ? formatLineNumber(line)
      : null;
  const age = stalenessSeconds(market.observed_at, asOf);

  const chartSelection =
    market.selections.find(
      (selection) => selection.id === chartSelectionId,
    ) ?? market.selections[0];

  return (
    <section className="rounded-card border border-rule bg-ground-1">
      {/* `flex-nowrap` + `min-w-0` on the text column, deliberately.
        *
        * With `flex-wrap` the analysis toggle sat beside the title on a market
        * whose provenance line was short (moneyline, no consensus line) and
        * dropped onto its own row on one whose line was long (spread, total) —
        * so three stacked panels put the same control in two different places.
        * The control's position is not information and must not move.
        *
        * `min-w-0` is what makes it work: without it the text column's intrinsic
        * width is the unbroken mono id, the flex item refuses to shrink below it,
        * and the row overflows instead. The long line wraps inside its own
        * column, which is where wrapping belongs. */}
      <header className="flex flex-nowrap items-start justify-between gap-4 border-b border-rule px-4 py-3">
        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <div className="flex flex-wrap items-center gap-2">
            <h3 className="t-h3 text-ink">{label}</h3>
            {market.status === 'open' ? null : (
              <Badge variant={statusVariant(market.status)}>
                {market.status}
              </Badge>
            )}
          </div>

          {/* A player prop is about somebody. Rendered only where the payload
              carries a subject; it is null on every other market type. */}
          {market.subject === null || market.subject === undefined ? null : (
            <p className="t-body text-ink-2">{market.subject}</p>
          )}

          <p className="t-mono text-ink-muted">
            {[
              `market ${market.id}`,
              lineText === null ? null : `consensus line ${lineText}`,
              `${String(market.selections.length)} selections`,
              `observed ${formatAbsolute(market.observed_at, timeZone)}`,
              age === null
                ? null
                : `${formatCompactDuration(age)} before assembly`,
              live ? 'on the stream' : 'not yet on the stream',
            ]
              .filter((part): part is string => part !== null)
              .join(' · ')}
          </p>
        </div>

        <Button
          type="button"
          size="sm"
          variant="outline"
          aria-expanded={open}
          aria-controls={panelId}
          onClick={() => {
            setOpen((current) => !current);
          }}
        >
          {open ? 'Hide analysis' : 'Books, fair value, movement'}
        </Button>
      </header>

      <div className="board-scroll px-4 py-3">
        <table className="w-full border-collapse">
          {/* Names which "best" this is. The designation is the API's, made
              when the page was assembled; the CELL then tracks that book's
              price live, so a reader is never shown a price and a book that
              did not go together. The Books column names the book. */}
          <caption className="sr-only">
            {`${marketLabel}. For each selection: the best price across every quoting book as of ${formatAbsolute(asOf, timeZone)}, kept live at that book, and how many books are quoting it.`}
          </caption>
          <thead>
            <tr className="border-b border-rule">
              <th scope="col" className="t-label px-2 py-2 text-left text-ink-muted">
                Selection
              </th>
              <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                Best price
              </th>
              <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                Books
              </th>
            </tr>
          </thead>
          <tbody>
            {/* The API returns selections in display order — home, draw, away,
                over, under, outright. It is not lexicographic and it is not
                re-sorted here, so every client renders the same tree. */}
            {market.selections.map((selection) => {
              const best = selection.best_price ?? null;
              const bookSlug = best?.book_slug ?? null;
              const bookLabel =
                bookSlug === null
                  ? null
                  : (books.get(bookSlug)?.name ?? bookSlug);

              return (
                <tr key={selection.id} className="border-b border-rule">
                  <th scope="row" className="px-2 py-2 text-left align-middle">
                    <span className="t-body block text-ink">
                      {selection.name}
                    </span>
                    <span className="t-mono block text-ink-muted">
                      {selectionRoleLabel(selection.role)}
                    </span>
                  </th>

                  <td className="w-40 px-2 py-2 align-middle">
                    <PriceCell
                      eventId={eventId}
                      eventName={eventName}
                      marketId={market.id}
                      marketType={market.type}
                      marketStatus={market.status}
                      selectionId={selection.id}
                      selectionName={selection.name}
                      selectionRole={selection.role}
                      restPrice={best}
                      bookLabel={bookLabel}
                    />
                  </td>

                  <td className="t-mono px-2 py-2 text-right align-middle text-ink-muted">
                    {selection.prices.length === 0
                      ? 'none quoting'
                      : `${String(selection.prices.length)} quoting${bookSlug === null ? '' : ` · best ${bookSlug}`}`}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      {/* The container exists whether or not it is open, so `aria-controls`
          always resolves to a real element. Its CONTENTS are mounted only when
          open, which is what keeps the comparison request, the stream
          subscription and the history query off an unopened panel. */}
      <div
        id={panelId}
        hidden={!open}
        className="border-t border-rule px-4 py-4"
      >
        {open ? (
          <Tabs defaultValue="books">
            <TabsList>
              <TabsTrigger value="books">Books</TabsTrigger>
              <TabsTrigger value="fair">Fair value</TabsTrigger>
              <TabsTrigger value="movement">Movement</TabsTrigger>
            </TabsList>

            <TabsContent value="books" className="pt-4">
              <BookComparison
                marketId={market.id}
                marketType={market.type}
                marketLine={line}
                marketLabel={marketLabel}
                selections={market.selections.map((selection) => ({
                  id: selection.id,
                  name: selection.name,
                  role: selection.role,
                }))}
              />
            </TabsContent>

            <TabsContent value="fair" className="pt-4">
              <FairValuePanel marketId={market.id} marketLabel={marketLabel} />
            </TabsContent>

            <TabsContent value="movement" className="pt-4">
              {chartSelection === undefined ? (
                <p className="t-body text-ink-2">
                  This market has no selections to chart.
                </p>
              ) : (
                <div className="flex flex-col gap-4">
                  <div
                    role="group"
                    aria-label="Selection to chart"
                    className="flex flex-wrap items-center gap-1"
                  >
                    {market.selections.map((selection) => {
                      const active = selection.id === chartSelection.id;
                      return (
                        <Button
                          key={selection.id}
                          type="button"
                          size="sm"
                          variant={active ? 'default' : 'ghost'}
                          aria-pressed={active}
                          onClick={() => {
                            setChartSelectionId(selection.id);
                          }}
                        >
                          {selection.name}
                        </Button>
                      );
                    })}
                  </div>

                  <LineMovementChart
                    key={chartSelection.id}
                    selectionId={chartSelection.id}
                    selectionName={chartSelection.name}
                    marketLabel={label}
                    bookSlugs={bookSlugs}
                  />
                </div>
              )}
            </TabsContent>
          </Tabs>
        ) : null}
      </div>
    </section>
  );
}
