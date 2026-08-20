/**
 * `/board/{leagueSlug}` — one league's board.
 *
 * The same server-rendered shape as `/board`, narrowed to one league. The URL
 * segment IS the stream channel: `league:{slug}` is keyed by slug rather than by
 * id precisely so that the route and its subscription are the same string, which
 * means the board keeps streaming even when the league currently has nothing on
 * it — a viewer waiting for the first event of the evening watches it appear.
 *
 * An unknown slug is a 404 from the API and a 404 here. It is NOT an empty
 * board: "this league has no events in this window" and "this league does not
 * exist" are different facts, and rendering the first when the second is true
 * would send a viewer looking for events that were never going to arrive.
 *
 * The catalogue read is duplicated from `/board` rather than shared. See the
 * note on that route: the only module both could import it from is one of the
 * board's client modules, whose exports a server render cannot call.
 */

import type { Metadata } from 'next';
import { notFound } from 'next/navigation';

import { serverApi } from '@/lib/api/server';
import { isApiError } from '@/lib/api/errors';
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

const PAGE_LIMIT = 50;

export const dynamic = 'force-dynamic';

export const metadata: Metadata = {
  title: 'League board',
  description:
    'Live odds for one league, streamed from Sharpline’s own pricing pipeline.',
};

const NO_CATALOGUE: BoardCatalogue = {
  leaguesById: {},
  booksBySlug: {},
  bookCount: 0,
  allSynthetic: false,
  anySynthetic: false,
};

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

function leagueBySlug(
  catalogue: BoardCatalogue,
  slug: string,
): BoardLeagueView | null {
  for (const league of Object.values(catalogue.leaguesById)) {
    if (league.slug === slug) return league;
  }
  return null;
}

type BoardResult =
  | { readonly ok: true; readonly page: SchemaBoardPage }
  | { readonly ok: false; readonly error: unknown };

async function fetchLeagueBoard(
  slug: string,
  startingBefore: string,
): Promise<BoardResult> {
  try {
    return {
      ok: true,
      page: await serverApi.getLeagueBoard(slug, {
        startingBefore,
        limit: PAGE_LIMIT,
      }),
    };
  } catch (error) {
    return { ok: false, error };
  }
}

interface LeagueBoardRouteProps {
  readonly params: Promise<{ league: string }>;
  readonly searchParams: Promise<Record<string, string | string[] | undefined>>;
}

export default async function LeagueBoardPage({
  params,
  searchParams,
}: LeagueBoardRouteProps) {
  const { league: leagueSlug } = await params;
  const query = await searchParams;
  const windowId = parseBoardWindow(query['window']);
  const liveOnly = parseLiveOnly(query['live']);
  const startingBefore = startingBeforeFor(windowId, new Date());

  const basePath = `/board/${encodeURIComponent(leagueSlug)}`;
  const here = boardHref(basePath, { window: windowId, liveOnly });

  const [catalogue, result] = await Promise.all([
    loadBoardCatalogue(),
    fetchLeagueBoard(leagueSlug, startingBefore),
  ]);

  // `notFound()` throws, so it is called outside the fetch's own try/catch —
  // swallowing it there would turn "no such league" back into a generic error.
  if (!result.ok && isApiError(result.error) && result.error.status === 404) {
    notFound();
  }

  const league = leagueBySlug(catalogue, leagueSlug);
  const heading = league?.name ?? leagueSlug;

  return (
    <section aria-labelledby="board-heading" className="flex w-full flex-col">
      <header className="flex flex-col gap-1 px-4 pb-3 pt-6">
        <h1 id="board-heading" className="t-h3 font-display text-ink">
          {heading}
        </h1>
        <p className="t-body text-ink-muted">
          {league?.sportName === null || league?.sportName === undefined
            ? 'Prices are streamed as they move.'
            : `${league.sportName} · prices are streamed as they move.`}
        </p>
      </header>

      {result.ok ? (
        <LiveBoard
          initialData={result.page}
          catalogue={catalogue}
          basePath={basePath}
          window={windowId}
          liveOnly={liveOnly}
          startingBefore={startingBefore}
          limit={PAGE_LIMIT}
          leagueSlug={leagueSlug}
          leagueName={league?.name ?? null}
        />
      ) : (
        <BoardUnavailable error={result.error} retryHref={here} />
      )}
    </section>
  );
}
