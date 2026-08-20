// ---------------------------------------------------------------------------
// The live stream: protocol first, movement second.
// ---------------------------------------------------------------------------
// The feed is a stochastic market maker. Asserting "a price changed in the next
// N seconds" is asserting about an RNG, and a flaky assertion about an RNG is
// worse than an honest skip — it teaches everyone to ignore a red suite.
//
// So this file is split by determinism:
//
//   HARD  — the protocol. `hello` arrives first, `seq` starts at 1 and advances
//           monotonically, a subscribe is answered, a gap is answered by a
//           client resync, and no credential ever appears in the socket URL.
//           None of that depends on whether a price moved.
//
//   SOFT  — movement. Deltas on the wire, then a visible change on the board.
//           Both SKIP with a clear message if the feed was genuinely quiet.
//
// The wire is read through `page.on('websocket')`, so these assertions need no
// cooperation from the frontend at all: no test id, no exposed counter, no
// debug hook. internal/wsgw/doc.go is the contract.
// ---------------------------------------------------------------------------

import { expect, test } from '@playwright/test';
import { gotoBoard, waitForBoard } from '../support/board';
import {
  MOVEMENT_WINDOW_MS,
  POLL_INTERVAL_MS,
  SNAPSHOT_MS,
  STREAM_CONNECT_MS,
  WS_PROTOCOL,
} from '../support/env';
import { readPriceTexts } from '../support/odds';
import { credentialInUrl } from '../support/security';
import {
  accessibleNameOf,
  decayingRails,
  priceCells,
  STREAM_LIVE_LABEL,
  streamStateLabel,
  streamStatus,
} from '../support/selectors';
import { sleep, StreamRecorder } from '../support/stream';

