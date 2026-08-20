'use client';

/**
 * One market column of one row: the `<td>` that holds a market's stacked prices.
 *
 * DESIGN.md's board geometry is a 36px row with "price cell 15px tall, 2px gap,
 * stacked two per market". Two stacked prices inside one column of one row can
 * only be one table cell, so the accessible-name requirement — market, selection
 * and price on every price — is carried by each price's own control rather than
 * by a `<td>` per price. Splitting the market into two cells would mean either
 * two `<tr>` per event (which contradicts the 36px/56px row heights the design
 * system's own CSS encodes) or six side-by-side columns (which contradicts the
 * stacking). This is the one place the two constraints could not both be met
 * literally, and the accessible name is the half that was kept whole.
 *
 * A market with three selections — a three-way moneyline — simply renders three
 * cells and the row grows: `.board-row` sets a height, and a table row treats a
 * height as a minimum. Dropping the draw to keep the geometry would be dropping
 * real data.
 */

import type { SchemaMarket } from '@/lib/api/schema';
import { marketTypeLabel } from '@/lib/odds/line';
import { PriceCell } from './price-cell';
import { bookLabel, displayPrice, orderedSelections } from './use-board-live';
import type { BoardCatalogue, BoardColumn } from './use-board-live';

/** The stack itself. `min-h` rather than `h`, so a third selection can grow it. */
const STACK = 'flex min-h-[52px] flex-col justify-center gap-[2px] md:min-h-[32px]';

/**
 * `bg-ground-1` on every cell of the row, not just on the game cell. Below 768px
 * the sticky game column has to be opaque or the price cells scroll visibly
 * under it, and the design system's CSS paints it `ground-1`; a row whose other
 * cells were transparent would show a seam down the sticky edge at exactly the
 * moment the column is doing its job.
 */
const CELL =
  'board-market-col border-b border-rule bg-ground-1 px-0.5 py-0.5 align-middle';

export interface MarketColumnProps {
  readonly eventId: string;
  readonly column: BoardColumn;
  /** Null when this event does not offer the column's market. */
  readonly market: SchemaMarket | null;
  readonly catalogue: BoardCatalogue;
  readonly bookFilter: readonly string[];
}

export function MarketColumn({
  eventId,
  column,
  market,
  catalogue,
  bookFilter,
}: MarketColumnProps) {
  if (market === null) {
    return (
      <td className={CELL}>
        <div className={STACK}>
          <span className="sr-only">{marketTypeLabel(column)} not offered</span>
          {/* Two empty wells, in the same 15px box a real price sits in, so the
              column's rhythm survives an event that does not offer this market.
              The outer span takes the slack exactly as a priced cell's link
              does; the well itself never grows. */}
          <span className="flex min-h-[15px] flex-1 items-center">
            <span className="price-cell w-full" data-price-cell="none" />
          </span>
          <span className="flex min-h-[15px] flex-1 items-center">
            <span className="price-cell w-full" data-price-cell="none" />
          </span>
        </div>
      </td>
    );
  }

  const selections = orderedSelections(market);

  return (
    <td className={CELL}>
      <div className={STACK}>
        {selections.map((selection) => {
          const price = displayPrice(selection, bookFilter);
          return (
            <PriceCell
              key={selection.id}
              eventId={eventId}
              marketId={market.id}
              marketType={market.type}
              marketStatus={market.status}
              selectionId={selection.id}
              selectionName={selection.name}
              selectionRole={selection.role}
              restPrice={price}
              bookLabel={bookLabel(catalogue, price?.book_slug ?? null)}
            />
          );
        })}
      </div>
    </td>
  );
}
