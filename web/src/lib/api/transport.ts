/**
 * The typed REST client's SHARED half: query encoding, the timeout/abort
 * plumbing, one `request`, and the `ApiClient` surface built over an injected
 * transport.
 *
 * The two call sites live in separate modules — `@/lib/api/client` for the
 * browser and `@/lib/api/server` for React Server Components — and that split
 * is load-bearing rather than cosmetic.
 *
 * # The browser goes through the proxy. Always.
 *
 * `NEXT_PUBLIC_API_URL` is a PROXY PATH (`/api`), not an origin. Caddy maps
 * `/api/*` onto `api:8080`. A browser cannot resolve `api`, so a container
 * hostname in a client-side fetch is not a configuration mistake that degrades
 * — it is a request that cannot be made at all. CLAUDE.md §7 states the rule.
 *
 * # A server component is inside the network and uses the service name
 *
 * `SHARPLINE_INTERNAL_API_URL` (`http://api:8080`) is a non-public variable.
 * Keeping `serverFetch` in its own module is what keeps that hostname — the
 * literal fallback included — out of the client bundle entirely. When both
 * halves shared one module, importing `browserApi` from a client component
 * dragged the server half in with it and the string `http://api:8080` shipped
 * to every browser. It was dead code, but it was also a standing invitation to
 * call `serverApi` from a component that cannot reach it.
 *
 * Two further rules hold on the server side:
 *
 *   - `cache: 'no-store'` on every read. Odds are live. A cached board is a lie
 *     told with a straight face, and it is the one failure a viewer cannot
 *     detect by looking — the prices are plausible and simply wrong.
 *   - A hard `AbortSignal.timeout`. A wedged upstream must not hang a page
 *     render; a 3 second ceiling turns "the page never loads" into "the board
 *     renders its error state", which is a diagnosable outcome.
 *
 * # odds_format is never sent
 *
 * The API's `odds_format` parameter adds a rendered `display` string to a REST
 * payload. This frontend does not use it: `decimal_odds` is canonical and the
 * WebSocket carries only decimal, so a REST price rendered from `display` and a
 * streamed price rendered from `decimal_odds` would be produced by two different
 * conversions and would eventually disagree. Everything converts client-side,
 * through `@/lib/odds/format`, from the one canonical number. Omitting the
 * parameter also gets the API's default, which is decimal.
 *
 * # The access token
 *
 * It is attached as `Authorization: Bearer` and nowhere else. It never enters a
 * URL, a query string, a log line or an error. Only `BrowserCallOptions` carries
 * one — a server render has no user credential to supply, and the type says so.
 *
 * # Idempotency-Key is a HEADER, and the money endpoints require it
 *
 * `POST /wagers` and `POST /wagers/{id}/cashout` derive their resource
 * identifier from `(user, Idempotency-Key)`, so a retried submit collides with
 * the primary key it already wrote instead of booking a second bet. The key is
 * therefore not optional and is not defaulted here: the two methods that need
 * one take it as a required argument, so a caller cannot forget it and get an
 * at-least-once money path by omission.
 *
 * It travels as a header rather than in the body for the reason the header
 * belongs to the SUBMIT and not to the slip — reusing a key with a different
 * body returns the ORIGINAL resource unchanged, which only makes sense if the
 * key is framing rather than content.
 */

import {
  abortedError,
  apiErrorFromBody,
  malformedResponseError,
  networkError,
  timeoutError,
} from '@/lib/api/errors';
import type {
  SchemaAccount,
  SchemaBalanceResponse,
  SchemaBoardPage,
  SchemaBookPage,
  SchemaCashOutQuote,
  SchemaEventDetail,
  SchemaHistoryResolution,
  SchemaHistorySeries,
  SchemaLeaguePage,
  SchemaMarketComparison,
  SchemaPlacement,
  SchemaPlaceWagerRequest,
  SchemaSearchPage,
  SchemaSessionResponse,
  SchemaSlipQuote,
  SchemaSlipQuoteRequest,
  SchemaSportPage,
  SchemaWager,
  SchemaWagerPage,
  SchemaWagerStatus,
} from '@/lib/api/schema';

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

/** Default deadline for a server-side render's upstream call. */
export const SERVER_TIMEOUT_MS = 3_000;

/** Default deadline for a browser call. Longer: the user can see it working. */
export const BROWSER_TIMEOUT_MS = 10_000;

export function trimTrailingSlash(value: string): string {
  return value.endsWith('/') ? value.slice(0, -1) : value;
}

// -----------------------------------------------------------------------------
// Query strings
// -----------------------------------------------------------------------------

