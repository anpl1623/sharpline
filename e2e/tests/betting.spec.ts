/**
 * The critical path CLAUDE.md §10 names by hand:
 *
 *   "sign in → browse board → build parlay → place → observe settlement"
 *
 * Every other spec in this suite reads. This one WRITES — it is the only place
 * the e2e tier proves that a price on the board and a row in the ledger are the
 * same fact, and it is deliberately end-to-end rather than a component test: the
 * slip's stake field, the placement request, the double-entry write and the
 * history page each work in isolation in their own tier, and the failure this
 * catches is the one where they disagree with each other.
 *
 * # Why funding goes through the API and the bet does not
 *
 * There is no UI that credits an account, and there should not be: `POST
 * /account/grant` exists because play money has to enter the system somehow
 * (see its own note in openapi.yaml), not because a customer is meant to press
 * a button and mint some. So the grant is issued over HTTP with a token this
 * spec logs in for, and everything a customer actually does — choosing a price,
 * setting a stake, placing — goes through the browser, which is the whole point
 * of the tier.
 *
 * # Why settlement is asserted at the ledger and not on the clock
 *
 * A synthetic contest finishes when the results poller says so, on the interval
 * config gives it, against a slate the provider generated for real wall-clock
 * times. Waiting for one inside a Playwright budget would make this spec a
 * timing bet that fails on a slow machine and passes on a fast one. What is
 * asserted instead is the half of settlement that IS deterministic at placement
 * time: the stake leaves cash, arrives in escrow, and the two still sum to what
 * was granted. Grading itself is covered where it can be driven rather than
 * awaited — internal/settlement's own tests and test/integration.
 */

import { expect, test, type APIRequestContext, type Page } from '@playwright/test';
import { gotoBoard, waitForBoard } from '../support/board';
import { BASE_URL } from '../support/env';
import { registerNewAccount, type Credentials } from '../support/auth';
import { quotedPriceCells } from '../support/selectors';

/** Minor units granted before each placement test. Comfortably above any stake below. */
const GRANT_MINOR = 500_000;

/** The stake typed into the slip, in MAJOR units, as a customer would type it. */
const STAKE_MAJOR = '20.00';
const STAKE_MINOR = 2_000;

/** How many different prices a placement test will try before giving up. */
const PLACEMENT_ATTEMPTS = 6;

interface Balances {
  cashMinor: number;
  escrowMinor: number;
  totalMinor: number;
}

/**
 * Log in over HTTP and return the access token.
 *
 * The browser is already signed in by this point; this is a SECOND session for
 * the same credentials, opened only so the grant can be authorised. Reaching
 * into the app's own token store instead would couple this spec to where that
 * store keeps it, which is a refactor away from breaking.
 */
async function apiToken(request: APIRequestContext, credentials: Credentials): Promise<string> {
  const res = await request.post(`${BASE_URL}/api/v1/auth/login`, {
    data: { email: credentials.email, password: credentials.password },
  });
  expect(res.status(), 'log in for an API token').toBe(200);

  const body = (await res.json()) as { access_token?: string };
  const token = body.access_token;
  expect(token, 'the login response carries an access token').toBeTruthy();
  return token as string;
}

/** Credit the account so a stake has somewhere to come from. */
async function grant(request: APIRequestContext, token: string, amountMinor: number): Promise<void> {
  const res = await request.post(`${BASE_URL}/api/v1/account/grant`, {
    headers: {
      authorization: `Bearer ${token}`,
      // Required, and deliberately unique per call: the identifier is derived
      // from (user, key), so a reused key would replay the first grant rather
      // than crediting again.
      'idempotency-key': `e2e-grant-${Date.now()}-${Math.random().toString(36).slice(2)}`,
    },
    data: { amount_minor: amountMinor },
  });
  expect(res.status(), 'grant play money').toBe(201);
}

/** Read the derived balances. There is no balance column anywhere; this is a fold. */
async function balances(request: APIRequestContext, token: string): Promise<Balances> {
  const res = await request.get(`${BASE_URL}/api/v1/account/balance`, {
    headers: { authorization: `Bearer ${token}` },
  });
  expect(res.status(), 'read the account balance').toBe(200);

  const body = (await res.json()) as {
    cash: { balance_minor: number };
    escrow: { balance_minor: number };
    total_minor: number;
  };
  return {
    cashMinor: body.cash.balance_minor,
    escrowMinor: body.escrow.balance_minor,
    totalMinor: body.total_minor,
  };
}

/** The slip, whichever way the viewport renders it. */
function slip(page: Page) {
  return page.getByRole('complementary', { name: /bet slip/iu }).or(page.getByRole('dialog', { name: /bet slip/iu })).first();
}

