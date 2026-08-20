// ---------------------------------------------------------------------------
// THE UI CONTRACT.
// ---------------------------------------------------------------------------
// Every dependency this suite has on the frontend lives in this one file, so a
// rename in web/ is a one-file fix here rather than a scavenger hunt through
// six specs.
//
// Priority order for every locator below:
//
//   1. An ARIA role plus an accessible name. DESIGN.md's accessibility section
//      and CLAUDE.md §7 already *require* these to exist, so depending on them
//      is depending on a promise the product has already made.
//   2. A `data-testid`, where no role is defined by the design (the board's
//      empty state, the price cell).
//   3. A design-system class from web/src/app/globals.css — `.price-cell`,
//      `.digit-roll-current`, `.status-pip`. These are NOT incidental CSS: they
//      are the documented hooks the delta rail and the status pip are built on,
//      and a board that does not carry them has no rail. They are the most
//      stable class names in the project.
//
// There is no brittle CSS chain anywhere in this suite. Where a class appears
// it is a published contract, and it is always ORed with a test id so either
// satisfies it.
// ---------------------------------------------------------------------------

import type { Locator, Page } from '@playwright/test';

// --- landing ---------------------------------------------------------------

/**
 * CLAUDE.md §0 / DESIGN.md "Non-negotiable content": the landing page states
 * that this is a simulation and not a licensed sportsbook. Asserted as body
 * copy rather than as a component so it survives any redesign — which is the
 * whole point of testing it.
 */
export const DISCLAIMER_SIMULATION = /simulation/iu;
export const DISCLAIMER_NOT_LICENSED = /not a licen[cs]ed sportsbook/iu;

export function disclaimer(page: Page): Locator {
  return page
    .getByTestId('legal-disclaimer')
    .or(page.getByText(DISCLAIMER_NOT_LICENSED))
    .first();
}

// --- board -----------------------------------------------------------------

/**
 * The board is a `<table>`. DESIGN.md's mobile pass is explicit that the table
 * survives at every breakpoint precisely so "every price is a table cell" stays
 * structurally true rather than re-implemented with ARIA.
 *
 * League grouping may produce one table per league, so this is plural.
 */
export function boardTables(page: Page): Locator {
  return page.getByRole('table');
}

export function boardTable(page: Page): Locator {
  return boardTables(page).first();
}

export function columnHeaders(page: Page): Locator {
  return page.getByRole('columnheader');
}

/**
 * Every price cell, in any of its three states. `.price-cell` is the globals.css
 * class that carries the delta rail (`::before`, `[data-direction]`,
 * `.rail-decaying`), so any cell able to show a price move already has it, and
 * `data-price-cell` marks which state it is in.
 *
 * Use this to count and to assert structure. For anything that assumes the cell
 * is ACTIONABLE — focus, an accessible name, a navigation — use
 * {@link quotedPriceCells} instead.
 */
export function priceCells(root: Page | Locator): Locator {
  return root.locator('[data-price-cell], [data-testid="price-cell"], .price-cell');
}

/**
 * Only the cells carrying a live, actionable quote.
 *
 * The board renders three states and only this one is interactive: a SUSPENDED
 * market keeps its price on screen struck through (blanking it would make the
 * board flicker on a suspension that is usually seconds long) and an UNQUOTED
 * selection renders an empty well, because "no book has quoted this" is a
 * correct answer rather than a missing one. Neither is focusable and neither
 * offers a name to act on — deliberately.
 *
 * So an assertion like "every price cell is focusable and named" is only true of
 * this subset. Selecting on `.price-cell` instead passes today purely because
 * the current feed happens to have no suspended market on screen; the first
 * mid-run suspension would fail it on a cell that is behaving correctly.
 */
export function quotedPriceCells(root: Page | Locator): Locator {
  return root.locator('[data-price-cell="quoted"]');
}

/**
 * The element inside a price cell that actually takes focus. DESIGN.md requires
 * every price cell to be keyboard focusable with a visible ring; whether that
 * is the cell itself (`tabindex`) or a control inside it (`<button>`) is an
 * implementation choice, and this resolves either.
 */