test.describe('live stream', () => {
  test('the gateway connects and speaks the snapshot/delta protocol', async ({ page }) => {
    const stream = StreamRecorder.attach(page);
    await gotoBoard(page);

    // --- the socket exists -------------------------------------------------
    await expect
      .poll(() => stream.socketCount(), {
        timeout: STREAM_CONNECT_MS,
        message: 'the board opened no WebSocket to /ws — market data is public, so this must happen signed out',
      })
      .toBeGreaterThan(0);

    // internal/wsgw D5: the token travels in the `sharpline.bearer.`
    // subprotocol, never the URL. Asserted here for the anonymous connection
    // and again in auth.spec.ts for the authenticated one.
    for (const url of stream.socketUrls()) {
      expect(credentialInUrl(url), `the gateway URL carries a credential: ${url}`).toBeNull();
      expect(new URL(url).search, `the gateway URL carries a query string: ${url}`).toBe('');
    }

    // --- hello -------------------------------------------------------------
    await expect
      .poll(() => stream.received('hello').length, {
        timeout: STREAM_CONNECT_MS,
        message: `no hello frame arrived. Wire: ${stream.describe()}`,
      })
      .toBeGreaterThan(0);

    const hello = stream.hello(0);
    expect(hello, 'socket 0 must say hello').toBeDefined();
    expect(hello?.seq, 'hello is the first frame on the connection, so seq is 1').toBe(1);
    expect(hello?.body?.['protocol'], 'the server selects and echoes only sharpline.v1').toBe(WS_PROTOCOL);
    expect(typeof hello?.body?.['connection_id'], 'hello names the connection').toBe('string');
    expect(
      typeof hello?.body?.['heartbeat_seconds'],
      'hello states the heartbeat interval the client must honour',
    ).toBe('number');

    // Anonymous is legal and is the default — the board must work signed out.
    expect(hello?.body?.['authenticated'], 'a signed-out board connects anonymously').toBe(false);

    // --- every frame is sequenced -----------------------------------------
    expect(stream.malformed(), 'every server frame is a JSON object').toEqual([]);
    expect(stream.sequenceViolations().join('\n')).toBe('');

    // --- subscriptions -----------------------------------------------------
    const branch = await waitForBoard(page);
    if (branch === 'empty') {
      test.info().annotations.push({
        type: 'stream',
        description: 'board is empty, so there were no channels to subscribe to; protocol assertions still hold',
      });
      return;
    }

    await expect
      .poll(() => stream.sent('subscribe').length, {
        timeout: SNAPSHOT_MS,
        message: `a populated board must subscribe to channels. Wire: ${stream.describe()}`,
      })
      .toBeGreaterThan(0);

    const channels = stream.subscribedChannels();
    expect(channels.length, 'a subscribe frame must name at least one channel').toBeGreaterThan(0);
    for (const channel of channels) {
      // internal/wsgw channel algebra: event:{id} | market:{id} | league:{slug}.
      // League is keyed by SLUG so the board's URL and its subscription are the
      // same string.
      expect(channel, `"${channel}" is not a valid channel name`).toMatch(/^(event|market|league):.+$/u);
    }

    await expect
      .poll(() => stream.received('ack').length, {
        timeout: SNAPSHOT_MS,
        message: `a subscribe must be acknowledged. Wire: ${stream.describe()}`,
      })
      .toBeGreaterThan(0);

    // Nothing the board asked for may be rejected: a rejection here means the
    // frontend built a channel name the gateway does not recognise.
    for (const ack of stream.received('ack')) {
      const rejected = ack.body?.['rejected'];
      if (Array.isArray(rejected) && rejected.length > 0) {
        expect(rejected, `the gateway rejected a channel the board asked for: ${JSON.stringify(rejected)}`).toEqual([]);
      }
    }

    await expect
      .poll(() => stream.received('snapshot').length, {
        timeout: SNAPSHOT_MS,
        message: `a subscribe must be followed by a snapshot. Wire: ${stream.describe()}`,
      })
      .toBeGreaterThan(0);

    // --- errors ------------------------------------------------------------
    const errors = stream.received('error');
    expect(
      errors.map((frame) => frame.raw),
      'the gateway sent an error frame during a plain board browse',
    ).toEqual([]);
  });

  test('a sequence gap is answered by a client resync', async ({ page }) => {
    // `seq` is assigned at ENQUEUE precisely so that a dropped slow-consumer
    // buffer shows on the wire as a visible hole (4 then 41). The client
    // contract is: one gap, one resync with no channels ("everything this
    // connection holds"), then reconcile from the snapshots that follow.
    //
    // A gap cannot be provoked from a browser without a slow consumer, so this
    // does not assert that one happened — it asserts that IF one happened, it
    // was answered. On a healthy run it passes vacuously, and it is the only
    // thing standing between a silent frontend regression and a board that goes
    // quietly stale in production.
    const stream = StreamRecorder.attach(page);
    await gotoBoard(page);
    await waitForBoard(page);

    await expect
      .poll(() => stream.received('hello').length, { timeout: STREAM_CONNECT_MS })
      .toBeGreaterThan(0);

    // Give the connection a window in which a gap could occur at all.
    await stream.waitUntil(() => stream.gaps().length > 0, MOVEMENT_WINDOW_MS / 2);

    const gaps = stream.gaps();
    test.info().annotations.push({
      type: 'gaps',
      description: gaps.length === 0 ? 'none observed (healthy)' : JSON.stringify(gaps),
    });

    const unanswered = stream.unansweredGaps();
    expect(
      unanswered,
      'a sequence gap was observed and the client did not send a resync. ' +
        'The board is now silently missing every update in that hole.',
    ).toEqual([]);

    // A server-initiated resync (slow_consumer / presence_lost) must also be
    // followed by fresh snapshots, otherwise the board never recovers.
    if (stream.received('resync').length > 0) {
      await expect
        .poll(() => stream.received('snapshot').length, {
          timeout: SNAPSHOT_MS,
          message: 'the server asked for a resync and no snapshot followed',
        })
        .toBeGreaterThan(0);
    }
  });

  test('the status surface reports the connection state', async ({ page }) => {
    await gotoBoard(page);

    // DESIGN.md: the engineering layer is permanent, not a debug drawer. It is
    // on screen at every breakpoint — the 24px mono rail at >= 768px, the
    // collapsed pip below it.
    const status = streamStatus(page);
    await expect(status).toBeVisible();
    expect(await accessibleNameOf(status), 'the status surface must be named').not.toBe('');

    // The state must be carried in WORDS, never by the pip's fill alone.
    // `describeStream()` in web/src/lib/ws/client.ts produces one of five
    // strings; matched loosely so an em dash or a casing change in the copy does
    // not fail the run.
    const label = streamStateLabel(page);
    await expect(label, 'no connection state is stated in words — colour alone must never carry it').toBeVisible();

    await expect
      .poll(async () => (await label.innerText()).replace(/\s+/gu, ' ').trim(), {
        timeout: STREAM_CONNECT_MS,
        message: 'the stream never reported itself live',
      })
      .toMatch(STREAM_LIVE_LABEL);
  });

  test('prices move on the wire', async ({ page }) => {
    const stream = StreamRecorder.attach(page);
    await gotoBoard(page);
    const branch = await waitForBoard(page);
    test.skip(branch === 'empty', 'the board is legitimately empty — there is nothing to move');

    await expect
      .poll(() => stream.received('snapshot').length, { timeout: SNAPSHOT_MS })
      .toBeGreaterThan(0);

    // Sampling a stochastic feed over a window. This is the one legitimate use
    // of a wall-clock wait in the suite, and it is a *bounded poll*, not a
    // blind sleep: it returns the moment the first delta lands.
    const moved = await stream.waitUntil(() => stream.received('delta').length > 0, MOVEMENT_WINDOW_MS);

    test.skip(
      !moved,
      `the synthetic feed produced no delta in ${String(MOVEMENT_WINDOW_MS / 1000)}s. ` +
        `Skipping rather than failing: this is a statement about an RNG, not about the code. Wire: ${stream.describe()}`,
    );

    const deltas = stream.received('delta');
    expect(deltas.length).toBeGreaterThan(0);

    // Each delta names a channel and carries either an updated market or a
    // tombstone. Nothing else is a legal delta.
    for (const delta of deltas.slice(0, 20)) {
      expect(typeof delta.body?.['channel'], 'a delta names the channel it belongs to').toBe('string');
      const hasMarket = delta.body?.['market'] !== undefined;
      const hasRemoval = typeof delta.body?.['removed'] === 'string';
      expect(hasMarket || hasRemoval, `delta carries neither a market nor a removal: ${delta.raw.slice(0, 200)}`).toBe(
        true,
      );
    }

    // Deltas must remain in sequence with everything else on the connection.
    expect(stream.sequenceViolations().join('\n')).toBe('');
  });

  test('a moving price is visible on the board', async ({ page }) => {
    const stream = StreamRecorder.attach(page);
    await gotoBoard(page);
    const branch = await waitForBoard(page);
    test.skip(branch === 'empty', 'the board is legitimately empty — there is nothing to render moving');

    const cells = priceCells(page);
    const sampleSize = Math.min(await cells.count(), 20);
    const before = (await readPriceTexts(cells, sampleSize)).join('|');

    // Two independent corroborating signals, either of which counts:
    //   * a numeral changed, or
    //   * a delta rail is mid-decay (globals.css `.rail-decaying`) — the
    //     signature element, and the one that proves the change was rendered as
    //     information rather than as a re-render.
    //
    // The rail is checked as well as the text because a delta may legitimately
    // land on a market that is not in the sampled slice.
    let sawRail = false;
    let sawTextChange = false;
    let observed = false;

    const deadline = Date.now() + MOVEMENT_WINDOW_MS;
    while (!observed) {
      if ((await decayingRails(page).count()) > 0) {
        sawRail = true;
        observed = true;
        break;
      }
      if ((await readPriceTexts(cells, sampleSize)).join('|') !== before) {
        sawTextChange = true;
        observed = true;
        break;
      }
      if (Date.now() >= deadline) break;
      await sleep(POLL_INTERVAL_MS);
    }

    test.skip(
      !observed,
      `no visible price change in ${String(MOVEMENT_WINDOW_MS / 1000)}s — the feed was quiet, or every ` +
        `delta landed outside the sampled slice. Skipping rather than failing. Wire: ${stream.describe()}`,
    );

    test.info().annotations.push({
      type: 'movement',
      description: `rail=${String(sawRail)} text=${String(sawTextChange)} deltas=${String(stream.received('delta').length)}`,
    });

    // If the board visibly moved, the wire must explain why. A board that
    // changes without a delta behind it is showing something it invented.
    expect(
      stream.received('delta').length + stream.received('snapshot').length,
      'the board changed with no snapshot or delta behind it',
    ).toBeGreaterThan(0);
  });
});
