'use client';

/**
 * The board's client half: the WebSocket subscription, the keyset pager, and the
 * one live region the whole surface is allowed.
 *
 * # The first paint is server-rendered and this component never blanks it
 *
 * `initialData` is a real page of prices fetched by the route. There is no
 * loading state on mount, no client waterfall, and no refetch: the socket takes
 * over from here and updates prices in place. If the gateway is unreachable the
 * prices on screen stay on screen, they stop moving, and the connection state
 * says so — which is the honest rendering of that situation and the one a viewer
 * can act on.
 *
 * # What this component deliberately does not own
 *
 * NOT the live region. The app shell mounts exactly one `aria-live` region for
 * market movement (`components/live/live-announcer.tsx`), throttled to one
 * batched sentence every five seconds. A board that mounted a second would make
 * a screen reader say everything twice.
 *
 * NOT the connection state. The shell's 24px status rail reports it permanently
 * on every page and collapses to the pip below 768px. Reading it here would also
 * re-render the board at the frame rate, since the stream's state object is
 * replaced on every frame — the exact cost the per-cell subscriptions exist to
 * avoid.
 *
 * NOT the board's staleness. The rail derives that from the stream and only
 * from the stream. A board-published median was considered and the channel for
 * it removed: the only staleness this component can compute is REST
 * `observed_at` against `BoardPage.as_of`, and both are frozen at page
 * assembly, so it would sit unchanged while ingestion stalled — masking the one
 * failure the staleness SLO exists to surface.
 *
 * # The pager
 *
 * `starting_before` and `limit` arrive as props rather than being recomputed
 * here. A cursor is bound to the filters it was minted under and is rejected
 * with a 400 if presented with different ones, so the follow-up page must send
 * the byte-identical upper bound the first page was fetched with — and a
 * client-side `new Date()` would not be it.
 */

import { useCallback, useMemo, useState } from 'react';

import { browserApi } from '@/lib/api/client';
import { userFacingMessage } from '@/lib/api/errors';
import type { SchemaBoardPage } from '@/lib/api/schema';
import { useBookFilter } from '@/lib/store/preferences';
import { BoardEmpty } from './board-empty';
import { BoardTable } from './board-table';
import {
  BoardPagination,
  BoardToolbar,
  boardHref,
  boardWindowOption,
  widerBoardWindow,
} from './board-toolbar';
import type { BoardWindowId } from './board-toolbar';
import {
  boardChannels,
  filterLiveOnly,
  groupEntriesByLeague,
  priceBasisNote,
  provenanceNote,
  useBoardChannels,
} from './use-board-live';
import type { BoardCatalogue } from './use-board-live';

export interface LiveBoardProps {
  /** The route's own fetch. Real prices, already rendered on the server. */
  readonly initialData: SchemaBoardPage;
  readonly catalogue: BoardCatalogue;
  /** `/board` or `/board/{slug}` — what the window and status links point at. */
  readonly basePath: string;
  readonly window: BoardWindowId;
  readonly liveOnly: boolean;
  /** The exact RFC 3339 bound the first page was fetched with. */
  readonly startingBefore: string;
  readonly limit: number;
  /** Non-null on the single-league route. */
  readonly leagueSlug: string | null;
  readonly leagueName: string | null;
}

export function LiveBoard({
  initialData,
  catalogue,
  basePath,
  window: windowId,
  liveOnly,
  startingBefore,
  limit,
  leagueSlug,
  leagueName,
}: LiveBoardProps) {
  const bookFilter = useBookFilter();

  // Every server render of this route mints a fresh `starting_before`, so this
  // key changes whenever the route re-fetches and the accumulated pages have to
  // be dropped: they were paged against a bound that no longer applies.
  const resetKey = `${basePath}|${windowId}|${String(liveOnly)}|${startingBefore}`;

  const [pages, setPages] = useState<readonly SchemaBoardPage[]>(() => [initialData]);
  const [pagesKey, setPagesKey] = useState(resetKey);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);

  // Adjusting state during render — React's own idiom for state derived from a
  // prop, and cheaper than an effect that would render the stale pages once
  // before correcting them.
  if (pagesKey !== resetKey) {
    setPagesKey(resetKey);
    setPages([initialData]);
    setLoading(false);
    setLoadError(null);
  }

  const lastPage = pages[pages.length - 1];
  const hasMore = lastPage?.page.has_more ?? false;
  const nextCursor = lastPage?.page.next_cursor ?? null;

  const loadMore = useCallback(() => {
    if (nextCursor === null || nextCursor === '') return;
    setLoading(true);
    setLoadError(null);

    const params = { startingBefore, limit, cursor: nextCursor };
    const request =
      leagueSlug === null
        ? browserApi.getBoard(params)
        : browserApi.getLeagueBoard(leagueSlug, params);

    request.then(
      (page) => {
        setPages((previous) => [...previous, page]);
        setLoading(false);
      },
      (error: unknown) => {
        setLoadError(userFacingMessage(error));
        setLoading(false);
      },
    );
  }, [nextCursor, startingBefore, limit, leagueSlug]);

  const loadedEntries = useMemo(() => pages.flatMap((page) => page.data), [pages]);
  const entries = useMemo(
    () => filterLiveOnly(loadedEntries, liveOnly),
    [loadedEntries, liveOnly],
  );
  const groups = useMemo(
    () => groupEntriesByLeague(entries, catalogue),
    [entries, catalogue],
  );

  const channels = useMemo(
    () => boardChannels(leagueSlug, loadedEntries, catalogue),
    [leagueSlug, loadedEntries, catalogue],
  );
  useBoardChannels(channels);

  const windowOption = boardWindowOption(windowId);
  const wider = widerBoardWindow(windowId);

  const scope = leagueName === null ? '' : ` in ${leagueName}`;
  const caption =
    `Live odds board. ${String(entries.length)} events${scope} starting within ` +
    `${windowOption.phrase}, grouped by league, with moneyline, spread and total ` +
    `prices. Prices update live; a summary of what moved is announced every five seconds.`;

  return (
    <>
      <BoardToolbar
        basePath={basePath}
        window={windowId}
        liveOnly={liveOnly}
        shownCount={entries.length}
        loadedCount={loadedEntries.length}
        hasMore={hasMore}
        priceBasis={priceBasisNote(catalogue, bookFilter)}
        provenance={provenanceNote(catalogue)}
      />

      {groups.length === 0 ? (
        <BoardEmpty
          windowPhrase={windowOption.phrase}
          liveOnly={liveOnly}
          loadedCount={loadedEntries.length}
          showAllHref={boardHref(basePath, { window: windowId, liveOnly: false })}
          widerHref={
            wider === null ? null : boardHref(basePath, { window: wider, liveOnly })
          }
          widerLabel={wider === null ? null : boardWindowOption(wider).label}
          bookFilterCount={bookFilter.length}
          leagueName={leagueName}
        />
      ) : (
        <BoardTable
          groups={groups}
          catalogue={catalogue}
          bookFilter={bookFilter}
          caption={caption}
        />
      )}

      <BoardPagination
        hasMore={hasMore}
        loading={loading}
        error={loadError}
        onLoadMore={loadMore}
      />
    </>
  );
}
