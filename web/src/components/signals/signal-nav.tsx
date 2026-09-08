'use client';

/**
 * The sub-navigation across the three signal feeds.
 *
 * # Why three routes rather than one page with tabs
 *
 * Each feed has a different ordering, a different set of filters and a different
 * pagination contract — +EV is ranked by value and cursored, steam is ranked by
 * recency and cursored, arbitrage is a bounded live set with no cursor at all.
 * Tabs on one route would put three URLs' worth of state into one address, so a
 * link to "the arbitrage I am looking at" would not be a link to it. Three
 * routes keep the address bar honest, which is the same reason `/board/{slug}`
 * is a route rather than a filter.
 *
 * The active state is the underline the league strip uses, and for the same
 * reason: no hue in this product means "selected".
 */

import Link from 'next/link';
import { usePathname } from 'next/navigation';

import { cn } from '@/lib/utils';

interface Feed {
  readonly href: string;
  readonly label: string;
  readonly description: string;
}

export const SIGNAL_FEEDS: readonly Feed[] = [
  {
    href: '/signals/ev',
    label: 'Positive EV',
    description: 'Prices that beat the sharp book’s no-vig fair value.',
  },
  {
    href: '/signals/arbitrage',
    label: 'Arbitrage',
    description: 'Markets whose best prices sum to under one.',
  },
  {
    href: '/signals/steam',
    label: 'Steam',
    description: 'Correlated line moves, led by one book.',
  },
];

export function SignalNav({ className }: { readonly className?: string }) {
  const pathname = usePathname();

  return (
    <nav aria-label="Signal feeds" className={cn('min-w-0', className)}>
      <ul
        className={cn(
          'flex items-stretch gap-0.5 overflow-x-auto',
          '[scrollbar-width:none] [&::-webkit-scrollbar]:hidden',
        )}
      >
        {SIGNAL_FEEDS.map((feed) => {
          const active = pathname === feed.href;
          return (
            <li key={feed.href} className="flex shrink-0">
              <Link
                href={feed.href}
                aria-current={active ? 'page' : undefined}
                className={cn(
                  'inline-flex items-center border-b-2 px-2 py-2 whitespace-nowrap t-ui ui-transition',
                  active
                    ? 'border-ink text-ink'
                    : 'border-transparent text-ink-muted hover:text-ink',
                )}
              >
                {feed.label}
              </Link>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}
