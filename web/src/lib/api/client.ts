/**
 * The REST client for the BROWSER. Every call goes through the reverse proxy.
 *
 * `NEXT_PUBLIC_API_URL` is a proxy PATH (`/api`), not an origin: Caddy maps
 * `/api/*` onto `api:8080`, and a browser cannot resolve `api`. A container
 * hostname in a client-side fetch is not a configuration mistake that degrades
 * — it is a request that cannot be made at all (CLAUDE.md §7).
 *
 * The server-side twin lives in `@/lib/api/server` and is deliberately NOT
 * re-exported from here: importing it from a client component would put the
 * in-network hostname in the browser bundle. See the header of
 * `@/lib/api/transport`.
 */

import type {
  ApiClient,
  BrowserCallOptions,
  HttpMethod,
  QueryValue,
} from '@/lib/api/transport';
import {
  BROWSER_TIMEOUT_MS,
  createApiClient,
  request,
  trimTrailingSlash,
} from '@/lib/api/transport';

/**
 * The browser's base URL. `NEXT_PUBLIC_API_URL` is inlined at build time by
 * Next; the fallback matches `next.config.ts`.
 */
function browserBaseUrl(): string {
  const configured = process.env.NEXT_PUBLIC_API_URL ?? '/api';
  return `${trimTrailingSlash(configured)}/v1`;
}

/**
 * The low-level browser fetch. Prefer the typed methods on `browserApi`; this
 * exists for a call the generated schema does not describe yet.
 */
export function browserFetch<T>(
  path: string,
  options: BrowserCallOptions & {
    readonly method?: HttpMethod;
    readonly query?: Readonly<Record<string, QueryValue>> | undefined;
    readonly body?: unknown;
  } = {},
): Promise<T> {
  return request<T>({
    baseUrl: browserBaseUrl(),
    path,
    method: options.method ?? 'GET',
    query: options.query,
    body: options.body,
    accessToken: options.accessToken,
    signal: options.signal,
    timeoutMs: options.timeoutMs ?? BROWSER_TIMEOUT_MS,
  });
}

/**
 * The client for CLIENT COMPONENTS. Goes through the reverse proxy.
 *
 * `queries.ts` already wires this into every TanStack Query option factory, so
 * most components never touch it directly.
 */
export const browserApi: ApiClient = createApiClient(browserFetch);

export {
  BROWSER_TIMEOUT_MS,
  buildQueryString,
} from '@/lib/api/transport';
export type {
  ApiClient,
  BoardParams,
  BookFilterParams,
  BrowserCallOptions,
  CallOptions,
  HistoryParams,
  QueryValue,
  SearchParams,
} from '@/lib/api/transport';
export { ApiError } from '@/lib/api/errors';
