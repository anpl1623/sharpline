/**
 * The board's route-level loading state. Covers `/board` and `/board/{league}`.
 *
 * It is the board's GEOMETRY and nothing else — no numbers, no names, no
 * plausible-looking placeholder prices. A skeleton that showed something a
 * viewer could read as data would be the worst possible loading state on a
 * surface whose entire claim is that its numbers are real.
 *
 * The blocks do not pulse. DESIGN.md spends the whole motion budget on the delta
 * rail, and the reason a 2px rule is the loudest thing in the viewport is that
 * nothing else on screen moves; a grid of pulsing rectangles handing over to a
 * live board would compete with the only motion that carries information.
 * "Loading" is still distinguishable from "empty" because an empty board renders
 * written prose, and because this container carries `aria-busy`.
 */

import { Skeleton } from '@/components/ui/skeleton';

/** Enough rows to occupy a first viewport. A shape, not a quantity of data. */
const ROW_COUNT = 10;

const ROWS = Array.from({ length: ROW_COUNT }, (_, index) => index);

const MARKET_COLUMNS = [0, 1, 2];

export default function BoardLoading() {
  return (
    <section
      aria-busy="true"
      aria-label="Loading the board"
      className="flex w-full flex-col"
    >
      <header className="flex flex-col gap-2 px-4 pb-3 pt-6">
        <Skeleton className="h-6 w-40" />
        <Skeleton className="h-4 w-72 max-w-full" />
      </header>

      <div className="flex flex-wrap items-center gap-1 border-b border-rule px-4 py-3">
        <Skeleton className="h-9 w-24" />
        <Skeleton className="h-9 w-24" />
        <Skeleton className="h-9 w-24" />
        <Skeleton className="h-9 w-24" />
      </div>

      <div className="board-scroll w-full">
        <div className="w-full min-w-[496px]">
          {ROWS.map((row) => (
            <div
              key={row}
              className="board-row flex items-center gap-1 border-b border-rule bg-ground-1 px-1"
            >
              <div className="board-game-cell flex min-w-[132px] flex-1 flex-col justify-center gap-1 px-2">
                <Skeleton className="h-3 w-28 max-w-full" />
                <Skeleton className="h-2 w-20 max-w-full" />
              </div>
              {MARKET_COLUMNS.map((column) => (
                <div
                  key={column}
                  className="board-market-col flex w-[140px] shrink-0 flex-col justify-center gap-[2px] px-0.5"
                >
                  <Skeleton className="h-[15px] w-full" />
                  <Skeleton className="h-[15px] w-full" />
                </div>
              ))}
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}
