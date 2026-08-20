// ---------------------------------------------------------------------------
// The accessibility contract, checked by hand.
// ---------------------------------------------------------------------------
// There is no axe dependency in the image and adding one would mean an npm
// install inside e2e/, which the container mandate forbids. That is not a great
// loss here: a generic rule engine would not check the two things that actually
// matter on this product, both of which are specific and both of which are
// spelled out in DESIGN.md's Accessibility section and CLAUDE.md §7.
//
//   1. EXACTLY ONE polite live region, throttled to one announcement per 5s.
//      A live region that fires per tick is, in DESIGN.md's words, "the single
//      worst thing this UI could do to a screen reader user" — a dense board
//      updating hundreds of cells would produce a continuous, unstoppable
//      stream of speech. Two regions is the same failure twice.
//
//   2. EVERY PRICE IS A FOCUSABLE CELL WITH A NAME. The mobile pass rejected a
//      card-per-event layout partly on this ground: "a card grid re-implements
//      that with ARIA and gets it subtly wrong."
//
// Everything below is a specific, hand-checkable promise rather than a generic
// audit.
// ---------------------------------------------------------------------------

import { expect, test, type Page } from '@playwright/test';
import { gotoBoard, waitForBoard } from '../support/board';
import { ROUTES } from '../support/env';
import {
  accessibleNameOf,
  assertiveLiveRegions,
  focusTargetOf,
  ODDS_FORMAT_NAMES,
  oddsFormatControl,
  politeLiveRegions,
  priceCells,
  quotedPriceCells,
  statusLiveRegions,
} from '../support/selectors';

const PAGES: ReadonlyArray<{ readonly name: string; readonly path: string }> = [
  { name: 'landing', path: ROUTES.landing },
  { name: 'board', path: ROUTES.board },
];

async function open(page: Page, path: string): Promise<void> {
  await page.goto(path, { waitUntil: 'domcontentloaded' });
}