export type QueryValue =
  | string
  | number
  | boolean
  | readonly string[]
  | null
  | undefined;

/**
 * Builds a query string, OMITTING absent parameters entirely.
 *
 * An empty string is not the same as an absent parameter: `?q=` is a search for
 * the empty string and is a 400, where omitting `q` is a different request. A
 * repeated parameter (`book=a&book=b`) is appended once per value, which is what
 * the API's `BookFilter` expects.
 */
export function buildQueryString(
  params: Readonly<Record<string, QueryValue>>,
): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null) continue;
    if (typeof value === 'object') {
      for (const entry of value) {
        if (entry !== '') search.append(key, entry);
      }
      continue;
    }
    const rendered = String(value);
    if (rendered === '') continue;
    search.append(key, rendered);
  }
  const rendered = search.toString();
  return rendered === '' ? '' : `?${rendered}`;
}

// -----------------------------------------------------------------------------
// Signals
// -----------------------------------------------------------------------------

interface CombinedSignal {
  readonly signal: AbortSignal;
  readonly dispose: () => void;
  timedOut: boolean;
}

/**
 * Combines the caller's signal with this request's own deadline.
 *
 * Hand-rolled rather than `AbortSignal.any`, so the combiner can record WHICH
 * side fired: a timeout and a caller-initiated cancellation are different
 * outcomes, and reporting a superseded query as "the server timed out" would put
 * a false failure in front of the user on every keystroke of a search box.
 */
function combineSignals(
  timeoutMs: number,
  caller: AbortSignal | undefined,
): CombinedSignal {
  const controller = new AbortController();
  const combined: CombinedSignal = {
    signal: controller.signal,
    timedOut: false,
    dispose: () => {
      clearTimeout(timer);
      caller?.removeEventListener('abort', onCallerAbort);
    },
  };

  const timer = setTimeout(() => {
    combined.timedOut = true;
    controller.abort();
  }, timeoutMs);

  const onCallerAbort = (): void => {
    controller.abort();
  };

  if (caller !== undefined) {
    if (caller.aborted) {
      controller.abort();
    } else {
      caller.addEventListener('abort', onCallerAbort, { once: true });
    }
  }

  return combined;
}

// -----------------------------------------------------------------------------
// The request builder
// -----------------------------------------------------------------------------

export type HttpMethod = 'GET' | 'POST' | 'DELETE';

interface RequestSpec {
  readonly baseUrl: string;
  readonly path: string;
  readonly method?: HttpMethod;
  readonly query?: Readonly<Record<string, QueryValue>> | undefined;
  readonly body?: unknown;
  readonly accessToken?: string | undefined;
  /**
   * Extra request headers. `Idempotency-Key` is the only one this frontend
   * sends, and it is spelled at the call site rather than assembled here so a
   * reader of `placeWager` can see the whole contract in one place.
   *
   * `Accept`, `Content-Type` and `Authorization` are owned by `request` and are
   * applied AFTER these, so nothing passed here can overwrite the credential
   * header or the content type with something the server would reject.
   */
  readonly headers?: Readonly<Record<string, string>> | undefined;
  readonly signal?: AbortSignal | undefined;
  readonly timeoutMs: number;
  readonly cache?: RequestCache | undefined;
}

/**
 * One request. Parses the `{"error": {...}}` envelope into a typed `ApiError`,
 * returns `undefined` for a 204, and never lets a raw `TypeError` from `fetch`
 * escape as something a UI has to guess about.
 */
export async function request<T>(spec: RequestSpec): Promise<T> {
  const url = `${spec.baseUrl}${spec.path}${
    spec.query === undefined ? '' : buildQueryString(spec.query)
  }`;

  const headers = new Headers();
  for (const [name, value] of Object.entries(spec.headers ?? {})) {
    if (value !== '') headers.set(name, value);
  }
  headers.set('Accept', 'application/json');
  if (spec.body !== undefined) {
    headers.set('Content-Type', 'application/json');
  }
  if (spec.accessToken !== undefined && spec.accessToken !== '') {
    headers.set('Authorization', `Bearer ${spec.accessToken}`);
  }

  const combined = combineSignals(spec.timeoutMs, spec.signal);

  const init: RequestInit = {
    method: spec.method ?? 'GET',
    headers,
    signal: combined.signal,
  };
  if (spec.cache !== undefined) init.cache = spec.cache;
  if (spec.body !== undefined) init.body = JSON.stringify(spec.body);

  let response: Response;
  try {
    response = await fetch(url, init);
  } catch (cause) {
    if (combined.timedOut) throw timeoutError(spec.timeoutMs, cause);
    if (spec.signal?.aborted === true) throw abortedError(cause);
    throw networkError(cause);
  } finally {
    combined.dispose();
  }

  if (response.status === 204) {
    // The one endpoint that answers 204 is logout, whose declared result is
    // void. Every other caller asks for a body and gets one.
    return undefined as unknown as T;
  }

  const text = await response.text().catch(() => '');

  let parsed: unknown;
  if (text !== '') {
    try {
      parsed = JSON.parse(text);
    } catch (cause) {
      if (!response.ok) throw apiErrorFromBody(response.status, undefined);
      throw malformedResponseError(cause);
    }
  }

  if (!response.ok) {
    throw apiErrorFromBody(response.status, parsed);
  }
  if (parsed === undefined) {
    throw malformedResponseError();
  }
  return parsed as T;
}

