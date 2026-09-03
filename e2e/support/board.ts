// ---------------------------------------------------------------------------
// The board's two legal states.
// ---------------------------------------------------------------------------
// An empty board is CORRECT. The synthetic provider may genuinely be holding no
// events inside the requested window, and a board that says so explicitly has
// done its job. What is NOT correct is a third state: no rows and no empty
// state — a blank rectangle where the user cannot tell whether the system is
// broken, loading, or simply quiet.
//
// So the board is asserted as an exclusive disjunction, and `waitForBoard`
// failing to resolve to one of the two branches IS the defect.
// ---------------------------------------------------------------------------

import { expect, type Page } from '@playwright/test';
import { BOARD_READY_MS, ROUTES } from './env';
import { boardEmptyState, boardTables, priceCells } from './selectors';

export type BoardBranch = 'populated' | 'empty';

export async function gotoBoard(page: Page): Promise<void> {
  await page.goto(ROUTES.board, { waitUntil: 'domcontentloaded' });
}

/**
 * Populated means a price cell is VISIBLE, not merely present in the DOM.
 *
 * The distinction is load-bearing and was found by the phase-9 gate. Next's
 * streaming SSR delivers the board's content inside a `<div hidden
 * style="display:none">` and reveals it with an inline script a beat later, so
 * for roughly the first 200ms after `domcontentloaded` the table, its forty rows
 * and its 225 price cells are all in the document and none of them is visible.
 *
 * `Locator.count()` on a CSS selector is visibility-blind, so the old check
 * resolved this branch to 'populated' inside that window — and every assertion
 * downstream is visibility-AWARE: `getByRole('row')` skips hidden elements and
 * `focus()` does nothing to a `display:none` element. The result was three tests
 * that failed against a board a human sees render perfectly.
 *
 * Requiring visibility is STRICTER than requiring presence, not weaker: a board
 * that rendered its rows and never revealed them would now be caught here rather
 * than misreported as a missing-rows defect three assertions later.
 */
async function isPopulated(page: Page): Promise<boolean> {
  return await priceCells(page)
    .first()
    .isVisible()
    .catch(() => false);
}

async function isExplicitlyEmpty(page: Page): Promise<boolean> {
  return await boardEmptyState(page)
    .isVisible()
    .catch(() => false);
}

/**
 * Wait until the board has resolved to exactly one of its two legal states, and
 * report which. Throws — loudly, with what it actually saw — if it resolves to
 * neither or to both.
 */
export async function waitForBoard(page: Page): Promise<BoardBranch> {
  await expect
    .poll(
      async () => {
        if (await isPopulated(page)) return 'populated';
        if (await isExplicitlyEmpty(page)) return 'empty';
        return 'undecided';
      },
      {
        timeout: BOARD_READY_MS,
        message:
          'the board resolved to neither prices nor an explicit empty state. ' +
          'A blank board is the defect this assertion exists to catch: an empty ' +
          'board must SAY it is empty.',
      },
    )
    .not.toBe('undecided');

  const populated = await isPopulated(page);
  const empty = await isExplicitlyEmpty(page);

  expect(
    populated !== empty,
    populated
      ? 'the board rendered prices AND an empty state at the same time'
      : 'the board rendered neither prices nor an empty state',
  ).toBe(true);

  return populated ? 'populated' : 'empty';
}

/**
 * Data rows, excluding header rows. Used to prove the empty branch is
 * internally consistent — an empty state above a table full of rows would be a
 * worse bug than either state alone.
 */
export async function dataRowCount(page: Page): Promise<number> {
  const tables = await boardTables(page).all();
  let total = 0;
  for (const table of tables) {
    const rows = await table.getByRole('row').all();
    for (const row of rows) {
      // A header row contains columnheaders and no cells.
      const cells = await row.getByRole('cell').count();
      if (cells > 0) total += 1;
    }
  }
  return total;
}
