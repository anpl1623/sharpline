// ---------------------------------------------------------------------------
// Board -> event detail.
// ---------------------------------------------------------------------------
// The event's identity is carried FROM the board rather than asserted against a
// literal: the row's own text is read, then looked for on the detail page. That
// is the only way to assert "the market tree renders with the event's real
// name" without knowing a single team name — and this suite must not know one,
// because knowing one would mean it had been hardcoded somewhere.
//
// Skips cleanly when the board is empty. An empty board is a legal state of a
// live system, not a test failure.
// ---------------------------------------------------------------------------

import { expect, test } from '@playwright/test';
import { gotoBoard, waitForBoard } from '../support/board';
import { ROUTES } from '../support/env';
import { containsRenderedPrice, normaliseText, readPriceTexts } from '../support/odds';
import { credentialInUrl } from '../support/security';
import { columnHeaders, eventLinks, priceCells } from '../support/selectors';

/** Words long enough to identify a competitor, from a board row's own text. */
function identifyingWords(text: string): string[] {
  return normaliseText(text)
    .split(/[^\p{L}\p{N}]+/u)
    .filter((word) => word.length >= 4 && /\p{L}/u.test(word));
}

test.describe('event detail', () => {
  test('follows an event from the board and renders its market tree', async ({ page }) => {
    await gotoBoard(page);
    const branch = await waitForBoard(page);
    test.skip(branch === 'empty', 'the board is legitimately empty — there is no event to follow');

    const link = eventLinks(page).first();
    await expect(link, 'a populated board row must link to its event').toBeVisible();

    const linkText = normaliseText(await link.innerText());
    const words = identifyingWords(linkText);
    expect(
      words.length,
      `the board's event link names nothing identifiable: "${linkText}"`,
    ).toBeGreaterThan(0);

    await link.click();
    await page.waitForURL((url) => url.pathname !== ROUTES.board, { timeout: 30_000 });

    // --- the event is named ------------------------------------------------
    const heading = page.getByRole('heading', { level: 1 });
    await expect(heading).toHaveCount(1);
    await expect(heading.first()).toBeVisible();

    const pageText = normaliseText(await page.locator('body').innerText());
    const carried = words.filter((word) => pageText.toLowerCase().includes(word.toLowerCase()));
    expect(
      carried.length,
      `none of the competitors named on the board row appear on the event page. ` +
        `Row said: "${linkText}". This is the join between the board and the detail view.`,
    ).toBeGreaterThan(0);

    // --- the market tree ---------------------------------------------------
    // At least one market, with at least one real price in it. The event may
    // carry a suspended market with no quote, so the assertion is over the page
    // rather than over every cell.
    const cells = priceCells(page);
    await expect
      .poll(() => cells.count(), {
        timeout: 30_000,
        message: 'the event detail page rendered no price cells — the market tree is missing',
      })
      .toBeGreaterThan(0);

    const sample = await readPriceTexts(cells, Math.min(await cells.count(), 16));
    expect(
      sample.some((text) => containsRenderedPrice(text)),
      `no price on the event page parsed as odds. Sample: ${JSON.stringify(sample)}`,
    ).toBe(true);

    // The detail view keeps the table semantics the board promises, so a price
    // is still a cell with a header above it.
    expect(await columnHeaders(page).count(), 'the market tree must label its columns').toBeGreaterThan(0);
  });

  test('the board links to a stable, credential-free event URL', async ({ page }) => {
    await gotoBoard(page);
    const branch = await waitForBoard(page);
    test.skip(branch === 'empty', 'the board is legitimately empty — there is no event link to inspect');

    const href = await eventLinks(page).first().getAttribute('href');
    expect(href, 'the event link must be a real href, not a click handler').not.toBeNull();
    expect(href ?? '', 'an event link must be an in-app path').toMatch(/^\//u);

    const absolute = new URL(href ?? '/', page.url()).toString();
    expect(credentialInUrl(absolute), `the event link carries a credential: ${absolute}`).toBeNull();
  });
});
