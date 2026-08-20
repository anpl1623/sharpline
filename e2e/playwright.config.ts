// ---------------------------------------------------------------------------
// Playwright configuration — sharpline critical path (CLAUDE.md §10)
// ---------------------------------------------------------------------------
// Runs ONLY as the compose `e2e` service:
//
//   make e2e
//     -> docker compose ... run --rm e2e
//        -> deploy/docker/playwright.Dockerfile, uid 1001, /workspace = repo e2e/
//
// There is deliberately NO `webServer` block. The stack is already running when
// this executes (the compose service `depends_on: proxy: service_healthy`), and
// starting a server from here would mean a process outside a container of its
// own — precisely the exception CLAUDE.md's prime directive refuses.
//
// VERSION LOCKSTEP: @playwright/test in ./package.json, the version installed
// globally in the image, and the image tag all read 1.62.1. They move together
// or Playwright refuses to run at all.
// ---------------------------------------------------------------------------

import { defineConfig } from '@playwright/test';

/**
 * The compose service supplies this as `http://proxy` — the Caddy reverse proxy
 * on the internal network, which is the ONLY published entrypoint (CLAUDE.md
 * §9). Never a container hostname of a Go service, never a published app port.
 */
const BASE_URL = process.env.PLAYWRIGHT_BASE_URL ?? 'http://proxy';

/**
 * The image bakes `ENV CI=1`, but compose overrides it with `CI: ${CI:-}` so a
 * local `make e2e` sees an empty string (falsy) and a real CI runner sees the
 * runner's own value. Retries therefore switch on the *runner*, not the image.
 */
const IS_CI = Boolean(process.env.CI);

export default defineConfig({
  testDir: './tests',
  outputDir: './test-results',

  // Parallel across files, serial within a file. Every spec provisions its own
  // state (auth registers a fresh account each run) so cross-file parallelism is
  // safe; two workers keeps the WebSocket fanout honest without hammering a
  // single-node stack.
  fullyParallel: false,
  workers: 2,

  forbidOnly: IS_CI,
  retries: IS_CI ? 2 : 0,

  // Generous but finite. These specs sample a live stochastic feed over a window
  // (see live.spec.ts), so a per-test budget below ~90s would turn a quiet feed
  // into a timeout instead of the honest skip it should be.
  // Worst realistic case in live.spec.ts: navigation + board resolution +
  // snapshot + a full 45s movement window, back to back. 180s clears that with
  // room; anything longer would be waiting on a hang rather than on the feed.
  timeout: 180_000,
  globalTimeout: 25 * 60_000,
  expect: { timeout: 20_000 },

  reporter: [
    ['list'],
    ['html', { outputFolder: 'playwright-report', open: 'never' }],
    ['junit', { outputFile: 'test-results/junit.xml' }],
  ],

  use: {
    baseURL: BASE_URL,

    // Caddy issues its own internal-CA certificate in dev, so an https override
    // of PLAYWRIGHT_BASE_URL would otherwise fail the TLS handshake.
    ignoreHTTPSErrors: true,

    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
    video: 'off',

    actionTimeout: 20_000,
    navigationTimeout: 45_000,

    // Pinned so date/number rendering in assertions matches what @/lib/time
    // produces server-side (it is locale-pinned en-GB, UTC-defaulted).
    locale: 'en-GB',
    timezoneId: 'UTC',
  },

  projects: [
    {
      name: 'chromium',
      use: {
        browserName: 'chromium',

        // Explicit rather than `devices['Desktop Chrome']`: the device
        // descriptor is a moving target across Playwright versions and can
        // carry a branded `channel`, which would demand a Google Chrome build
        // the image does not assert the presence of. This is also the layout
        // decision — DESIGN.md's board is the desktop board at >= 768px, and
        // the persistent 24px mono status rail only exists above that
        // breakpoint. Below it the rail collapses to a pip and these specs
        // would be asserting against a different design.
        viewport: { width: 1440, height: 900 },

        launchOptions: {
          // The container runs as uid 1001 with no extra capabilities;
          // playwright.Dockerfile states explicitly that the sandbox flag
          // belongs here rather than baked into the image.
          // --disable-dev-shm-usage pairs with compose's `ipc: host`.
          args: ['--no-sandbox', '--disable-dev-shm-usage'],
        },
      },
    },
  ],
  // No firefox/webkit projects: only Chromium's binaries are present in
  // mcr.microsoft.com/playwright for this image, and declaring a project whose
  // browser is absent fails the whole run rather than one test.
});