// -----------------------------------------------------------------------------
// Call options
// -----------------------------------------------------------------------------

/** Options every call accepts. */
export interface CallOptions {
  /** Caller cancellation — a superseded query, a navigation. */
  readonly signal?: AbortSignal | undefined;
  /** Overrides the transport's default deadline. */
  readonly timeoutMs?: number | undefined;
}

/** Browser calls may additionally present a credential. */
export interface BrowserCallOptions extends CallOptions {
  /**
   * The in-memory access token. Sent as `Authorization: Bearer` and nowhere
   * else. Never persisted by this module, never logged, never placed in a URL.
   */
  readonly accessToken?: string | undefined;
}

// -----------------------------------------------------------------------------
// Parameter shapes
// -----------------------------------------------------------------------------

/**
 * Board query parameters.
 *
 * `book` is repeatable and restricts prices to those books by slug. An unknown
 * slug is a 400, not a silent empty result — a typo that quietly returns nothing
 * is the worst possible outcome for a price comparison.
 *
 * Pagination is KEYSET. There is no total count and there never will be: pass
 * the previous page's `page.next_cursor` back as `cursor`, and stop when
 * `page.has_more` is false.
 */
export interface BoardParams {
  /** RFC 3339 upper bound on `scheduled_start`. Defaults server-side to +24h. */
  readonly startingBefore?: string | undefined;
  readonly limit?: number | undefined;
  readonly cursor?: string | undefined;
  readonly book?: readonly string[] | undefined;
}

/** Event detail and market comparison both take only a book filter. */
export interface BookFilterParams {
  readonly book?: readonly string[] | undefined;
}

/**
 * Line-movement series parameters.
 *
 * `book` and `from` are REQUIRED. `book` because a chart mixing two books'
 * quotes on one series is not a line movement chart, it is two of them
 * overlaid. A window that would exceed `maxPoints` is REJECTED with a 422 and is
 * never silently truncated — a truncated series is a chart that lies about when
 * the line moved.
 */
export interface HistoryParams {
  readonly book: string;
  readonly from: string;
  readonly to?: string | undefined;
  readonly resolution?: SchemaHistoryResolution | undefined;
  readonly maxPoints?: number | undefined;
}

/**
 * Search parameters. `q` is a PREFIX match on competitor name and must be at
 * least 2 characters. The status set searched is narrower than the board's:
 * scheduled and live only.
 */
export interface SearchParams {
  readonly limit?: number | undefined;
  readonly cursor?: string | undefined;
}

/**
 * Wager history parameters.
 *
 * `status` is REPEATABLE and its values union: `?status=placed&status=open` is
 * "everything still running". The filter is applied to the page the server
 * scanned, so a filtered page may hold fewer than `limit` rows while `has_more`
 * is still true — follow `next_cursor` until `has_more` is false rather than
 * stopping at a short page.
 */
export interface WagerListParams {
  readonly status?: readonly SchemaWagerStatus[] | undefined;
  readonly limit?: number | undefined;
  readonly cursor?: string | undefined;
}

/**
 * A client-chosen key that makes a retry safe, and the reason the two money
 * methods take it as a REQUIRED positional argument rather than an option.
 *
 * The wager identifier is derived from `(user, this key)`, so a replayed submit
 * attempts the primary key it already inserted and the service answers with the
 * EXISTING resource. Omitting the key is not "the default behaviour" — it is a
 * `400`, and an endpoint that accepted a request without one would have an
 * at-least-once money path. Making it a required argument is how that stops
 * being a thing a caller can forget.
 */
export type IdempotencyKey = string;

function boardQuery(
  params: BoardParams | undefined,
): Readonly<Record<string, QueryValue>> {
  return {
    starting_before: params?.startingBefore,
    limit: params?.limit,
    cursor: params?.cursor,
    book: params?.book,
  };
}

