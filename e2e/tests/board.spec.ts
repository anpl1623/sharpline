// ---------------------------------------------------------------------------
// The odds board.
// ---------------------------------------------------------------------------
// Every number asserted here travelled provider -> ingest -> Kafka -> pricer ->
// Postgres -> api -> browser. Nothing in this suite knows the name of a team, a
// league or a book, and nothing may learn one: the assertions are about SHAPE
// and STRUCTURE, which is what can be true of a live system whose contents
// change between two reads.
//
// The headline assertion is an exclusive disjunction — populated XOR explicitly
// empty — and then the internal consistency of whichever branch was taken.
// ---------------------------------------------------------------------------

import { expect, test } from '@playwright/test';
import { dataRowCount, gotoBoard, waitForBoard } from '../support/board';
import { containsRenderedPrice, priceFormatsIn, priceTextOf, readPriceTexts } from '../support/odds';
import {
  accessibleNameOf,
  boardEmptyState,
  boardTable,
  columnHeaders,
  focusTargetOf,
  priceCells,
  quotedPriceCells,
} from '../support/selectors';

test.describe('odds board', () => {
  test.beforeEach(async ({ page }) => {
    await gotoBoard(page);
  });

  test('renders either populated rows or an explicit empty state, never neither', async ({ page }) => {
    const branch = await waitForBoard(page);
    test.info().annotations.push({ type: 'board', description: `resolved to: ${branch}` });

    if (branch === 'empty') {
      // Internal consistency of the empty branch: the empty state must carry
      // real copy, and there must be no data rows hiding under it.
      const empty = boardEmptyState(page);
      await expect(empty).toBeVisible();
      const copy = (await empty.innerText()).trim();
      expect(copy.length, 'the empty state must explain itself, not render a blank box').toBeGreaterThan(0);

      expect(await dataRowCount(page), 'an empty board must have no data rows').toBe(0);
      await expect(priceCells(page)).toHaveCount(0);
      return;
    }

    // Internal consistency of the populated branch.
    expect(await dataRowCount(page), 'a populated board must have data rows').toBeGreaterThan(0);
    await expect(boardEmptyState(page)).toBeHidden();

    const cells = priceCells(page);
    const count = await cells.count();
    expect(count, 'a populated board must render price cells').toBeGreaterThan(0);

    // Every sampled cell must render something; at least one must be a real,
    // parseable price. The distinction matters — a market may legitimately have
    // a selection with no current quote, which format.ts renders as NO_PRICE.
    const sample = await readPriceTexts(cells, Math.min(count, 24));
    for (const [index, text] of sample.entries()) {
      expect(text, `price cell ${String(index)} rendered no text at all`).not.toBe('');
    }

    const parseable = sample.filter((text) => containsRenderedPrice(text));
    expect(
      parseable.length,
      `no cell on the board parsed as odds in any display format. Sample: ${JSON.stringify(sample)}`,
    ).toBeGreaterThan(0);

    // Diagnostic only, deliberately NOT an assertion. A spread or total cell
    // legitimately carries its line beside its price ("O 54.5" / "-110"), and a
    // line is indistinguishable from a decimal price to a token-wise parser —
    // so "the board shows exactly one odds format" cannot be asserted from the
    // rendered text without false positives. The format toggle itself is
    // covered by role in a11y.spec.ts.
    const formats = new Set(parseable.flatMap((text) => priceFormatsIn(text)));
    test.info().annotations.push({ type: 'odds-formats-seen', description: [...formats].join(', ') });
  });

  test('carries the table semantics the accessibility contract promises', async ({ page }) => {
    const branch = await waitForBoard(page);
    test.skip(branch === 'empty', 'the board is legitimately empty — no cells to inspect');

    // DESIGN.md's mobile pass keeps the <table> at every breakpoint precisely so
    // that "every price is a table cell" stays structurally true rather than
    // being re-implemented with ARIA on a card grid.
    await expect(boardTable(page)).toBeVisible();

    const headers = columnHeaders(page);
    const headerCount = await headers.count();
    expect(headerCount, 'the board must have column headers').toBeGreaterThan(0);

    for (const header of (await headers.all()).slice(0, 12)) {
      await expect(header).not.toHaveText('');
    }

    // Every price cell has an accessible name that names the market, the
    // selection and the price. A name is checked for a digit (the price/line)
    // and for words (the market and selection) rather than for an exact string,
    // because the copy is the frontend's to choose.
    // quotedPriceCells: a suspended or unquoted cell is deliberately not
    // actionable and carries no name to act on, so only the quoted ones are in
    // scope for this contract.
    const cells = quotedPriceCells(page);
    const sample = (await cells.all()).slice(0, 12);
    expect(sample.length).toBeGreaterThan(0);

    for (const [index, cell] of sample.entries()) {
      const target = await focusTargetOf(cell);
      const name = await accessibleNameOf(target);

      expect(name, `price cell ${String(index)} has an empty accessible name`).not.toBe('');
      expect(
        /\d/u.test(name),
        `price cell ${String(index)} accessible name names no price: "${name}"`,
      ).toBe(true);
      expect(
        /\p{L}{3,}/u.test(name),
        `price cell ${String(index)} accessible name names no market or selection: "${name}". ` +
          'DESIGN.md: "Every price is a table cell with an accessible name (market, selection, price)" — ' +
          'a name that is only a numeral tells a screen-reader user which number, but not what it is for.',
      ).toBe(true);
    }
  });

  test('price cells are reachable from the keyboard', async ({ page }) => {
    const branch = await waitForBoard(page);
    test.skip(branch === 'empty', 'the board is legitimately empty — no cells to focus');

    const first = quotedPriceCells(page).first();
    const target = await focusTargetOf(first);

    // Pinned to an element handle for the same reason as a11y.spec.ts: `.first()`
    // is positional and this board re-sorts as events go live, so focusing
    // through a locator and asserting through the same locator can compare two
    // different elements.
    const handle = await target.elementHandle();
    expect(handle, 'the first price cell vanished before it could be focused').not.toBeNull();
    await handle?.focus();
    const focused = await handle?.evaluate((el) => el === document.activeElement);
    expect(focused, 'the first price cell did not take focus').toBe(true);
    await handle?.dispose();

    // Focus must not destroy the value: the cell still reads as a price after
    // being focused (a focus handler that swaps content would be a defect).
    const text = await priceTextOf(first);
    expect(text).not.toBe('');
  });
});
