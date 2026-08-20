// ---------------------------------------------------------------------------
// THE CRITICAL PATH — sign in, browse the board.
// ---------------------------------------------------------------------------
// CLAUDE.md §10 names the phase 7 critical path as "sign in -> browse board".
// Wagering, the slip and settlement are phase 8 and appear nowhere in this
// suite, deliberately.
//
// The account is REAL: registered against the live API on every run, against a
// unique address, because there is no seeding and no fixture user and
// registering twice is a 409. That is the only way this test proves the signed-
// in board is genuinely reachable rather than a UI state someone faked.
//
// It also enforces a security property, in the only place it can be enforced
// end to end: internal/wsgw D5 — a bearer token travels in the
// `sharpline.bearer.` SUBPROTOCOL and NEVER in a URL, because a URL lands in
// proxy access logs, Referer headers and browser history, none of which a token
// can be revoked from.
// ---------------------------------------------------------------------------

import { expect, test } from '@playwright/test';
import { expectSignedOut, registerNewAccount, signOut } from '../support/auth';
import { gotoBoard, waitForBoard } from '../support/board';
import { ROUTES, STREAM_CONNECT_MS } from '../support/env';
import { attachUrlGuard, credentialInUrl } from '../support/security';
import { priceCells } from '../support/selectors';
import { StreamRecorder } from '../support/stream';

test.describe('critical path', () => {
  test('register, browse the board signed in, then sign out', async ({ page }) => {
    const guard = attachUrlGuard(page);
    const stream = StreamRecorder.attach(page);

    // --- 1. a brand-new account -------------------------------------------
    const credentials = await registerNewAccount(page);
    test.info().annotations.push({ type: 'account', description: credentials.email });
    expect(credentialInUrl(page.url()), `after registration: ${page.url()}`).toBeNull();

    // --- 2. the board is browsable while authenticated --------------------
    await gotoBoard(page);
    expect(page.url(), 'the board URL must not carry a credential').toContain(ROUTES.board);
    expect(credentialInUrl(page.url()), `on the board: ${page.url()}`).toBeNull();

    const signedInBranch = await waitForBoard(page);
    test.info().annotations.push({ type: 'board(signed in)', description: signedInBranch });

    if (signedInBranch === 'populated') {
      await expect(priceCells(page).first()).toBeVisible();
    }

    // The session survives a navigation — if it did not, the refresh-token
    // rotation is broken and every reload would sign the user out.
    await page.reload({ waitUntil: 'domcontentloaded' });
    await waitForBoard(page);
    expect(credentialInUrl(page.url()), `after reload: ${page.url()}`).toBeNull();

    // --- 3. the authenticated socket carries no credential in its URL -----
    await expect
      .poll(() => stream.socketCount(), {
        timeout: STREAM_CONNECT_MS,
        message: 'the signed-in board opened no WebSocket to /ws',
      })
      .toBeGreaterThan(0);

    for (const url of stream.socketUrls()) {
      expect(
        credentialInUrl(url),
        `the authenticated gateway URL carries a credential — the token must travel in the ` +
          `sharpline.bearer. subprotocol, never the URL: ${url}`,
      ).toBeNull();
      expect(new URL(url).search, `the gateway URL carries a query string: ${url}`).toBe('');
    }

    // The gateway refuses a token in the query string as a distinct outcome, so
    // a frontend that tried it would show up here as an unauthorized error.
    for (const frame of stream.received('error')) {
      expect(frame.body?.['code'], `the gateway rejected the connection: ${frame.raw}`).not.toBe('unauthorized');
    }

    // --- 4. sign out -------------------------------------------------------
    await signOut(page);
    expect(credentialInUrl(page.url()), `after sign-out: ${page.url()}`).toBeNull();

    // Market data is public: the board must still work signed out.
    await gotoBoard(page);
    await waitForBoard(page);
    await expectSignedOut(page);

    // --- 5. nothing, anywhere, leaked a credential through a URL ----------
    expect(guard.violations, guard.report()).toEqual([]);
  });

  test('a signed-out visitor can browse the board', async ({ page }) => {
    // Stated separately because it is a product requirement, not a side effect:
    // the board is public market data and must not be gated behind auth.
    const guard = attachUrlGuard(page);

    await gotoBoard(page);
    const branch = await waitForBoard(page);
    test.info().annotations.push({ type: 'board(signed out)', description: branch });

    await expectSignedOut(page);
    expect(guard.violations, guard.report()).toEqual([]);
  });
});
