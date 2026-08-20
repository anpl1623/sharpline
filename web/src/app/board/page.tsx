/**
 * `/board` — the live odds board across every league.
 *
 * A SERVER component, deliberately. It fetches one page of `GET /board` through
 * the in-network service name with `cache: 'no-store'`, so the first paint is
 * real prices: no spinner, no client waterfall, and no window in which a viewer
 * sees an empty table that is about to fill. `LiveBoard` takes that page and the
 * socket keeps it current from there.
 *
 * # Why this route also reads the catalogue
 *
 * `EventSummary` names its league by ID and a `Price` names its book by SLUG,
 * and the board payload carries neither name. Three cheap catalogue reads —
 * `/sports`, `/sports/{slug}/leagues`, `/books` — turn those identifiers into
 * league headers a person can read, into the `league:{slug}` channels the stream
 * is subscribed by, and into the provenance sentence that says a synthetic
 * book's quote is generated rather than observed. Doing it here means the client
 * makes no catalogue request at all.
 *
 * The catalogue is decoration for the board and not its content, so a catalogue
 * failure degrades (ids instead of names, no provenance line) rather than
 * refusing to render prices that were fetched perfectly well.
 *
 * # Nothing on this page is invented
 *
 * Every event, market, selection and price below travelled provider → ingest →
 * Kafka → pricer → Postgres → api → here. An empty board is a correct board.
 */

import type { Metadata } from 'next';

import { serverApi } from '@/lib/api/server';
import type { SchemaBoardPage } from '@/lib/api/schema';
import { BoardUnavailable } from '@/components/board/board-empty';
import {
  boardHref,
  parseBoardWindow,
  parseLiveOnly,
  startingBeforeFor,
} from '@/components/board/board-toolbar';
import { LiveBoard } from '@/components/board/live-board';
import type {
  BoardBookView,
  BoardCatalogue,
  BoardLeagueView,
} from '@/components/board/use-board-live';

/**
 * The API's own default page size. Sent explicitly so the value this route used
 * is the value the client's follow-up pages use — a cursor is bound to the
 * filters it was minted under.
 */
const PAGE_LIMIT = 50;

const BASE_PATH = '/board';

export const dynamic = 'force-dynamic';

export const metadata: Metadata = {
  title: 'Live board',
  description:
    'Live odds across every league, streamed from Sharpline’s own pricing pipeline.',
};

const NO_CATALOGUE: BoardCatalogue = {
  leaguesById: {},
  booksBySlug: {},
  bookCount: 0,
  allSynthetic: false,
  anySynthetic: false,
};

/**
 * Leagues by id and books by slug.
 *
 * Duplicated in the single-league route rather than shared: the only module both
 * routes could import it from is one of the board's client modules, and a client
 * module's exports are references a server render cannot call. Fourteen lines of
 * fetch are the cheaper half of that trade.
 */
async function loadBoardCatalogue(): Promise<BoardCatalogue> {
  try {
    const [sports, books] = await Promise.all([
      serverApi.listSports(),
      serverApi.listBooks(),
    ]);
    const leaguePages = await Promise.all(
      sports.data.map((sport) => serverApi.listLeaguesInSport(sport.slug)),
    );

    const sportNames = new Map<string, string>();
    for (const sport of sports.data) sportNames.set(sport.id, sport.name);

    const leaguesById: Record<string, BoardLeagueView> = {};
    for (const leaguePage of leaguePages) {
      for (const league of leaguePage.data) {
        leaguesById[league.id] = {
          id: league.id,
          slug: league.slug,
          name: league.name,
          sportName: sportNames.get(league.sport_id) ?? null,
        };
      }
    }

    const booksBySlug: Record<string, BoardBookView> = {};
    let synthetic = 0;
    for (const book of books.data) {
      booksBySlug[book.slug] = {
        slug: book.slug,
        name: book.name,
        kind: book.kind,
        isReference: book.is_reference,
      };
      if (book.kind === 'synthetic') synthetic += 1;
    }

    return {
      leaguesById,
      booksBySlug,
      bookCount: books.data.length,
      allSynthetic: books.data.length > 0 && synthetic === books.data.length,
      anySynthetic: synthetic > 0,
    };
  } catch {
    return NO_CATALOGUE;
  }
}

type BoardResult =
  | { readonly ok: true; readonly page: SchemaBoardPage }
  | { readonly ok: false; readonly error: unknown };

async function fetchBoard(startingBefore: string): Promise<BoardResult> {
  try {
    return {
      ok: true,
      page: await serverApi.getBoard({ startingBefore, limit: PAGE_LIMIT }),
    };
  } catch (error) {
    return { ok: false, error };
  }
}

interface BoardRouteProps {
  readonly searchParams: Promise<Record<string, string | string[] | undefined>>;
}

export default async function BoardPage({ searchParams }: BoardRouteProps) {
  const params = await searchParams;
  const windowId = parseBoardWindow(params['window']);
  const liveOnly = parseLiveOnly(params['live']);
  const startingBefore = startingBeforeFor(windowId, new Date());
  const here = boardHref(BASE_PATH, { window: windowId, liveOnly });

  const [catalogue, result] = await Promise.all([
    loadBoardCatalogue(),
    fetchBoard(startingBefore),
  ]);

  return (
    <section aria-labelledby="board-heading" className="flex w-full flex-col">
      <header className="flex flex-col gap-1 px-4 pb-3 pt-6">
        <h1 id="board-heading" className="t-h3 font-display text-ink">
          Live board
        </h1>
        <p className="t-body text-ink-muted">
          Every league, soonest first. Prices are streamed as they move.
        </p>
      </header>

      {result.ok ? (
        <LiveBoard
          initialData={result.page}
          catalogue={catalogue}
          basePath={BASE_PATH}
          window={windowId}
          liveOnly={liveOnly}
          startingBefore={startingBefore}
          limit={PAGE_LIMIT}
          leagueSlug={null}
          leagueName={null}
        />
      ) : (
        <BoardUnavailable error={result.error} retryHref={here} />
      )}
    </section>
  );
}