/**
 * Put `count` prices on the slip, and report how many actually went on.
 *
 * A price cell is only bettable when it carries a book, so the count returned
 * may be lower than the count asked for — the board is fed by a stochastic
 * provider and this spec does not get to assume a full slate. Callers skip
 * rather than fail on a short count: an empty board is a legitimate state the
 * board spec already asserts, and turning it into a betting failure would make
 * this spec flaky for a reason that has nothing to do with betting.
 */
async function addToSlip(page: Page, count: number): Promise<number> {
  const cells = quotedPriceCells(page);
  const available = await cells.count();
  let added = 0;

  for (let i = 0; i < available && added < count; i += 1) {
    const cell = cells.nth(i);
    if (await cell.isDisabled()) continue;

    // Already on the slip from an earlier call — count it and move on. Clicking
    // it again would TOGGLE it off, which is how this helper used to remove the
    // leg it had just been asked to add.
    if ((await cell.getAttribute('aria-pressed')) === 'true') {
      added += 1;
      continue;
    }

    await cell.scrollIntoViewIfNeeded();
    await cell.click();

    // `aria-pressed` is the cell's own report that it is on the slip, and it is
    // what a screen-reader user hears; asserting it rather than a count in the
    // panel keeps this helper honest about the affordance under test.
    await expect(cell).toHaveAttribute('aria-pressed', 'true');
    added += 1;
  }

  return added;
}

/** Type a stake into whichever stake field the slip is showing. */
async function setStake(page: Page, major: string): Promise<void> {
  const field = slip(page).getByLabel(/^stake( per ticket)?$/iu).first();
  await field.fill(major);
  // The field commits on change; the Place control reads the committed value.
  await expect(field).toHaveValue(major);
}

function placeButton(page: Page) {
  return slip(page).getByRole('button', { name: /place bet/iu });
}

/**
 * Choose the ticket kind.
 *
 * The slip does not infer it from the leg count, and that is deliberate rather
 * than an omission: two legs are a parlay OR a round robin OR (on the right
 * market types) a teaser, and guessing would silently price one as another. So
 * a one-leg slip opens on Parlay reporting "a parlay needs at least two
 * selections" until a kind is chosen, and this helper is what a customer's
 * click on that control is.
 */
async function chooseKind(page: Page, label: RegExp): Promise<void> {
  const control = slip(page).getByRole('group', { name: /ticket kind/iu });
  const button = control.getByRole('button', { name: label });
  await expect(button, 'the ticket kind is offered for this slip').toBeEnabled();
  await button.click();
  await expect(button).toHaveAttribute('aria-pressed', 'true');
}

/**
 * Read one of the slip's summary amounts, in MAJOR units.
 *
 * The summary is a description list, so the amount is the definition beside the
 * term — addressed by its term rather than by position, because the rows the
 * slip shows depend on the ticket kind.
 */
async function readMoney(panel: ReturnType<typeof slip>, term: RegExp): Promise<number> {
  const value = await panel.getByRole('term').filter({ hasText: term }).first()
    .locator('xpath=following-sibling::dd[1]').innerText();

  // "2,588.00 play" -> 2588.00. The FIRST amount in the cell, not every digit
  // in it: the definition carries the unit beside the number and, on some rows,
  // a second line under it, so stripping all non-numerics would splice two
  // amounts into one and assert against a number nothing displays.
  const match = /-?\d[\d,]*\.\d{2}/u.exec(value);
  expect(match, `"${value}" contains an amount`).not.toBeNull();
  const parsed = Number((match as RegExpExecArray)[0].replace(/,/gu, ''));
  expect(Number.isFinite(parsed), `"${value}" is a number`).toBe(true);
  return parsed;
}

/**
 * Try to place a straight, moving on to another price when the book declines.
 *
 * Returns whether anything was placed. A decline is not a failure here — it is
 * the book refusing to lay a stale quote, which is correct — so this walks the
 * board the way a customer would, and reports honestly when nothing on it was
 * layable.
 */
async function attemptPlacement(page: Page): Promise<boolean> {
  const cells = quotedPriceCells(page);
  const available = Math.min(await cells.count(), PLACEMENT_ATTEMPTS);

  for (let i = 0; i < available; i += 1) {
    const cell = cells.nth(i);
    if (await cell.isDisabled()) continue;

    await cell.scrollIntoViewIfNeeded();
    await cell.click();
    if ((await cell.getAttribute('aria-pressed')) !== 'true') continue;

    await chooseKind(page, /^straight$/iu);
    await setStake(page, STAKE_MAJOR);

    const place = placeButton(page);
    if (!(await place.isEnabled())) {
      await cell.click();
      continue;
    }
    await place.click();

    const panel = slip(page);
    const placed = panel.getByText(/bet placed/iu).first();
    const refused = panel.getByRole('alert').first();
    await expect(placed.or(refused), 'the slip answers the placement').toBeVisible();

    if (await placed.isVisible()) return true;

    // Refused. Take this selection back off the slip and try another price.
    if ((await cell.getAttribute('aria-pressed')) === 'true') await cell.click();
  }

  return false;
}

