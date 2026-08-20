'use client';

/**
 * Multi-book comparison for one market (CLAUDE.md §6).
 *
 * Rows are books, columns are selections, plus each book's overround. Fetched
 * from `GET /markets/{id}/prices` on demand — the panel this lives in is
 * collapsed until a reader opens it, and a comparison for every market on an
 * event would be a request per market for a table nobody is looking at.
 *
 * # This table is a SNAPSHOT, and deliberately does not tick
 *
 * `best` and `overround` are computed server-side at `as_of`. If the price
 * cells here were driven by the socket while those two stayed frozen, the
 * highlighted "best" cell would visibly stop being the best price and the
 * margin beside it would describe prices that are no longer on screen. A table
 * that is internally consistent at a stated instant is more useful than one
 * that is live in three columns and stale in two, so this renders the payload
 * it was given, says when that was, and offers to fetch a new one.
 *
 * The live surface is the market tree above it, where every cell carries its
 * own delta rail.
 *
 * # `best` is not recomputed here
 *
 * The API computes it so that "best" means one thing on every surface in the
 * product. A client that re-derived it would eventually disagree with the
 * arbitrage detector, which reads the same field.
 */

import { useQuery } from '@tanstack/react-query';

import {
  Badge,
  Button,
  Skeleton,
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui';
import { useLocalTimeZone } from '@/components/event/use-event-live';
import { developerDetail, userFacingMessage } from '@/lib/api/errors';
import { marketComparisonQueryOptions } from '@/lib/api/queries';
import type {
  SchemaBestPrice,
  SchemaBookQuote,
  SchemaMarketType,
  SchemaSelectionRole,
} from '@/lib/api/schema';
import { NO_PRICE, formatOdds, renderPercent } from '@/lib/odds/format';
import { formatLine, marketTypeHasLine } from '@/lib/odds/line';
import { useOddsFormat } from '@/lib/store/preferences';
import {
  formatAbsolute,
  formatDurationWords,
  formatStaleness,
  stalenessSeconds,
} from '@/lib/time';

/** The selection identity the grid needs. Names come from REST and only REST. */
export interface ComparisonSelection {
  readonly id: string;
  readonly name: string;
  readonly role: SchemaSelectionRole;
}

export interface BookComparisonProps {
  readonly marketId: string;
  readonly marketType: SchemaMarketType;
  /** The market's consensus line, for the column headings. May be null. */
  readonly marketLine: number | null;
  /** "Moneyline — Team A at Team B". Used as the table caption. */
  readonly marketLabel: string;
  /** In the API's display order. NOT re-sorted here. */
  readonly selections: readonly ComparisonSelection[];
}

function quoteFor(book: SchemaBookQuote, selectionId: string) {
  return book.quotes.find((quote) => quote.selection_id === selectionId);
}

/**
 * `overround: null` means the book has not quoted every selection. There is no
 * substitute number to show: `sum(1/d) - 1` over a partial market is a smaller
 * number that looks like a better margin, which is the most misleading possible
 * thing to print in this column.
 */
function OverroundUnavailable() {
  return (
    <TooltipProvider>
      <Tooltip>
        <TooltipTrigger asChild>
          <button
            type="button"
            className="t-price-sm rounded-price px-1 text-ink-muted underline decoration-dotted underline-offset-4"
          >
            <span aria-hidden="true">{NO_PRICE}</span>
            <span className="sr-only">
              No overround. This book has not quoted every selection on the
              market, and an overround computed over a partial market is not a
              margin.
            </span>
          </button>
        </TooltipTrigger>
        <TooltipContent>
          This book has not quoted every selection. An overround over a partial
          market is not a margin — it is a smaller number that looks like a
          better price.
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}

export function BookComparison({
  marketId,
  marketType,
  marketLine,
  marketLabel,
  selections,
}: BookComparisonProps) {
  const format = useOddsFormat();
  const timeZone = useLocalTimeZone();
  const comparison = useQuery(marketComparisonQueryOptions(marketId));

  if (comparison.isPending) {
    return (
      <div aria-busy="true" className="flex flex-col gap-2">
        <Skeleton className="h-8 w-full" />
        <Skeleton className="h-8 w-full" />
        <Skeleton className="h-8 w-full" />
      </div>
    );
  }

  if (comparison.isError) {
    const error: unknown = comparison.error;
    return (
      <div className="flex flex-col gap-3 rounded-card border border-rule bg-ground-1 p-4">
        <p className="t-body text-ink">{userFacingMessage(error)}</p>
        {developerDetail(error) === null ? null : (
          <details>
            <summary className="t-ui text-ink-muted">Details</summary>
            <p className="t-mono text-ink-muted">{developerDetail(error)}</p>
          </details>
        )}
        <div>
          <Button
            type="button"
            size="sm"
            onClick={() => {
              void comparison.refetch();
            }}
          >
            Try again
          </Button>
        </div>
      </div>
    );
  }

  const data = comparison.data;
  const best = new Map(
    data.best.map(
      (entry): [string, SchemaBestPrice] => [entry.selection_id, entry],
    ),
  );
  const showsLine = marketTypeHasLine(marketType);

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="t-mono text-ink-muted">
          {`${String(data.books.length)} books · snapshot ${formatAbsolute(data.as_of, timeZone)}`}
        </p>
        <Button
          type="button"
          size="sm"
          variant="ghost"
          onClick={() => {
            void comparison.refetch();
          }}
        >
          Refresh snapshot
        </Button>
      </div>

      {data.books.length === 0 ? (
        <p className="t-body text-ink-2">
          No book has quoted this market inside the freshness window. That is an
          answer, not a missing one.
        </p>
      ) : (
        <div className="board-scroll -mx-4 px-4">
          <table className="w-full border-collapse">
            <caption className="sr-only">
              {`Every book's price on ${marketLabel}, and each book's overround, as of ${formatAbsolute(data.as_of, timeZone)}.`}
            </caption>
            <thead>
              <tr className="border-b border-rule">
                <th
                  scope="col"
                  className="t-label px-2 py-2 text-left text-ink-muted"
                >
                  Book
                </th>
                {selections.map((selection) => {
                  const line = showsLine
                    ? formatLine(marketType, selection.role, marketLine)
                    : '';
                  return (
                    <th
                      key={selection.id}
                      scope="col"
                      className="px-2 py-2 text-right"
                    >
                      <span className="t-label block text-ink-2">
                        {selection.name}
                      </span>
                      {line === '' ? null : (
                        <span className="t-price-sm block text-ink-muted">
                          {line}
                        </span>
                      )}
                    </th>
                  );
                })}
                <th
                  scope="col"
                  className="t-label px-2 py-2 text-right text-ink-muted"
                >
                  Overround
                </th>
              </tr>
            </thead>
            <tbody>
              {data.books.map((book) => (
                <tr key={book.book_id} className="border-b border-rule">
                  <th scope="row" className="px-2 py-2 text-left align-top">
                    <span className="t-ui block text-ink">
                      {book.book_name}
                    </span>
                    <span className="flex flex-wrap gap-1 pt-1">
                      {book.is_reference ? (
                        <Badge variant="info">reference</Badge>
                      ) : null}
                      {/* ADR 0003: a synthetic book's quote is a statement
                          about a random number generator, and every surface
                          rendering one must be able to say so. */}
                      {book.book_kind === 'synthetic' ? (
                        <Badge variant="neutral">synthetic</Badge>
                      ) : null}
                    </span>
                  </th>

                  {selections.map((selection) => {
                    const quote = quoteFor(book, selection.id);
                    if (quote === undefined) {
                      return (
                        <td
                          key={selection.id}
                          className="px-2 py-2 text-right align-top"
                        >
                          <span className="t-price text-ink-muted" aria-hidden="true">
                            {NO_PRICE}
                          </span>
                          <span className="sr-only">Not quoted</span>
                        </td>
                      );
                    }

                    const isBest =
                      best.get(selection.id)?.book_slug === book.book_slug;
                    const age = stalenessSeconds(quote.observed_at, data.as_of);
                    const quoteLine = quote.line ?? null;
                    const differs =
                      showsLine &&
                      quoteLine !== null &&
                      marketLine !== null &&
                      quoteLine !== marketLine;

                    return (
                      <td
                        key={selection.id}
                        className="px-2 py-2 text-right align-top"
                      >
                        <span
                          className={
                            isBest
                              ? 'inline-flex flex-col items-end gap-1 rounded-price border border-rule-hi bg-ground-2 px-2 py-1'
                              : 'inline-flex flex-col items-end gap-1 px-2 py-1'
                          }
                        >
                          <span className="t-price text-ink">
                            {formatOdds(quote.decimal_odds, format)}
                          </span>
                          {isBest ? (
                            <span className="t-label text-ink-2">Best</span>
                          ) : null}
                          {differs && quoteLine !== null ? (
                            <span className="t-mono text-ink-muted">
                              {`at ${formatLine(marketType, selection.role, quoteLine)}`}
                            </span>
                          ) : null}
                          <span className="t-mono text-ink-muted">
                            <span aria-hidden="true">
                              {formatStaleness(quote.observed_at, data.as_of)}
                            </span>
                            <span className="sr-only">
                              {`observed ${formatDurationWords(age)} before this snapshot`}
                            </span>
                          </span>
                        </span>
                      </td>
                    );
                  })}

                  <td className="px-2 py-2 text-right align-top">
                    {book.overround === null || book.overround === undefined ? (
                      <OverroundUnavailable />
                    ) : (
                      <span className="t-price-sm text-ink-2">
                        {renderPercent(book.overround, 2)}
                      </span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
