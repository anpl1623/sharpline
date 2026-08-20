'use client';

/**
 * One event, one `<tr>`.
 *
 * This component does NOT re-render when a price moves. Every price on the row
 * subscribes to its own `(market, selection, book)` triple inside `PriceCell`,
 * which is what makes DESIGN.md's "per-cell decay timers; do not re-render the
 * row" true of the implementation rather than only of the intention. The row
 * re-renders when the tree changes — a new page, a book filter, an odds format —
 * and at no other time.
 *
 * The game cell is a `<th scope="row">`, so a screen reader moving across the
 * row announces the game as its header and the market as its column header
 * without either being repeated in the markup.
 */

import type { SchemaBoardEntry } from '@/lib/api/schema';
import { EventCell } from './event-cell';
import { MarketColumn } from './market-column';
import { BOARD_COLUMNS, marketForColumn } from './use-board-live';
import type { BoardCatalogue } from './use-board-live';

export interface BoardRowProps {
  readonly entry: SchemaBoardEntry;
  readonly catalogue: BoardCatalogue;
  readonly bookFilter: readonly string[];
  readonly timeZone: string;
}

export function BoardRow({ entry, catalogue, bookFilter, timeZone }: BoardRowProps) {
  const { event, markets } = entry;

  return (
    <tr className="board-row">
      {/* `board-game-cell` is where the mobile pass lives: below 768px it goes
          sticky at 132px with an opaque backdrop, so the row is never orphaned
          from its identity while the market columns scroll under it. Above
          768px the class is inert and the column simply absorbs the slack. */}
      <th
        scope="row"
        className="board-game-cell min-w-[132px] border-b border-rule bg-ground-1 px-3 py-0.5 text-left align-middle font-normal"
      >
        <EventCell event={event} timeZone={timeZone} />
      </th>
      {BOARD_COLUMNS.map((column) => (
        <MarketColumn
          key={column}
          eventId={event.id}
          eventName={event.name}
          column={column}
          market={marketForColumn(markets, column)}
          catalogue={catalogue}
          bookFilter={bookFilter}
        />
      ))}
    </tr>
  );
}
