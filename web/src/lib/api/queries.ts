/**
 * Query keys and query options.
 *
 * Every REST read a client component performs goes through a factory here, for
 * two reasons: the key for a given read is spelled once, so an invalidation
 * cannot miss a cache entry because two call sites disagreed about the shape of
 * a key; and the per-endpoint staleness policy lives beside the endpoint rather
 * than being repeated at every `useQuery`.
 *
 * # Two staleness policies, and the line between them
 *
 * MARKET DATA (board, event, comparison) is `staleTime: 0`. It ticks, and the
 * WebSocket is what keeps it current — the REST read establishes the board and
 * the stream maintains it. There is NO polling anywhere in this file; see
 * `query/provider.tsx` for why a poll beside the socket would double-render the
 * same change.
 *
 * CATALOGUE DATA (sports, leagues, books) is cached for minutes. A league does
 * not get renamed while somebody is looking at it, and refetching the book list
 * on every focus is a request that can only ever return the same answer.
 *
 * # Keys never contain a credential
 *
 * The account key is `['sharpline', 'account']` and does not include the access
 * token. A query key is visible in devtools and is retained in the cache; a
 * token in one would be a credential sitting in a place nobody thinks of as
 * storage.
 */

import { infiniteQueryOptions, queryOptions } from '@tanstack/react-query';

import { browserApi } from '@/lib/api/client';
import type {
  BoardParams,
  BookFilterParams,
  HistoryParams,
  SearchParams,
  WagerListParams,
} from '@/lib/api/client';
import type { SchemaSlipQuoteRequest } from '@/lib/api/schema';

/** Catalogue reads are cached this long. They do not tick. */
export const CATALOGUE_STALE_TIME_MS = 5 * 60 * 1_000;

/**
 * The API rejects a shorter query, and a one-character prefix would match most
 * of the slate anyway. The factory reports `enabled: false` below the bound
 * rather than issuing a request that is a guaranteed 400.
 */
export const MIN_SEARCH_LENGTH = 2;

// -----------------------------------------------------------------------------
// Key parts
// -----------------------------------------------------------------------------

interface BoardKeyPart {
  readonly startingBefore: string | null;
  readonly limit: number | null;
  readonly book: readonly string[];
}

/**
 * The part of the board parameters that identifies WHICH board. The cursor is
 * excluded because it identifies a PAGE of that board, and an infinite query
 * carries it as a page parameter instead.
 *
 * The book list is sorted, so selecting two books in either order produces one
 * cache entry rather than two.
 */
function boardKeyPart(params?: BoardParams): BoardKeyPart {
  return {
    startingBefore: params?.startingBefore ?? null,
    limit: params?.limit ?? null,
    book: [...(params?.book ?? [])].sort(),
  };
}

function bookKeyPart(params?: BookFilterParams): readonly string[] {
  return [...(params?.book ?? [])].sort();
}

/**
 * The part of the wager parameters that identifies WHICH list. The cursor is
 * excluded for the same reason the board's is: it identifies a page, not a list.
 *
 * The status set is sorted so "open then placed" and "placed then open" are one
 * cache entry. It is NOT de-duplicated here — a repeated status is a caller bug
 * that should be visible as a distinct key rather than quietly folded away.
 */
function wagerKeyPart(params?: WagerListParams): Record<string, unknown> {
  return {
    status: [...(params?.status ?? [])].sort(),
    limit: params?.limit ?? null,
  };
}

function historyKeyPart(params: HistoryParams): Record<string, unknown> {
  return {
    book: params.book,
    from: params.from,
    to: params.to ?? null,
    resolution: params.resolution ?? null,
    maxPoints: params.maxPoints ?? null,
  };
}

// -----------------------------------------------------------------------------
// The key factory
// -----------------------------------------------------------------------------

