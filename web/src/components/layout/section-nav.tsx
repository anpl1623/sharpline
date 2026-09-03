'use client';

/**
 * The product's top-level sections: the board, the signals, the leaderboard.
 *
 * # Why this exists at all, and why it is separate from the league strip
 *
 * The league strip answers "which board", and every entry in it is a league the
 * catalogue reported. These three are not leagues and not filters — they are
 * different surfaces over the same pipeline, and folding them into the strip
 * would make "Signals" look like a competition somebody could bet on.
 *
 * A feature nobody can navigate to is a feature that does not exist, and the
 * phase 9 analytics surface is the one CLAUDE.md §6 calls "the differentiator".
 * It gets a permanent entry point rather than being reachable only by typing a
 * URL.
 *
 * # The active state is the underline, not a colour
 *
 * The same treatment `LeagueNav` uses, deliberately: no hue in this product
 * means "selected". A second visual language for the same idea two rows apart
 * would be exactly the inconsistency DESIGN.md's restraint exists to prevent.
 *
 * A section is active for its whole subtree — `/board/nfl` keeps "Board" lit and
 * `/signals/steam` keeps "Signals" lit — so the highlight answers "where am I"
 * rather than "which exact URL is this".
 */

import Link from 'next/link';
import { usePathname } from 'next/navigation';

import { cn } from '@/lib/utils';

interface Section {
  readonly href: string;
  readonly label: string;
}

const SECTIONS: readonly Section[] = [
  { href: '/board', label: 'Board' },
  { href: '/signals/ev', label: 'Signals' },
  { href: '/leaderboard', label: 'Leaders' },
];

/** `/signals/ev` and `/signals/steam` are both the signals section. */
function sectionRoot(href: string): string {
  const [, first = ''] = href.split('/');
  return `/${first}`;
}

export function SectionNav({ className }: { readonly className?: string }) {
  const pathname = usePathname();

  return (
    <nav
      aria-label="Sections"
      className={cn('min-w-0 self-stretch', className)}
    >
      <ul className="flex h-full items-stretch gap-0.5">
        {SECTIONS.map((section) => {
          const root = sectionRoot(section.href);
          const active = pathname === root || pathname.startsWith(`${root}/`);
          return (
            <li key={section.href} className="flex shrink-0">
              <Link
                href={section.href}
                aria-current={active ? 'page' : undefined}
                className={cn(
                  'inline-flex items-center border-b-2 px-2 whitespace-nowrap t-ui ui-transition',
                  active
                    ? 'border-ink text-ink'
                    : 'border-transparent text-ink-muted hover:text-ink',
                )}
              >
                {section.label}
              </Link>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}
