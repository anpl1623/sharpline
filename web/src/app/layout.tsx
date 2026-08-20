import type { ReactNode } from 'react';
import type { Metadata, Viewport } from 'next';

import { Providers } from '@/components/layout/providers';
import { SiteHeader } from '@/components/layout/site-header';
import { StatusRail } from '@/components/layout/status-rail';
import { LiveAnnouncer } from '@/components/live/live-announcer';
import { fontVariables } from '@/lib/fonts';

import './globals.css';

/**
 * The shell reads the live league catalogue from the API at request time
 * (`site-header.tsx`), and the API does not exist during `docker build`. The
 * fetch is already `no-store`, which opts the route out of prerendering, but
 * stating it here makes the reason legible rather than implicit in a transport
 * detail two modules away.
 *
 * It is also simply true of this product: every page under this layout renders
 * live odds. There is nothing here to prerender.
 */
export const dynamic = 'force-dynamic';

export const metadata: Metadata = {
  title: {
    default: 'Sharpline',
    template: '%s · Sharpline',
  },
  description:
    'Sharpline is a self-hosted sportsbook simulation. It is not a licensed sportsbook: no real money moves, and all wagering is play-money.',
  applicationName: 'Sharpline',
  robots: { index: false, follow: false },
  /**
   * iOS Safari otherwise turns handicaps, totals and American odds into
   * telephone links — a board is dense with `+150`, `-4.5` and `54.5`, and every
   * one of them becomes a tappable phone number.
   */
  formatDetection: {
    telephone: false,
    date: false,
    address: false,
    email: false,
  },
};

export const viewport: Viewport = {
  colorScheme: 'dark',
  /* The sRGB rendering of `ground-0`. Browser chrome cannot read a custom
   * property, so this is the one place the palette is written as a literal. */
  themeColor: '#0B0D12',
  width: 'device-width',
  initialScale: 1,
};

/**
 * The application shell.
 *
 * `body` is the flex column and `Providers` renders no DOM of its own, so the
 * header, the page and the status rail are direct flex children: the header
 * sticks to the top, the page takes the slack, and the 24px engineering rail
 * sticks to the bottom of the viewport without any fixed-position offset
 * arithmetic that could drift out of sync with its own height.
 *
 * The price announcer is mounted HERE and only here. There is exactly ONE
 * live region for market movement in this application, it is throttled to one
 * batched sentence every five seconds, and a page that mounts a second one is a
 * defect. (A user-initiated control may still own a `role="status"` for its own
 * result count — the search combobox does — because that fires on a keystroke
 * the user made, not on a price the market moved.)
 */
export default function RootLayout({
  children,
}: Readonly<{ children: ReactNode }>) {
  return (
    <html lang="en" className={fontVariables} suppressHydrationWarning>
      <body className="flex min-h-dvh flex-col bg-ground-0 text-ink">
        <Providers>
          <a
            href="#main"
            className="t-ui text-ink sr-only focus:not-sr-only focus:absolute focus:top-3 focus:left-3 focus:z-50 focus:rounded-price focus:border focus:border-rule-hi focus:bg-ground-2 focus:px-3 focus:py-2"
          >
            Skip to content
          </a>

          <SiteHeader />

          <main id="main" tabIndex={-1} className="min-w-0 flex-1">
            {children}
          </main>

          <StatusRail />
          <LiveAnnouncer />
        </Providers>
      </body>
    </html>
  );
}