export const queryKeys = {
  /** Invalidating this drops every cached read in the app. */
  root: ['sharpline'] as const,

  board: (params?: BoardParams) =>
    ['sharpline', 'board', boardKeyPart(params), params?.cursor ?? null] as const,
  boardInfinite: (params?: BoardParams) =>
    ['sharpline', 'board', 'infinite', boardKeyPart(params)] as const,

  leagueBoard: (leagueSlug: string, params?: BoardParams) =>
    [
      'sharpline',
      'league-board',
      leagueSlug,
      boardKeyPart(params),
      params?.cursor ?? null,
    ] as const,
  leagueBoardInfinite: (leagueSlug: string, params?: BoardParams) =>
    [
      'sharpline',
      'league-board',
      'infinite',
      leagueSlug,
      boardKeyPart(params),
    ] as const,

  event: (eventId: string, params?: BookFilterParams) =>
    ['sharpline', 'event', eventId, bookKeyPart(params)] as const,

  marketComparison: (marketId: string, params?: BookFilterParams) =>
    ['sharpline', 'market-comparison', marketId, bookKeyPart(params)] as const,

  selectionHistory: (selectionId: string, params: HistoryParams) =>
    [
      'sharpline',
      'selection-history',
      selectionId,
      historyKeyPart(params),
    ] as const,

  search: (query: string, params?: SearchParams) =>
    [
      'sharpline',
      'search',
      query,
      params?.limit ?? null,
      params?.cursor ?? null,
    ] as const,

  sports: () => ['sharpline', 'sports'] as const,
  leagues: (sportSlug: string) => ['sharpline', 'leagues', sportSlug] as const,
  books: () => ['sharpline', 'books'] as const,

  /** No token in the key. See the file comment. */
  account: () => ['sharpline', 'account'] as const,

  /**
   * The derived balances.
   *
   * Its own key rather than a branch of `account`, because it is invalidated on
   * a completely different event: placing a wager moves money and does not
   * touch the profile, and settling one moves money with no request from this
   * client at all.
   */
  balance: () => ['sharpline', 'balance'] as const,

  wagers: (params?: WagerListParams) =>
    ['sharpline', 'wagers', wagerKeyPart(params), params?.cursor ?? null] as const,
  wagersInfinite: (params?: WagerListParams) =>
    ['sharpline', 'wagers', 'infinite', wagerKeyPart(params)] as const,
  wager: (wagerId: string) => ['sharpline', 'wager', wagerId] as const,
  cashOutQuote: (wagerId: string) =>
    ['sharpline', 'cash-out-quote', wagerId] as const,

  /**
   * The priced slip.
   *
   * Keyed by the slip's whole CANONICAL request body, which is what makes the
   * cadence right: the quote refetches when the customer changes something —
   * a leg, the stake, the kind, an acceptance — and not when a price ticks.
   * Movement is detected over the WebSocket, which costs nothing; see
   * `components/slip/use-slip-quote.ts` for why the two are separated.
   */
  slipQuote: (body: SchemaSlipQuoteRequest) =>
    ['sharpline', 'slip-quote', body] as const,
} as const;

// -----------------------------------------------------------------------------
// Market data — staleTime 0
// -----------------------------------------------------------------------------

/** One page of the board. */
export function boardQueryOptions(params?: BoardParams) {
  return queryOptions({
    queryKey: queryKeys.board(params),
    queryFn: ({ signal }) => browserApi.getBoard(params, { signal }),
    staleTime: 0,
  });
}

/**
 * The board as a keyset-paginated infinite query.
 *
 * There is no total count and there never will be, so there is no page number
 * and no "page 3 of 12". `has_more` and `next_cursor` are the whole contract:
 * fetch until `has_more` is false.
 */
export function boardInfiniteQueryOptions(params?: BoardParams) {
  return infiniteQueryOptions({
    queryKey: queryKeys.boardInfinite(params),
    queryFn: ({ pageParam, signal }) =>
      browserApi.getBoard(
        {
          startingBefore: params?.startingBefore,
          limit: params?.limit,
          book: params?.book,
          cursor: pageParam,
        },
        { signal },
      ),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) =>
      lastPage.page.has_more ? (lastPage.page.next_cursor ?? undefined) : undefined,
    staleTime: 0,
  });
}

/** One page of a single league's board. */
export function leagueBoardQueryOptions(
  leagueSlug: string,
  params?: BoardParams,
) {
  return queryOptions({
    queryKey: queryKeys.leagueBoard(leagueSlug, params),
    queryFn: ({ signal }) =>
      browserApi.getLeagueBoard(leagueSlug, params, { signal }),
    staleTime: 0,
    enabled: leagueSlug !== '',
  });
}

export function leagueBoardInfiniteQueryOptions(
  leagueSlug: string,
  params?: BoardParams,
) {
  return infiniteQueryOptions({
    queryKey: queryKeys.leagueBoardInfinite(leagueSlug, params),
    queryFn: ({ pageParam, signal }) =>
      browserApi.getLeagueBoard(
        leagueSlug,
        {
          startingBefore: params?.startingBefore,
          limit: params?.limit,
          book: params?.book,
          cursor: pageParam,
        },
        { signal },
      ),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) =>
      lastPage.page.has_more ? (lastPage.page.next_cursor ?? undefined) : undefined,
    staleTime: 0,
    enabled: leagueSlug !== '',
  });
}

