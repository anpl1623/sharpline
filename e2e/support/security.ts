// ---------------------------------------------------------------------------
// "A token never appears in a URL" — enforced, not assumed.
// ---------------------------------------------------------------------------
// internal/wsgw D5: the gateway authenticates through the `sharpline.bearer.`
// SUBPROTOCOL, and a token in the query string is refused as a distinct
// outcome. The reason is not aesthetic — a URL lands in proxy access logs,
// Referer headers sent to third parties, and browser history, none of which are
// places a bearer token can be revoked from.
//
// That property is only real if something checks it, and the browser is the
// only place it can be checked end to end. This file is that check: it watches
// every request the page makes, every main-frame navigation, and every
// WebSocket URL, for a credential in a query string or fragment.
// ---------------------------------------------------------------------------

import type { Page } from '@playwright/test';

/** Query/fragment parameter names that would be carrying a credential. */
const CREDENTIAL_KEYS = [
  'token',
  'access_token',
  'accesstoken',
  'refresh_token',
  'refreshtoken',
  'id_token',
  'jwt',
  'bearer',
  'auth',
  'authorization',
  'session',
  'api_key',
  'apikey',
  'password',
];

/**
 * A compact JWS: `eyJ...` is the base64url of `{"` — every JWT the auth service
 * issues starts with it. Catches a token smuggled under an innocuous key name.
 */
const JWT_SHAPE = /eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\./u;

export interface UrlViolation {
  readonly where: string;
  readonly url: string;
  readonly reason: string;
}

/**
 * Returns the reason a URL leaks a credential, or null when it is clean.
 * Checks the query string and the fragment; the path itself is checked for a
 * JWT shape too, since `/board?x` is not the only way to get one into a log.
 */
export function credentialInUrl(raw: string): string | null {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return null;
  }

  for (const [key, value] of url.searchParams) {
    if (CREDENTIAL_KEYS.includes(key.toLowerCase()) && value.length > 0) {
      return `query parameter "${key}" carries a value`;
    }
    if (JWT_SHAPE.test(value)) {
      return `query parameter "${key}" carries a JWT-shaped value`;
    }
  }

  const fragment = url.hash.startsWith('#') ? url.hash.slice(1) : url.hash;
  if (fragment.length > 0) {
    const params = new URLSearchParams(fragment);
    for (const [key, value] of params) {
      if (CREDENTIAL_KEYS.includes(key.toLowerCase()) && value.length > 0) {
        return `fragment parameter "${key}" carries a value`;
      }
    }
    if (JWT_SHAPE.test(fragment)) {
      return 'fragment carries a JWT-shaped value';
    }
  }

  if (JWT_SHAPE.test(url.pathname)) {
    return 'path carries a JWT-shaped segment';
  }

  return null;
}

export interface UrlGuard {
  /** Everything caught so far. Assert this is empty at the end of a test. */
  readonly violations: readonly UrlViolation[];
  /** Check one URL immediately — use after `page.url()`. */
  check(where: string, url: string): void;
  /** A readable failure message. */
  report(): string;
}

/**
 * Attach BEFORE the first navigation. Watches:
 *   - every request the page issues (REST, RSC payloads, assets)
 *   - every main-frame navigation (what ends up in browser history)
 *   - every WebSocket URL (the gateway handshake)
 */
export function attachUrlGuard(page: Page): UrlGuard {
  const violations: UrlViolation[] = [];

  const record = (where: string, url: string): void => {
    const reason = credentialInUrl(url);
    if (reason !== null) violations.push({ where, url, reason });
  };

  page.on('request', (request) => record(`request(${request.method()})`, request.url()));
  page.on('websocket', (ws) => record('websocket', ws.url()));
  page.on('framenavigated', (frame) => {
    if (frame === page.mainFrame()) record('navigation', frame.url());
  });

  return {
    violations,
    check: record,
    report: () =>
      violations.length === 0
        ? 'no credential appeared in any URL'
        : violations
            .map((violation) => `${violation.where}: ${violation.reason} — ${violation.url}`)
            .join('\n'),
  };
}