// -----------------------------------------------------------------------------
// The client surface
// -----------------------------------------------------------------------------

/**
 * Every endpoint this frontend uses, typed from the generated schema.
 *
 * Implemented twice — `browserApi` and `serverApi` — over the two transports.
 * They are deliberately separate values rather than one client with a flag: a
 * server component calling the browser client would build a relative URL with no
 * origin and fail at request time, and a client component calling the server one
 * would try to resolve `api`. Choosing the wrong import is then a mistake that
 * shows up immediately rather than in production.
 */
export interface ApiClient {
  getBoard(params?: BoardParams, options?: BrowserCallOptions): Promise<SchemaBoardPage>;
  getLeagueBoard(
    leagueSlug: string,
    params?: BoardParams,
    options?: BrowserCallOptions,
  ): Promise<SchemaBoardPage>;
  getEvent(
    eventId: string,
    params?: BookFilterParams,
    options?: BrowserCallOptions,
  ): Promise<SchemaEventDetail>;
  compareMarketPrices(
    marketId: string,
    params?: BookFilterParams,
    options?: BrowserCallOptions,
  ): Promise<SchemaMarketComparison>;
  getSelectionHistory(
    selectionId: string,
    params: HistoryParams,
    options?: BrowserCallOptions,
  ): Promise<SchemaHistorySeries>;
  searchEvents(
    query: string,
    params?: SearchParams,
    options?: BrowserCallOptions,
  ): Promise<SchemaSearchPage>;
  listSports(options?: BrowserCallOptions): Promise<SchemaSportPage>;
  listLeaguesInSport(
    sportSlug: string,
    options?: BrowserCallOptions,
  ): Promise<SchemaLeaguePage>;
  listBooks(options?: BrowserCallOptions): Promise<SchemaBookPage>;
  register(
    email: string,
    password: string,
    options?: BrowserCallOptions,
  ): Promise<SchemaSessionResponse>;
  login(
    email: string,
    password: string,
    totpCode?: string,
    options?: BrowserCallOptions,
  ): Promise<SchemaSessionResponse>;
  refresh(
    refreshToken: string,
    options?: BrowserCallOptions,
  ): Promise<SchemaSessionResponse>;
  logout(refreshToken: string, options?: BrowserCallOptions): Promise<void>;
  getAccount(options: BrowserCallOptions): Promise<SchemaAccount>;

  /** The spendable and escrowed balances, folded from the ledger. */
  getBalance(options: BrowserCallOptions): Promise<SchemaBalanceResponse>;

  /**
   * Price a slip WITHOUT placing it. Writes nothing and moves no money — no
   * wager row, no ledger entry, no idempotency key — so a client may call it as
   * often as the user edits the slip.
   *
   * A moved price is REPORTED here (`price_moved: true` with both numbers on the
   * leg) rather than refused, because a quote whose whole job is to describe the
   * current state should not fail when the state is interesting.
   */
  quoteSlip(
    body: SchemaSlipQuoteRequest,
    options: BrowserCallOptions,
  ): Promise<SchemaSlipQuote>;

  /**
   * Book the ticket. `201` means booked now, `200` means booked earlier by this
   * same key — both carry the same body, and `Placement.replayed` restates the
   * distinction inside it so a client that only reads the body still knows.
   */
  placeWager(
    body: SchemaPlaceWagerRequest,
    idempotencyKey: IdempotencyKey,
    options: BrowserCallOptions,
  ): Promise<SchemaPlacement>;

  listWagers(
    params: WagerListParams | undefined,
    options: BrowserCallOptions,
  ): Promise<SchemaWagerPage>;

  /** Somebody else's wager is a 404, not a 403. See the OpenAPI note. */
  getWager(wagerId: string, options: BrowserCallOptions): Promise<SchemaWager>;

  /**
   * What the book will pay to close this ticket now.
   *
   * A snapshot at `quoted_at`, NOT an offer held open — there is deliberately no
   * expiry field, and whatever takes the cash-out re-prices while holding the
   * wager row.
   */
  getCashOutQuote(
    wagerId: string,
    options: BrowserCallOptions,
  ): Promise<SchemaCashOutQuote>;

  /**
   * Take the cash-out at a value the customer was SHOWN.
   *
   * `acceptedValueMinor` must be the number on screen, not a freshly fetched
   * one: echoing a re-read quote defeats the control entirely, and the service
   * refuses with `409 price_moved` when the value has changed.
   */
  takeCashOut(
    wagerId: string,
    acceptedValueMinor: number,
    idempotencyKey: IdempotencyKey,
    options: BrowserCallOptions,
  ): Promise<SchemaWager>;
}