/** One event and its full market tree. */
export function eventQueryOptions(eventId: string, params?: BookFilterParams) {
  return queryOptions({
    queryKey: queryKeys.event(eventId, params),
    queryFn: ({ signal }) => browserApi.getEvent(eventId, params, { signal }),
    staleTime: 0,
    enabled: eventId !== '',
  });
}

/**
 * Every book's price on one market, plus the best price on each selection.
 *
 * `best` is computed server-side rather than here so that "best" means one thing
 * across every surface in the product.
 */
export function marketComparisonQueryOptions(
  marketId: string,
  params?: BookFilterParams,
) {
  return queryOptions({
    queryKey: queryKeys.marketComparison(marketId, params),
    queryFn: ({ signal }) =>
      browserApi.compareMarketPrices(marketId, params, { signal }),
    staleTime: 0,
    enabled: marketId !== '',
  });
}

/**
 * A selection's line movement at ONE book.
 *
 * `book` and `from` are required by the API. A window that would exceed
 * `maxPoints` is rejected with a 422 and never truncated, so a caller widening
 * the window must also raise the resolution rather than assume the server will
 * thin the series.
 *
 * Longer staleTime than the live surfaces: history is append-only and the tail
 * of the chart moves at the resolution's cadence, not at the tick's.
 */
export function selectionHistoryQueryOptions(
  selectionId: string,
  params: HistoryParams,
) {
  return queryOptions({
    queryKey: queryKeys.selectionHistory(selectionId, params),
    queryFn: ({ signal }) =>
      browserApi.getSelectionHistory(selectionId, params, { signal }),
    staleTime: 30 * 1_000,
    enabled: selectionId !== '' && params.book !== '' && params.from !== '',
  });
}

/**
 * Event search. PREFIX match on competitor name; the status set is narrower than
 * the board's — scheduled and live only — so a settled game is not findable here
 * and that is correct.
 */
export function searchQueryOptions(query: string, params?: SearchParams) {
  const trimmed = query.trim();
  return queryOptions({
    queryKey: queryKeys.search(trimmed, params),
    queryFn: ({ signal }) =>
      browserApi.searchEvents(trimmed, params, { signal }),
    staleTime: 15 * 1_000,
    enabled: trimmed.length >= MIN_SEARCH_LENGTH,
  });
}

// -----------------------------------------------------------------------------
// Catalogue — cached
// -----------------------------------------------------------------------------

export function sportsQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.sports(),
    queryFn: ({ signal }) => browserApi.listSports({ signal }),
    staleTime: CATALOGUE_STALE_TIME_MS,
  });
}

export function leaguesQueryOptions(sportSlug: string) {
  return queryOptions({
    queryKey: queryKeys.leagues(sportSlug),
    queryFn: ({ signal }) =>
      browserApi.listLeaguesInSport(sportSlug, { signal }),
    staleTime: CATALOGUE_STALE_TIME_MS,
    enabled: sportSlug !== '',
  });
}

/**
 * The book catalogue. `is_reference` names the sharp book the pricer devigs
 * against — every EV number in this system is relative to it, so a surface that
 * renders EV must be able to name it — and `kind: "synthetic"` marks a book
 * whose quotes are computed by this system rather than observed from a real
 * bookmaker. Both must be renderable.
 */
export function booksQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.books(),
    queryFn: ({ signal }) => browserApi.listBooks({ signal }),
    staleTime: CATALOGUE_STALE_TIME_MS,
  });
}

// -----------------------------------------------------------------------------
// Account
// -----------------------------------------------------------------------------

/**
 * The signed-in profile. Disabled while anonymous, so an unauthenticated page
 * does not issue a request that can only 401.
 */
export function accountQueryOptions(accessToken: string | null) {
  return queryOptions({
    queryKey: queryKeys.account(),
    queryFn: ({ signal }) =>
      browserApi.getAccount({ accessToken: accessToken ?? undefined, signal }),
    staleTime: 60 * 1_000,
    enabled: accessToken !== null && accessToken !== '',
  });
}

// -----------------------------------------------------------------------------
// Betting
// -----------------------------------------------------------------------------

