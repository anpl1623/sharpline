'use client';

/**
 * The league strip: "All", then one entry per league the API actually reports.
 *
 * # Why this file is a client component when its data is fetched on the server
 *
 * The catalogue read belongs on the server — it is two cached, in-network calls
 * and there is no reason to make a browser do them. `site-header.tsx` owns that
 * fetch and hands the result down as a prop. What CANNOT be done on the server is
 * the active state: `usePathname` is a client hook, and a layout is not given the
 * pathname. So the split is data on the server, highlight on the client, and this
 * module is the client half.
 *
 * # Nothing here is hardcoded
 *
 * There is no league name, slug or count in this file. The synthetic provider
 * currently produces two leagues; it could produce twenty tomorrow, or none, and
 * this renders whatever `/sports/{slug}/leagues` returns. An empty catalogue
 * renders "All" alone, which is the correct board navigation for a system with no
 * leagues in it — not a defect to paper over.
 *
 * # Channels are keyed by slug, and so is this
 *
 * The WebSocket channel for a league is `league:{slug}`, deliberately, so that a
 * board's URL and its subscription are the same string. `/board/{slug}` keeps
 * that property visible in the address bar.
 */

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import type { ReactNode } from 'react';

import { cn } from '@/lib/utils';

/** The whole board, unfiltered. */
export const ALL_LEAGUES_HREF = '/board';

/** One league's board. */
export function leagueBoardHref(slug: string): string {
  return `/board/${encodeURIComponent(slug)}`;
}

/**
 * The shape the header hands down. Flattened from `Sport` + `League` so this
 * component never has to know how the catalogue is paginated.
 */
export interface LeagueNavItem {
  readonly slug: string;
  readonly name: string;
  readonly sportSlug: string;
  readonly sportName: string;
}

export interface LeagueNavProps {
  readonly leagues: readonly LeagueNavItem[];
  readonly className?: string | undefined;
}

export function LeagueNav({ leagues, className }: LeagueNavProps) {
  const pathname = usePathname();
  const activeSlug = leagueSlugFromPathname(pathname);
  const allActive = pathname === ALL_LEAGUES_HREF;

  return (
    <nav
      aria-label="Leagues"
      className={cn('min-w-0 self-stretch', className)}
    >
      {/* The strip scrolls rather than wrapping, because the header is a fixed
        * 48px and a wrapping nav would change the height of the whole chrome as
        * ingest discovers a league. The scrollbar is hidden — a 15px scrollbar in
        * a 48px header is louder than the thing it describes — so the trailing
        * fade is the ONLY affordance saying more exists. Without it the last
        * league is hard-clipped mid-word against the search box and reads as a
        * layout bug rather than as a scroll. */}
      <ul
        className={cn(
          'flex h-full items-stretch gap-0.5 overflow-x-auto',
          '[scrollbar-width:none] [&::-webkit-scrollbar]:hidden',
          '[mask-image:linear-gradient(to_right,black_calc(100%-24px),transparent)]',
        )}
      >
        <li className="flex shrink-0">
          <NavLink href={ALL_LEAGUES_HREF} active={allActive}>
            All
          </NavLink>
        </li>
        {leagues.map((league) => (
          <li key={league.slug} className="flex shrink-0">
            <NavLink
              href={leagueBoardHref(league.slug)}
              active={activeSlug === league.slug}
              title={`${league.name} — ${league.sportName}`}
            >
              {league.name}
            </NavLink>
          </li>
        ))}
      </ul>
    </nav>
  );
}

function NavLink({
  href,
  active,
  title,
  children,
}: {
  readonly href: string;
  readonly active: boolean;
  readonly title?: string | undefined;
  readonly children: ReactNode;
}) {
  return (
    <Link
      href={href}
      title={title}
      aria-current={active ? 'page' : undefined}
      className={cn(
        'inline-flex items-center border-b-2 px-2 whitespace-nowrap t-ui ui-transition',
        /* The underline is the state, not a colour wash. `ink` on the rule and
         * `ink` on the label — no hue, because no hue in this product means
         * "selected". */
        active
          ? 'border-ink text-ink'
          : 'border-transparent text-ink-muted hover:text-ink',
      )}
    >
      {children}
    </Link>
  );
}

/**
 * `/board/syn-sgl` -> `syn-sgl`. Null on `/board` and on anything else.
 *
 * Decoded, because `leagueBoardHref` encodes: a slug is URL-safe today, but the
 * round trip must not depend on that staying true.
 */
function leagueSlugFromPathname(pathname: string): string | null {
  const match = /^\/board\/([^/]+)/.exec(pathname);
  const slug = match?.[1];
  if (slug === undefined) return null;
  try {
    return decodeURIComponent(slug);
  } catch {
    return slug;
  }
}
