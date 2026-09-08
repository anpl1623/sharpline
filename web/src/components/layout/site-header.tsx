/**
 * The application chrome. A Server Component — it does the catalogue read itself
 * rather than making every browser do it.
 *
 * 48px tall, `ground-1` on a `rule` hairline, and quiet: the header is the part
 * of the screen that must NOT compete with a moving price. Left to right —
 * wordmark, section nav, league strip, flexible space, search, book filter, odds
 * format, and below 768px the connection pip.
 *
 * # Sections and leagues are two different navigations
 *
 * `SectionNav` answers "which surface" — the board, the signals, the leaderboard
 * — and `LeagueNav` answers "which board". Folding the first into the second
 * would make "Signals" read as a competition somebody could bet on, and would
 * put the phase 9 analytics surface inside a strip that scrolls horizontally,
 * where it could be carried off screen by a catalogue with enough leagues in
 * it.
 *
 * # The wordmark is the only display-face element outside the landing poster
 *
 * DESIGN.md scopes Clash Grotesk narrowly, to "the landing poster and section
 * heads". One 18px wordmark is the whole standing allocation; anything else in
 * the display face inside the app is a drift.
 *
 * # Below 768px the strip moves to its own row
 *
 * At 375px the top row is already carrying a wordmark and four controls. Rather
 * than truncating league names into uselessness, the strip drops to a second
 * 36px row and scrolls horizontally there — the same treatment the board itself
 * gets under DESIGN.md's mobile section, for the same reason: horizontal scroll
 * is a worse gesture than vertical scroll, and it is still better than losing the
 * information.
 *
 * # Why the fetch is here and not in `league-nav.tsx`
 *
 * The strip needs `usePathname` for its active state, which forces it to be a
 * client component. The catalogue read is two in-network calls that belong on the
 * server. So the server half lives here and hands the flattened list down.
 *
 * `cache()` is what makes the two placements — inline at >= 768px, second row
 * below it — cost ONE round trip rather than two: both `LeagueStrip` instances
 * resolve against the same per-request memo. Each sits behind its own `Suspense`
 * boundary so a slow or unreachable API delays the strip and nothing else; the
 * rest of the shell, and the page under it, stream immediately.
 */

import { Suspense, cache } from 'react';
import Link from 'next/link';

import { AccountMenu } from '@/components/auth/account-menu';
import { BookFilter } from '@/components/layout/book-filter';
import { LeagueNav } from '@/components/layout/league-nav';
import type { LeagueNavItem } from '@/components/layout/league-nav';
import { OddsFormatToggle } from '@/components/layout/odds-format-toggle';
import { SearchBox } from '@/components/layout/search-box';
import { SectionNav } from '@/components/layout/section-nav';
import { StatusPip } from '@/components/layout/status-pip';
import { serverApi } from '@/lib/api/server';
import type { SchemaSportPage } from '@/lib/api/schema';

/**
 * Every league the API reports, flattened across sports and ordered by sport
 * then by the order the API returned them in.
 *
 * Failures are absorbed to an EMPTY LIST rather than thrown. The header is in the
 * root layout, so an unreachable catalogue would otherwise take down every page
 * in the application — including the ones that would have explained why. An empty
 * strip still offers "All", which reaches the board, which renders its own error
 * state from its own request. The failure is logged where a container log will
 * show it, and it is visible in the UI as the absence of leagues rather than as a
 * fabricated list.
 */
async function listSportsOrNull(): Promise<SchemaSportPage | null> {
  try {
    return await serverApi.listSports();
  } catch (error) {
    console.error('site-header: /sports failed', error);
    return null;
  }
}

const loadLeagues = cache(async (): Promise<readonly LeagueNavItem[]> => {
  const sports = await listSportsOrNull();
  if (sports === null) return [];

  const perSport = await Promise.all(
    sports.data.map(async (sport): Promise<readonly LeagueNavItem[]> => {
      try {
        const page = await serverApi.listLeaguesInSport(sport.slug);
        return page.data.map((league) => ({
          slug: league.slug,
          name: league.name,
          sportSlug: sport.slug,
          sportName: sport.name,
        }));
      } catch (error) {
        console.error(
          `site-header: /sports/${sport.slug}/leagues failed`,
          error,
        );
        return [];
      }
    }),
  );

  return perSport.flat();
});

async function LeagueStrip({ className }: { readonly className: string }) {
  const leagues = await loadLeagues();
  return <LeagueNav leagues={leagues} className={className} />;
}

/**
 * While the catalogue is in flight the strip renders with NO leagues — not with
 * placeholder pills. "All" is real and reaches a real board; a row of grey
 * lozenges would be an invented shape that resolves into a different one.
 */
function LeagueStripFallback({ className }: { readonly className: string }) {
  return <LeagueNav leagues={[]} className={className} />;
}

const INLINE_STRIP_CLASS = 'hidden h-full min-w-0 flex-1 md:block';
const ROW_STRIP_CLASS = 'h-9 min-w-0 flex-1 px-3';

export function SiteHeader() {
  return (
    <header className="sticky top-0 z-30 shrink-0 border-b border-rule bg-ground-1">
      <div className="flex h-12 items-stretch gap-2 px-3 sm:gap-3">
        <Link
          href="/"
          className="flex shrink-0 items-center t-h3 font-display tracking-[-0.01em] text-ink ui-transition hover:text-ink-2"
        >
          SHARPLINE
        </Link>

        {/* The three top-level sections sit between the wordmark and the league
          * strip, not inside it: a league answers "which board", and these are
          * different surfaces. They do not scroll — the strip beside them does,
          * so the sections are always reachable however many leagues ingest
          * discovers. Below 768px they move to the second row with the strip. */}
        <SectionNav className="hidden shrink-0 md:block" />

        <Suspense fallback={<LeagueStripFallback className={INLINE_STRIP_CLASS} />}>
          <LeagueStrip className={INLINE_STRIP_CLASS} />
        </Suspense>

        {/* Below 768px the strip is on its own row, so this holds the flexible
         * space open and keeps the controls hard right. */}
        <div className="flex-1 md:hidden" aria-hidden="true" />

        <div className="flex shrink-0 items-center gap-1.5 sm:gap-2">
          <SearchBox />
          <BookFilter />
          <OddsFormatToggle />
          {/* Sign-in lives at the end of the control cluster, after everything
              that acts on the board. `/login` and `/register` exist and are
              otherwise unreachable from the chrome; the component holds a fixed
              height in all three of its states so the header never reflows when
              the auth store rehydrates. */}
          <AccountMenu />
          <StatusPip className="md:hidden" />
        </div>
      </div>

      {/* The second row below 768px. The sections are a fixed leading group and
        * the league strip scrolls beside them, so the strip's horizontal scroll
        * can never carry "Signals" off the screen. */}
      <div className="flex items-stretch border-t border-rule md:hidden">
        <SectionNav className="h-9 shrink-0 pl-3" />
        <span className="my-2 w-px shrink-0 bg-rule" aria-hidden="true" />
        <Suspense fallback={<LeagueStripFallback className={ROW_STRIP_CLASS} />}>
          <LeagueStrip className={ROW_STRIP_CLASS} />
        </Suspense>
      </div>
    </header>
  );
}