export async function focusTargetOf(cell: Locator): Promise<Locator> {
  const inner = cell.locator('button, a[href], [tabindex]:not([tabindex="-1"])');
  return (await inner.count()) > 0 ? inner.first() : cell;
}

/**
 * The COMPUTED accessible name of an element — what a screen reader actually
 * says.
 *
 * Reading `aria-label` alone is not the same thing: it misses
 * `aria-labelledby`, misses content-derived names, and would pass an element
 * that has no name at all. `ariaSnapshot()` runs Playwright's real name
 * computation, and only the FIRST line is considered so a descendant's name is
 * never mistaken for the element's own.
 */
export async function accessibleNameOf(target: Locator): Promise<string> {
  try {
    const snapshot = await target.ariaSnapshot();
    const firstLine = snapshot.split('\n', 1)[0] ?? '';
    const quoted = /"((?:[^"\\]|\\.)*)"/u.exec(firstLine);
    const name = quoted?.[1];
    if (name !== undefined) return name.replace(/\\(.)/gu, '$1').replace(/\s+/gu, ' ').trim();
  } catch {
    // Fall through to the attribute/text reading below.
  }
  const label = await target.getAttribute('aria-label');
  if (label !== null && label.trim() !== '') return label.replace(/\s+/gu, ' ').trim();
  return (await target.innerText()).replace(/\s+/gu, ' ').trim();
}

/** Cells currently mid-decay — the delta rail having fired. */
export function decayingRails(page: Page): Locator {
  return page.locator('.price-cell.rail-decaying, [data-testid="price-cell"].rail-decaying');
}

/**
 * The board's empty state. An empty board is CORRECT — the pipeline may
 * genuinely be holding no events in the requested window — and it must say so
 * explicitly rather than rendering a blank rectangle.
 */
export const EMPTY_BOARD_COPY = /no (upcoming |open |live )?(events|markets|games|odds)/iu;

export function boardEmptyState(page: Page): Locator {
  return page
    .getByTestId('board-empty')
    .or(page.getByRole('status').filter({ hasText: EMPTY_BOARD_COPY }))
    .or(page.getByText(EMPTY_BOARD_COPY))
    .first();
}

/** Links out of the board into event detail. */
export function eventLinks(page: Page): Locator {
  return boardTables(page).getByRole('link');
}

// --- header / chrome -------------------------------------------------------

/**
 * The stream status surface. At >= 768px this is DESIGN.md's persistent 24px
 * mono status rail; below it, the collapsed pip (`.status-pip[data-state]`).
 * Its text/accessible name is `describeStream(state).label` from
 * web/src/lib/ws/client.ts — "Live — streaming" / "Resyncing" / "Reconnecting"
 * / "Disconnected" / "Not connected".
 */
export const STREAM_STATE_LABELS =
  /live\s*[—–-]?\s*streaming|resyncing|reconnecting|disconnected|not connected/iu;
export const STREAM_LIVE_LABEL = /live\s*[—–-]?\s*streaming/iu;

/**
 * The status SURFACE — whichever of the two is on screen at this breakpoint.
 * `.filter({ visible: true })` matters: both exist in the DOM at all times and
 * CSS decides which one is shown, so an unfiltered `.first()` would resolve to
 * the hidden one and fail `toBeVisible()` for the wrong reason.
 */
export function streamStatus(page: Page): Locator {
  return page
    .getByTestId('stream-status')
    .or(page.locator('[aria-label="Stream status"]'))
    .or(page.locator('.status-pip'))
    .filter({ visible: true })
    .first();
}

/**
 * The rendered state WORD — `describeStream(state).label`. Separate from the
 * surface above because the surface's own name is a static landmark label
 * ("Stream status"), and asserting the connection reached a live state against
 * a static string would pass vacuously.
 */
export function streamStateLabel(page: Page): Locator {
  return page.getByText(STREAM_STATE_LABELS).filter({ visible: true }).first();
}

/**
 * DESIGN.md "Category conventions deliberately kept": American odds default
 * with a format toggle in the header. A three-way exclusive choice is a
 * radiogroup; `oddsFormatLabel()` supplies the three names.
 */
export function oddsFormatControl(page: Page): Locator {
  return page
    .getByRole('radiogroup', { name: /odds format/iu })
    .or(page.getByRole('radiogroup'))
    .first();
}

