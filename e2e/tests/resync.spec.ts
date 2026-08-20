// ---------------------------------------------------------------------------
// A sequence gap triggers a resync, and the board recovers WITHOUT a reload.
// ---------------------------------------------------------------------------
// live.spec.ts already asserts this conditionally: if a gap happened to occur,
// a resync must have followed. On a healthy loopback connection no gap ever
// occurs, so that assertion is vacuously true and proves nothing.
//
// This file makes it unconditional by forcing the gap — see support/gap.ts for
// how, and for a careful statement of what the technique does and does not
// prove. Everything asserted below is behaviour of the real client
// (web/src/lib/ws/client.ts) reacting to a real stream.
//
// The contract, from CLAUDE.md §5 and internal/wsgw D3/D4:
//
//   1. a gap in the per-connection sequence is DETECTED
//   2. the client answers it with a `resync` naming NO channels — "every channel
//      this connection holds", which is what a client that does not know which
//      channel lost a frame must send
//   3. the server answers with fresh snapshots
//   4. the board is whole again, on the SAME document — no reload, no navigation
//   5. none of it puts a credential in the socket URL
// ---------------------------------------------------------------------------

import { expect, test } from '@playwright/test';
import { gotoBoard, waitForBoard } from '../support/board';
import { MOVEMENT_WINDOW_MS, POLL_INTERVAL_MS, SNAPSHOT_MS } from '../support/env';
import { gapState, installGapInjector } from '../support/gap';
import { credentialInUrl } from '../support/security';
import { priceCells, quotedPriceCells } from '../support/selectors';
import { sleep, StreamRecorder } from '../support/stream';

test.describe('sequence gap recovery', () => {
  test('a forced gap is answered by a resync and the board recovers in place', async ({ page }) => {
    // Order matters twice over: the recorder only sees sockets opened after it
    // attaches, and the injector only shims documents created after it installs.
    const stream = StreamRecorder.attach(page);
    await installGapInjector(page);

    await gotoBoard(page);
    const branch = await waitForBoard(page);
    test.skip(
      branch === 'empty',
      'the board is legitimately empty, so no delta will arrive to drop — nothing to force',
    );

    const loadsBefore = (await gapState(page)).loads;
    expect(loadsBefore, 'the injector should have seen exactly one document load').toBe(1);

    // --- 1. a delta is swallowed --------------------------------------------
    // The feed is stochastic, so how long this takes is not deterministic even
    // though the drop itself is.
    //
    // SKIP rather than fail if nothing arrives. Everything after this point is
    // deterministic, but getting here is not: it needs the pipeline to actually
    // publish a delta, and `ingest` being stopped is an honest reason for this
    // test not to run — it is not evidence that resync is broken. Same rule
    // live.spec.ts applies to movement.
    const deadline = Date.now() + MOVEMENT_WINDOW_MS;
    let droppedSeq: number | null = null;
    while (Date.now() < deadline) {
      droppedSeq = (await gapState(page)).droppedSeq;
      if (droppedSeq !== null) break;
      await sleep(POLL_INTERVAL_MS);
    }
    test.skip(
      droppedSeq === null,
      'no delta frame arrived within the movement window, so no gap could be forced — ' +
        `the feed was silent, not the client. Wire: ${stream.describe()}`,
    );

    // --- 2. the client asks for a resync ------------------------------------
    await expect
      .poll(() => stream.sent('resync').length, {
        timeout: SNAPSHOT_MS,
        intervals: [POLL_INTERVAL_MS],
        message:
          `sequence ${String(droppedSeq)} was withheld from the client and it did not send a resync. ` +
          'internal/wsgw D3 stamps seq at enqueue specifically so this gap is visible to the client; ' +
          `a client that does not act on it makes that design pointless. Wire: ${stream.describe()}`,
      })
      .toBeGreaterThan(0);

    const resync = stream.sent('resync')[0];
    expect(resync, 'the resync frame was recorded').toBeDefined();

    // No channels named. The client detected a gap in the CONNECTION's sequence
    // space and cannot know which channel lost the frame, so it must ask for all
    // of them; naming a guess would leave the other channels silently stale.
    const channels = resync?.body?.['channels'];
    expect(
      channels === undefined || (Array.isArray(channels) && channels.length === 0),
      `a gap-triggered resync must name no channels, got ${JSON.stringify(channels)}`,
    ).toBe(true);

    // --- 3. the server answers with snapshots -------------------------------
    await expect
      .poll(() => stream.snapshotsAfterResync().length, {
        timeout: SNAPSHOT_MS,
        intervals: [POLL_INTERVAL_MS],
        message: `the client asked for a resync and no snapshot followed. Wire: ${stream.describe()}`,
      })
      .toBeGreaterThan(0);

    // --- 4. the board recovered on the same document ------------------------
    const after = await gapState(page);
    expect(
      after.loads,
      'the board reloaded to recover. A resync exists precisely so it does not have to: ' +
        'CLAUDE.md §5 makes a gap a stream-level event, not a page-level one.',
    ).toBe(loadsBefore);

    await expect(quotedPriceCells(page).first()).toBeVisible();
    expect(
      await priceCells(page).count(),
      'the board has no price cells after recovering',
    ).toBeGreaterThan(0);

    // --- 5. still no credential in the URL ----------------------------------
    for (const url of stream.socketUrls()) {
      expect(credentialInUrl(url), `the gateway URL carries a credential: ${url}`).toBeNull();
    }

    // The client must not have treated a gap as a reason to tear the socket
    // down: a resync is cheaper than a reconnect and is the whole point of D4.
    expect(
      stream.socketCount(),
      'a gap should be answered by a resync on the SAME socket, not by reconnecting',
    ).toBe(1);
  });
});