type Transport = <T>(
  path: string,
  options: BrowserCallOptions & {
    readonly method?: HttpMethod;
    readonly query?: Readonly<Record<string, QueryValue>> | undefined;
    readonly body?: unknown;
    readonly headers?: Readonly<Record<string, string>> | undefined;
  },
) => Promise<T>;

function encode(segment: string): string {
  return encodeURIComponent(segment);
}

export function createApiClient(send: Transport): ApiClient {
  return {
    getBoard: (params, options = {}) =>
      send<SchemaBoardPage>('/board', { ...options, query: boardQuery(params) }),

    getLeagueBoard: (leagueSlug, params, options = {}) =>
      send<SchemaBoardPage>(`/leagues/${encode(leagueSlug)}/board`, {
        ...options,
        query: boardQuery(params),
      }),

    getEvent: (eventId, params, options = {}) =>
      send<SchemaEventDetail>(`/events/${encode(eventId)}`, {
        ...options,
        query: { book: params?.book },
      }),

    compareMarketPrices: (marketId, params, options = {}) =>
      send<SchemaMarketComparison>(`/markets/${encode(marketId)}/prices`, {
        ...options,
        query: { book: params?.book },
      }),

    getSelectionHistory: (selectionId, params, options = {}) =>
      send<SchemaHistorySeries>(`/selections/${encode(selectionId)}/history`, {
        ...options,
        query: {
          book: params.book,
          from: params.from,
          to: params.to,
          resolution: params.resolution,
          max_points: params.maxPoints,
        },
      }),

    searchEvents: (query, params, options = {}) =>
      send<SchemaSearchPage>('/search', {
        ...options,
        query: { q: query, limit: params?.limit, cursor: params?.cursor },
      }),

    listSports: (options = {}) => send<SchemaSportPage>('/sports', options),

    listLeaguesInSport: (sportSlug, options = {}) =>
      send<SchemaLeaguePage>(`/sports/${encode(sportSlug)}/leagues`, options),

    listBooks: (options = {}) => send<SchemaBookPage>('/books', options),

    register: (email, password, options = {}) =>
      send<SchemaSessionResponse>('/auth/register', {
        ...options,
        method: 'POST',
        body: { email, password },
      }),

    login: (email, password, totpCode, options = {}) =>
      send<SchemaSessionResponse>('/auth/login', {
        ...options,
        method: 'POST',
        // The key is omitted rather than sent as undefined: the account may have
        // no second factor, and `{"totp_code": null}` is a different request
        // from one that never mentioned it.
        body:
          totpCode === undefined || totpCode === ''
            ? { email, password }
            : { email, password, totp_code: totpCode },
      }),

    refresh: (refreshToken, options = {}) =>
      send<SchemaSessionResponse>('/auth/refresh', {
        ...options,
        method: 'POST',
        body: { refresh_token: refreshToken },
      }),

    logout: (refreshToken, options = {}) =>
      send<void>('/auth/logout', {
        ...options,
        method: 'POST',
        body: { refresh_token: refreshToken },
      }),

    getAccount: (options) => send<SchemaAccount>('/account', options),

    getBalance: (options) =>
      send<SchemaBalanceResponse>('/account/balance', options),

    quoteSlip: (body, options) =>
      send<SchemaSlipQuote>('/slip/quote', {
        ...options,
        method: 'POST',
        body,
      }),

    placeWager: (body, idempotencyKey, options) =>
      send<SchemaPlacement>('/wagers', {
        ...options,
        method: 'POST',
        body,
        headers: { 'Idempotency-Key': idempotencyKey },
      }),

    listWagers: (params, options) =>
      send<SchemaWagerPage>('/wagers', {
        ...options,
        query: {
          status: params?.status,
          limit: params?.limit,
          cursor: params?.cursor,
        },
      }),

    getWager: (wagerId, options) =>
      send<SchemaWager>(`/wagers/${encode(wagerId)}`, options),

    getCashOutQuote: (wagerId, options) =>
      send<SchemaCashOutQuote>(`/wagers/${encode(wagerId)}/cashout`, options),

    takeCashOut: (wagerId, acceptedValueMinor, idempotencyKey, options) =>
      send<SchemaWager>(`/wagers/${encode(wagerId)}/cashout`, {
        ...options,
        method: 'POST',
        body: { accepted_value_minor: acceptedValueMinor },
        headers: { 'Idempotency-Key': idempotencyKey },
      }),
  };
}

export { ApiError } from '@/lib/api/errors';
