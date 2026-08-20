// ---------------------------------------------------------------------------
// The landing page, and the one sentence that must never fall off it.
// ---------------------------------------------------------------------------
// CLAUDE.md §0: "No real money moves. […] This distinction must be stated in the
// README and on the landing page — an unlicensed real-money book is a legal
// liability on a resume; a rigorous simulation of one is an engineering
// credential." DESIGN.md repeats it under Non-negotiable content: the
// disclaimer "survives every redesign".
//
// A rule that nothing checks is a rule that a redesign quietly deletes. This
// spec is what makes "survives every redesign" a mechanical fact.
// ---------------------------------------------------------------------------

import { expect, test } from '@playwright/test';
import { ROUTES } from '../support/env';
import { DISCLAIMER_NOT_LICENSED, DISCLAIMER_SIMULATION, disclaimer } from '../support/selectors';

test.describe('landing page', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto(ROUTES.landing, { waitUntil: 'domcontentloaded' });
  });

  test('renders with a single, named heading', async ({ page }) => {
    const headings = page.getByRole('heading', { level: 1 });
    await expect(headings).toHaveCount(1);

    const heading = headings.first();
    await expect(heading).toBeVisible();
    await expect(heading).not.toHaveText('');

    // A document title is the first thing a browser tab, a bookmark and a
    // screen reader announce.
    await expect(page).toHaveTitle(/\S/u);
  });

  test('states that this is a simulation, not a licensed sportsbook', async ({ page }) => {
    const body = page.locator('body');

    // Asserted as body copy rather than against a component, deliberately: the
    // requirement is that the STATEMENT is on the page, not that a particular
    // element renders it. Any redesign that keeps the sentence passes; one that
    // drops it fails, which is the entire point.
    await expect(body).toContainText(DISCLAIMER_SIMULATION);
    await expect(body).toContainText(DISCLAIMER_NOT_LICENSED);

    // …and it must be visible, not merely present in the DOM.
    await expect(disclaimer(page)).toBeVisible();
  });

  test('offers a way into the board', async ({ page }) => {
    // The landing page's job is to hand a visitor to the product. Matched on
    // destination rather than on link text so copy can change freely.
    const toBoard = page.locator(`a[href="${ROUTES.board}"], a[href$="${ROUTES.board}"]`).first();
    await expect(toBoard).toBeVisible();
  });
});