export const ODDS_FORMAT_NAMES = [/american/iu, /decimal/iu, /fractional/iu] as const;

/**
 * THE application announcer — the single throttled polite region that batches
 * price movement to one announcement per 5s. More than one of these means two
 * things are announcing price movement, which is the exact failure the rule
 * exists to prevent.
 *
 * `:not([role="status"])` is not a hedge, it is the distinction the frontend
 * itself draws: `role="status"` is an implicit live region in its own right, so
 * the price announcer deliberately does NOT carry it, while a search-results
 * count — a different concern, announced at a different moment — does. Counting
 * both together would conflate "the board is shouting twice" with "the search
 * box reports its result count", which are not the same defect.
 */
export function politeLiveRegions(page: Page): Locator {
  return page.locator('[aria-live="polite"]:not([role="status"])');
}

/** Polite regions belonging to a widget (search results and the like). */
export function statusLiveRegions(page: Page): Locator {
  return page.locator('[role="status"][aria-live="polite"]');
}

/**
 * Nothing this PRODUCT renders may shout. A moving price is not an alert.
 *
 * Next's App Router injects its own route announcer — `<next-route-announcer>`
 * wrapping `#__next-route-announcer__`, which is `role="alert"` and
 * `aria-live="assertive"` — and it is the standard, correct SPA answer to
 * "a client-side navigation replaced the page and a screen reader was told
 * nothing". It is empty at rest and speaks only the new page's title. Excluding
 * it keeps this assertion about the application's own markup, which is the
 * thing the rule is aimed at; asserting over it would only ever be satisfiable
 * by deleting a genuine accessibility feature of the framework.
 */
export function assertiveLiveRegions(page: Page): Locator {
  return page.locator(
    '[aria-live="assertive"]:not(#__next-route-announcer__ *):not(#__next-route-announcer__), ' +
      '[role="alert"]:not(#__next-route-announcer__ *):not(#__next-route-announcer__)',
  );
}

// --- auth chrome -----------------------------------------------------------

/**
 * Signed-in and signed-out are each detected by the control that only exists in
 * that state. This is the least brittle possible signal: a product cannot offer
 * a working sign-out without one, whatever it looks like.
 */
const SIGN_OUT = /sign out|log ?out/iu;
const SIGN_IN = /sign in|log ?in/iu;

/**
 * Sign-out is a `menuitem` inside the account dropdown today
 * (web/src/components/auth/account-menu.tsx), but a product may move it to a
 * plain button or link, so all three roles are accepted.
 */
export function signOutControl(page: Page): Locator {
  return page
    .getByTestId('sign-out')
    .or(page.getByRole('menuitem', { name: SIGN_OUT }))
    .or(page.getByRole('button', { name: SIGN_OUT }))
    .or(page.getByRole('link', { name: SIGN_OUT }))
    .first();
}

export function signInControl(page: Page): Locator {
  return page
    .getByTestId('sign-in')
    .or(page.getByRole('link', { name: SIGN_IN }))
    .or(page.getByRole('button', { name: SIGN_IN }))
    .first();
}

/**
 * The menu the sign-out control is folded into. The current trigger's
 * accessible name is "Account menu for {email}" — matched loosely so a copy
 * change does not break the critical path.
 */
export function accountMenu(page: Page): Locator {
  return page
    .getByTestId('account-menu')
    .or(page.getByRole('button', { name: /account|profile|signed in as/iu }))
    .first();
}

// --- auth forms ------------------------------------------------------------

export function emailField(page: Page): Locator {
  return page
    .getByTestId('email')
    .or(page.getByLabel(/e-?mail/iu))
    .or(page.getByRole('textbox', { name: /e-?mail/iu }))
    .first();
}

export function passwordField(page: Page): Locator {
  return page
    .getByTestId('password')
    .or(page.getByLabel(/^password$/iu))
    .or(page.locator('input[type="password"]'))
    .first();
}

export function submitControl(page: Page, name: RegExp): Locator {
  return page.getByRole('button', { name }).first();
}

export const REGISTER_SUBMIT = /create account|sign up|register/iu;
export const LOGIN_SUBMIT = /sign in|log ?in/iu;
export const REGISTER_LINK = /create account|sign up|register/iu;