test.describe('betting', () => {
  test('the slip prices a ticket from the legs on it', async ({ page }) => {
    // Signed in but UNFUNDED. The arithmetic below is the slip's own and needs
    // no money, but the panel withholds its summary from a signed-out visitor
    // ("The slip is kept while you sign in"), so there is a session and no grant.
    // This is the one part of placement that does not depend on the pipeline
    // being fresh enough to lay a bet.
    await registerNewAccount(page);
    await gotoBoard(page);
    await waitForBoard(page);

    const added = await addToSlip(page, 1);
    test.skip(added < 1, 'no bettable price on the board — nothing to price');

    await chooseKind(page, /^straight$/iu);
    await setStake(page, STAKE_MAJOR);

    const panel = slip(page);
    await expect(panel.getByText(/ticket price/iu)).toBeVisible();

    // To return = stake x ticket price, and Profit = that less the stake. The
    // relationship is asserted rather than a literal, because the price is
    // whatever the live board is showing.
    const toReturn = await readMoney(panel, /to return/iu);
    const profit = await readMoney(panel, /^profit$/iu);
    expect(
      Math.round((toReturn - profit) * 100),
      'profit is the return less the stake',
    ).toBe(Math.round(Number(STAKE_MAJOR) * 100));
  });

  test('the ticket kind is gated by what the legs can actually be', async ({ page }) => {
    await registerNewAccount(page);
    await gotoBoard(page);
    await waitForBoard(page);

    const added = await addToSlip(page, 1);
    test.skip(added < 1, 'no bettable price on the board');

    const kinds = slip(page).getByRole('group', { name: /ticket kind/iu });

    // One leg is a straight and cannot be anything else. The multis are
    // DISABLED rather than hidden, and each says why — a control that vanishes
    // teaches nothing about how to reach it.
    await expect(kinds.getByRole('button', { name: /^straight$/iu })).toBeEnabled();
    await expect(kinds.getByRole('button', { name: /^parlay$/iu })).toBeDisabled();
    await expect(slip(page).getByText(/a parlay needs at least two selections/iu)).toBeVisible();

    const second = await addToSlip(page, 2);
    test.skip(second < 2, 'only one bettable price on the board — no parlay to form');

    await expect(kinds.getByRole('button', { name: /^parlay$/iu })).toBeEnabled();
  });

  test('an unfunded account cannot place a bet', async ({ page }) => {
    // No grant. The slip refuses on the client rather than sending a placement
    // the ledger would have to reject — the balance it needs is already on
    // screen in the stake field.
    await registerNewAccount(page);
    await gotoBoard(page);
    await waitForBoard(page);

    const added = await addToSlip(page, 1);
    test.skip(added < 1, 'no bettable price on the board — nothing to place');

    await chooseKind(page, /^straight$/iu);
    await setStake(page, STAKE_MAJOR);
    await expect(
      placeButton(page),
      'a stake larger than the balance is not placeable',
    ).toBeDisabled();
  });

  test('a funded placement moves the stake from cash into escrow', async ({ page, request }) => {
    const credentials = await registerNewAccount(page);
    const token = await apiToken(request, credentials);
    await grant(request, token, GRANT_MINOR);

    const opening = await balances(request, token);
    expect(opening.cashMinor, 'the grant landed in cash').toBe(GRANT_MINOR);
    expect(opening.escrowMinor, 'nothing is in escrow before a bet').toBe(0);

    await gotoBoard(page);
    await waitForBoard(page);

    const placed = await attemptPlacement(page);

    // WHY THIS SKIPS RATHER THAN FAILS. A placement needs a quote inside
    // betting.DefaultMaxQuoteAge (3 minutes). Change detection means
    // `observed_at` is when a line last MOVED, not when the book last confirmed
    // it, so a quiet market's freshest row ages out and the book will not lay
    // it — placement.go records this as a known limit against the wrong clock.
    // On a board fed by a stochastic generator, whether ANY market is inside
    // that window at the instant this test clicks is not something the test
    // controls. Failing on it would make this spec a report on the generator's
    // mood; skipping says plainly that the path was not exercised.
    test.skip(!placed, 'no market on the board had a quote inside MaxQuoteAge — nothing was layable');

    const after = await balances(request, token);
    expect(after.escrowMinor, 'the stake is held in escrow').toBe(STAKE_MINOR);
    expect(after.cashMinor, 'the stake left cash').toBe(GRANT_MINOR - STAKE_MINOR);
    expect(
      after.totalMinor,
      'placing a bet moves money between accounts and creates none — the total is conserved',
    ).toBe(opening.totalMinor);

    // The ticket is the customer's record of it, and it is reachable.
    await page.goto('/bets');
    await expect(page.getByText(/placed/iu).first()).toBeVisible();
  });
});