/**
 * Whether a credential is present. Every factory below is disabled without one:
 * these endpoints answer 401 to an anonymous caller, and issuing a request whose
 * only possible outcome is a 401 turns a signed-out page into an error state.
 */
function signedIn(accessToken: string | null): boolean {
  return accessToken !== null && accessToken !== '';
}

/**
 * The spendable and escrowed balances.
 *
 * `staleTime: 0` because the number moves for reasons this client never sees:
 * a settlement, a void, a grant. Unlike the board there is no socket carrying
 * it, so the honest policy is "re-read it whenever something asks", and the
 * things that ask are a mounted slip and a wager list.
 */
export function balanceQueryOptions(accessToken: string | null) {
  return queryOptions({
    queryKey: queryKeys.balance(),
    queryFn: ({ signal }) =>
      browserApi.getBalance({ accessToken: accessToken ?? undefined, signal }),
    staleTime: 0,
    enabled: signedIn(accessToken),
  });
}

/**
 * The wager history, keyset-paginated on `(placed_at, id)`.
 *
 * `placed_at` alone is not a total order — a round robin writes N tickets at one
 * instant — so the identifier is part of the key and a cursor is unambiguous.
 *
 * The status filter is applied to the page the server SCANNED, so a filtered
 * page can hold fewer than `limit` rows while `has_more` is still true. Callers
 * must follow `next_cursor` until `has_more` is false rather than stopping at a
 * short page; `getNextPageParam` below reads `has_more` and not the row count
 * for exactly that reason.
 */
export function wagersInfiniteQueryOptions(
  accessToken: string | null,
  params?: WagerListParams,
) {
  return infiniteQueryOptions({
    queryKey: queryKeys.wagersInfinite(params),
    queryFn: ({ pageParam, signal }) =>
      browserApi.listWagers(
        {
          status: params?.status,
          limit: params?.limit,
          cursor: pageParam,
        },
        { accessToken: accessToken ?? undefined, signal },
      ),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) =>
      lastPage.page.has_more ? (lastPage.page.next_cursor ?? undefined) : undefined,
    staleTime: 0,
    enabled: signedIn(accessToken),
  });
}

/**
 * One ticket with its legs.
 *
 * A running ticket's legs grade at different times and its status changes with
 * no request from here, so this does not cache. Somebody else's wager id is a
 * 404 and not a 403 — the ownership comparison happens on the row as it is read,
 * so there is no branch anywhere that could tell the two apart, which is the
 * point: a 403 would confirm the id exists.
 */
export function wagerQueryOptions(accessToken: string | null, wagerId: string) {
  return queryOptions({
    queryKey: queryKeys.wager(wagerId),
    queryFn: ({ signal }) =>
      browserApi.getWager(wagerId, {
        accessToken: accessToken ?? undefined,
        signal,
      }),
    staleTime: 0,
    enabled: signedIn(accessToken) && wagerId !== '',
  });
}

/**
 * What the book will pay to close a ticket now.
 *
 * `enabled` is passed in rather than derived, because the caller knows something
 * this factory cannot: a cash-out quote should be fetched when the customer ASKS
 * for one, not on every render of a wager row. Quoting a page of open tickets
 * eagerly would be a request per row for a number most of them will never show.
 *
 * `staleTime: 0` and no `refetchInterval`. The quote is a SNAPSHOT at
 * `quoted_at`, not an offer held open — the spec is explicit that there is no
 * expiry field because an expiry would imply the book stands behind the number
 * until then. Polling it would animate a number the book is not committed to;
 * the customer refreshes it deliberately, and whatever takes the cash-out
 * re-prices while holding the wager row anyway.
 */
export function cashOutQuoteQueryOptions(
  accessToken: string | null,
  wagerId: string,
  enabled: boolean,
) {
  return queryOptions({
    queryKey: queryKeys.cashOutQuote(wagerId),
    queryFn: ({ signal }) =>
      browserApi.getCashOutQuote(wagerId, {
        accessToken: accessToken ?? undefined,
        signal,
      }),
    staleTime: 0,
    gcTime: 0,
    // A 409 `cash_out_unavailable` is a STATE, not a fault — the ticket is
    // terminal, a leg is void, a reference price went stale — and retrying it
    // would just ask the same question again. `ApiError.isRetryable` already
    // says no to a 4xx; this is here so the intent is legible at the endpoint.
    retry: false,
    enabled: enabled && signedIn(accessToken) && wagerId !== '',
  });
}
