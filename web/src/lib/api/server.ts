/**
 * The REST client for REACT SERVER COMPONENTS.
 *
 * This module is server-only by construction, and that is the entire reason it
 * is a separate file. `SHARPLINE_INTERNAL_API_URL` (`http://api:8080`) is a
 * non-public variable naming a container the browser cannot resolve; keeping it
 * — and its literal fallback — out of any module a client component imports is
 * what keeps it out of the client bundle.
 *
 * Importing anything from here into a `'use client'` module is a defect. There
 * is no runtime guard: `server-only` is not a dependency of this project and
 * adding one to enforce a rule the module boundary already states was judged
 * the worse trade. The check is the import graph, and it is grep-able —
 * `grep -rl "@/lib/api/server" src/components` must stay empty.
 *
 * Two rules hold on every call here:
 *
 *   - `cache: 'no-store'`. Odds are live. A cached board is a lie told with a
 *     straight face, and it is the one failure a viewer cannot detect by
 *     looking — the prices are plausible and simply wrong.
 *   - A hard `AbortSignal.timeout`. A wedged upstream must not hang a page
 *     render; a 3 second ceiling turns "the page never loads" into "the board
 *     renders its error state", which is a diagnosable outcome.
 */

import type { ApiClient, CallOptions, HttpMethod, QueryValue } from '@/lib/api/transport';
import {
  SERVER_TIMEOUT_MS,
  createApiClient,
  request,
  trimTrailingSlash,
} from '@/lib/api/transport';

/**
 * The in-network base URL, read at request time. Not a `NEXT_PUBLIC_` variable,
 * so it is absent from the client bundle by construction.
 */
function serverBaseUrl(): string {
  const configured =
    process.env.SHARPLINE_INTERNAL_API_URL ?? 'http://api:8080';
  return `${trimTrailingSlash(configured)}/api/v1`;
}

/**
 * The low-level server fetch, for React Server Components. Always `no-store`;
 * see the file comment.
 */
export function serverFetch<T>(
  path: string,
  options: CallOptions & {
    readonly method?: HttpMethod;
    readonly query?: Readonly<Record<string, QueryValue>> | undefined;
    readonly body?: unknown;
  } = {},
): Promise<T> {
  return request<T>({
    baseUrl: serverBaseUrl(),
    path,
    method: options.method ?? 'GET',
    query: options.query,
    body: options.body,
    signal: options.signal,
    timeoutMs: options.timeoutMs ?? SERVER_TIMEOUT_MS,
    cache: 'no-store',
  });
}

/**
 * The client for REACT SERVER COMPONENTS. Uses the in-network service name, is
 * always `no-store`, and times out in 3 seconds.
 *
 * Its `accessToken` option is inert — a server render holds no user credential.
 */
export const serverApi: ApiClient = createApiClient(serverFetch);

export { SERVER_TIMEOUT_MS } from '@/lib/api/transport';
