'use client';

/**
 * The board. A real `<table>`, at every viewport width.
 *
 * DESIGN.md's mobile pass is explicit and it is implemented here rather than
 * approximated: below 768px the table SURVIVES, keeps all three market columns
 * and keeps every price cell at its full desktop size. The game column goes
 * `sticky left: 0` at 132px with an opaque backdrop, the market columns scroll
 * horizontally inside an `overflow-x: auto` wrapper with `scroll-snap-type: x
 * proximity`, and the row grows to 56px so the two competitors stack instead of
 * truncating. All of that lives in `globals.css` under `.board-scroll`,
 * `.board-game-cell`, `.board-market-col` and `.board-row`; this file's job is
 * to put those classes on the right elements and to give the table a minimum
 * width, because a table that shrinks to fit has nothing to scroll.
 *
 * Card-per-event is REJECTED by DESIGN.md and the reason is worth restating: a
 * board is a table because the eye compares one market down many games in a
 * single vertical sweep, and a layout where only one game is ever on screen has
 * nothing for a moving price to be loud against.
 *
 * The board is FULL BLEED. There is no max-width here and there must not be one.
 */

import Link from 'next/link';

import { marketTypeLabel } from '@/lib/odds/line';
import { BoardRow } from './board-row';
import { BOARD_COLUMNS, useDisplayTimeZone } from './use-board-live';
import type { BoardCatalogue, BoardGroup } from './use-board-live';

/** 132px game column + three 116px market columns, plus the cells' own padding. */
// 132 (game) + 3 x 140 (market). Sized from the WIDEST content a market column
// can hold, not from a round number: a totals line is `O 244.5` in `t-price-sm`
// beside `-110` in `t-price`, and at the previous 116px that pair wrapped onto
// two lines and pushed the row from 36px to 59px. A board whose row height
// depends on how many digits a basketball total happens to have is not a board
// with a row height. The cells themselves are `whitespace-nowrap` so a future
// overflow clips visibly instead of silently re-flowing the whole table.
const TABLE_MIN_WIDTH = 'min-w-[552px]';

const HEAD_CELL = 't-label border-b border-rule bg-ground-1 px-3 py-2 text-left align-middle text-ink-muted';

export interface BoardTableProps {
  readonly groups: readonly BoardGroup[];
  readonly catalogue: BoardCatalogue;
  readonly bookFilter: readonly string[];
  /** The table's own summary, for a screen reader. Never rendered visually. */
  readonly caption: string;
}

export function BoardTable({
  groups,
  catalogue,
  bookFilter,
  caption,
}: BoardTableProps) {
  const timeZone = useDisplayTimeZone();

  return (
    <div className="board-scroll w-full">
      <table className={`w-full ${TABLE_MIN_WIDTH} border-separate border-spacing-0`}>
        <caption className="sr-only">{caption}</caption>
        <colgroup>
          {/* Unset, so the game column absorbs everything the market columns
              do not take. Its 132px floor is on the cells themselves. */}
          <col />
          {BOARD_COLUMNS.map((column) => (
            <col key={column} className="w-[140px]" />
          ))}
        </colgroup>

        <thead>
          <tr>
            <th scope="col" className={`board-game-cell min-w-[132px] ${HEAD_CELL}`}>
              Game
            </th>
            {BOARD_COLUMNS.map((column) => (
              <th
                key={column}
                scope="col"
                className={`board-market-col ${HEAD_CELL} text-center`}
              >
                {marketTypeLabel(column)}
              </th>
            ))}
          </tr>
        </thead>

        {groups.map((group) => (
          <tbody key={group.leagueId}>
            <tr>
              {/* `scope="rowgroup"`: this header labels every row in its own
                  tbody, which is exactly what a league block is. */}
              <th
                scope="rowgroup"
                colSpan={BOARD_COLUMNS.length + 1}
                className="border-y border-rule bg-ground-0 px-3 py-2 text-left align-middle font-normal"
              >
                {/* Sticky within the horizontal scroller so the league name stays
                    on screen while the market columns scroll under the row. */}
                <span className="sticky left-0 inline-flex items-baseline gap-2">
                  {group.leagueSlug === null ? (
                    <span className="t-h3 font-display text-ink">{group.leagueName}</span>
                  ) : (
                    <Link
                      href={`/board/${group.leagueSlug}`}
                      className="t-h3 rounded-price font-display text-ink ui-transition hover:text-ink-2"
                    >
                      {group.leagueName}
                    </Link>
                  )}
                  {group.sportName === null ? null : (
                    <span className="t-label text-ink-muted">{group.sportName}</span>
                  )}
                </span>
              </th>
            </tr>

            {group.entries.map((entry) => (
              <BoardRow
                key={entry.event.id}
                entry={entry}
                catalogue={catalogue}
                bookFilter={bookFilter}
                timeZone={timeZone}
              />
            ))}
          </tbody>
        ))}
      </table>
    </div>
  );
}