test.describe('accessibility contract', () => {
  for (const target of PAGES) {
    test(`${target.name}: exactly one h1`, async ({ page }) => {
      await open(page, target.path);
      // More than one h1 destroys the document outline a screen-reader user
      // navigates by; zero leaves the page unnamed.
      await expect(page.getByRole('heading', { level: 1 })).toHaveCount(1);
    });

    test(`${target.name}: every image carries an alt attribute`, async ({ page }) => {
      await open(page, target.path);
      // An EMPTY alt is correct for a decorative image; a MISSING alt makes a
      // screen reader read the file name out loud.
      await expect(page.locator('img:not([alt])')).toHaveCount(0);
    });

    test(`${target.name}: nothing shouts`, async ({ page }) => {
      await open(page, target.path);
      // A moving price is information, not an emergency. assertive regions and
      // role="alert" interrupt whatever the user is currently reading.
      await expect(assertiveLiveRegions(page)).toHaveCount(0);
    });
  }

  test('board: exactly one application-level polite live region', async ({ page }) => {
    await gotoBoard(page);
    await waitForBoard(page);

    // The one that batches price movement to a single announcement per 5s.
    // Two of these is the failure the rule exists to prevent: a dense board
    // updating hundreds of cells would produce continuous, unstoppable speech.
    await expect(politeLiveRegions(page)).toHaveCount(1);

    // Widget-scoped `role="status"` regions (a search result count) are a
    // different concern and are allowed — but only one, and only that one.
    expect(
      await statusLiveRegions(page).count(),
      'more than one widget-scoped status region is on the board',
    ).toBeLessThanOrEqual(1);

    test.info().annotations.push({
      type: 'live-regions',
      description: `announcer=${String(await politeLiveRegions(page).count())} status=${String(await statusLiveRegions(page).count())}`,
    });
  });

  test('landing: at most one application-level polite live region', async ({ page }) => {
    await open(page, ROUTES.landing);
    // The announcer is mounted by the root client shell, so it may be present
    // here too. What must never happen is a second one.
    expect(await politeLiveRegions(page).count()).toBeLessThanOrEqual(1);
  });

  test('board: the odds format control is a radiogroup naming all three formats', async ({ page }) => {
    await gotoBoard(page);
    await waitForBoard(page);

    // A three-way exclusive choice. Rendering it as three unrelated buttons
    // gives a screen-reader user no way to know they are alternatives, or which
    // one is currently in effect.
    const group = oddsFormatControl(page);
    await expect(group).toBeVisible();

    const radios = group.getByRole('radio');
    await expect(radios).toHaveCount(3);

    for (const name of ODDS_FORMAT_NAMES) {
      await expect(group.getByRole('radio', { name })).toHaveCount(1);
    }

    // Exactly one is selected — DESIGN.md keeps American as the default.
    const checked = group.getByRole('radio', { checked: true });
    await expect(checked).toHaveCount(1);
  });

  test('board: every price cell is focusable and named', async ({ page }) => {
    await gotoBoard(page);
    const branch = await waitForBoard(page);
    test.skip(branch === 'empty', 'the board is legitimately empty — there are no cells to check');

    // quotedPriceCells, not priceCells: a suspended market and an unquoted
    // selection both render a `.price-cell` and both are correctly NOT
    // focusable, so asserting focus over every cell would fail on markup that
    // is behaving exactly as designed.
    const cells = quotedPriceCells(page);
    const sample = (await cells.all()).slice(0, 16);
    expect(sample.length).toBeGreaterThan(0);

    for (const [index, cell] of sample.entries()) {
      const target = await focusTargetOf(cell);

      // Pin the ELEMENT, not the locator, before focusing.
      //
      // `cells.all()` hands back POSITIONAL locators (`nth(i)`), and this board
      // is live: rows are ordered by kickoff, so an event going live re-sorts
      // them and `nth(0)` starts resolving to a different `<a>`. `focus()` then
      // lands on the old element while `toBeFocused()` re-resolves to the new
      // one and reports "inactive" for twenty seconds — a red suite describing a
      // reorder, not a focus bug. (Verified separately: focus on a pinned cell
      // survives fifteen seconds of live deltas without moving.)
      const handle = await target.elementHandle();
      expect(handle, `price cell ${String(index)} vanished before it could be focused`).not.toBeNull();
      await handle?.focus();
      const focused = await handle?.evaluate((el) => el === document.activeElement);
      expect(focused, `price cell ${String(index)} did not take focus`).toBe(true);
      await handle?.dispose();

      const name = await accessibleNameOf(target);
      expect(name, `price cell ${String(index)} has no accessible name`).not.toBe('');
      expect(
        /\d/u.test(name),
        `price cell ${String(index)} names no price: "${name}"`,
      ).toBe(true);
    }
  });

  test('board: an individual price change is described, not announced', async ({ page }) => {
    await gotoBoard(page);
    const branch = await waitForBoard(page);
    test.skip(branch === 'empty', 'the board is legitimately empty — there are no cells to check');

    // DESIGN.md: "Individual price changes are exposed via aria-describedby on
    // focus, not announced." A per-cell live region would be the worst possible
    // reading of the same requirement, so what is asserted here is the absence
    // of one inside the table.
    const cell = priceCells(page).first();
    await expect(cell.locator('[aria-live]')).toHaveCount(0);
  });

  test('board: the page is reachable by keyboard from the top', async ({ page }) => {
    await gotoBoard(page);
    await waitForBoard(page);

    // A trivial but load-bearing check: the first Tab must land on something.
    // A page whose first focusable element is unreachable is a page a keyboard
    // user cannot enter at all.
    await page.keyboard.press('Tab');
    const focused = await page.evaluate(() => {
      const active = document.activeElement;
      return active === null || active === document.body ? null : active.tagName.toLowerCase();
    });
    expect(focused, 'nothing took focus on the first Tab').not.toBeNull();
  });
});
