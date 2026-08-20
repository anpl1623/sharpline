// ---------------------------------------------------------------------------
// Environment, routes and the timing budget.
// ---------------------------------------------------------------------------
// Everything a spec needs to know about *where* the system is, in one file.
// There is no seeding and no fixture anywhere in this suite: every assertion is
// made against the live compose stack fed by the synthetic stochastic provider,
// so the budgets below are sized for a real pipeline
// (provider -> ingest -> Kafka -> pricer -> Postgres -> api/stream -> browser)
// rather than for a mock.
// ---------------------------------------------------------------------------

/**
 * The proxy, and only the proxy. Compose supplies `http://proxy`; a human
 * debugging from the host overrides it with the published proxy address.
 */
export const BASE_URL = process.env.PLAYWRIGHT_BASE_URL ?? 'http://proxy';

/**
 * Application routes this suite depends on.
 *
 * `landing` and `board` are fixed by the phase 7 brief. `register` and `login`
 * are the *canonical* auth routes; `support/auth.ts` falls back to following a
 * header link by accessible name if they are not where they are declared here,
 * so a rename degrades to a slower path rather than a failure.
 */
export const ROUTES = {
  landing: '/',
  board: '/board',
  register: '/register',
  login: '/login',
} as const;

/** Canonical first, fallbacks after. See `openAuthForm` in support/auth.ts. */
export const REGISTER_PATHS: readonly string[] = ['/register', '/signup', '/sign-up', '/auth/register'];
export const LOGIN_PATHS: readonly string[] = ['/login', '/signin', '/sign-in', '/auth/login'];

/** The WebSocket gateway path behind the proxy (Caddyfile route 1). */
export const GATEWAY_PATH = '/ws';

/** The subprotocol the gateway selects and echoes. internal/wsgw. */
export const WS_PROTOCOL = 'sharpline.v1';

// --- timing budget ---------------------------------------------------------

/** Board content (rows or the empty state) must resolve inside this. */
export const BOARD_READY_MS = 30_000;

/** The socket must exist and say hello inside this. */
export const STREAM_CONNECT_MS = 30_000;

/** A snapshot must follow a subscribe inside this. */
export const SNAPSHOT_MS = 30_000;

/**
 * How long we watch a stochastic feed before concluding it genuinely did not
 * move. Observed behaviour is a price change every few seconds across a
 * 123-market league, so 45s is many multiples of the expected interval — long
 * enough that a skip means something, short enough to stay inside the per-test
 * timeout.
 */
export const MOVEMENT_WINDOW_MS = 45_000;

/** Polling grain for the sampling loops in support/stream.ts. */
export const POLL_INTERVAL_MS = 250;

// --- credentials -----------------------------------------------------------

/**
 * A brand-new address on every call.
 *
 * The API has no test-user seeding and registering the same address twice is a
 * 409, so a fixed address would pass once and fail for ever after. Lowercase
 * only: internal/auth normalises to lowercase and the schema enforces
 * `CHECK (email = lower(email))`.
 */
export function uniqueEmail(prefix = 'e2e'): string {
  const stamp = Date.now().toString(36);
  const suffix = Math.random().toString(36).slice(2, 10);
  return `${prefix}-${stamp}-${suffix}@sharpline.test`.toLowerCase();
}

/**
 * internal/auth.MinPasswordLen is 12 bytes. This is comfortably over it and
 * mixes classes so it survives any policy that gets added later.
 */
export function uniquePassword(): string {
  const suffix = Math.random().toString(36).slice(2, 10);
  return `Sharpline-E2E-${suffix}-9x!`;
}
