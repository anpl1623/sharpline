import localFont from 'next/font/local';
import { Instrument_Sans, JetBrains_Mono } from 'next/font/google';

/**
 * Typefaces — DESIGN.md § Typography.
 *
 * NO FONT CDN REACHES THE BROWSER. `next/font` downloads and self-hosts every
 * face at build time and emits it from this origin, which is the same rule the
 * rest of the project runs on: this thing computes on hardware the author
 * controls, and fonts are not an exception carved out for convenience.
 *
 * Two registers, and the register change is the product's whole typographic
 * argument: the consumer surface is sans, the engineering layer is mono. Every
 * mono glyph on screen means *the machine is talking*. Prices are SANS with
 * tabular figures — mono on a price would collapse the distinction.
 *
 * Each face exposes a CSS custom property that `globals.css` composes into
 * `--font-sans` / `--font-mono` / `--font-display` with its fallback stack. The
 * indirection is what lets the theme own the fallbacks in one place.
 */

/**
 * Instrument Sans — body, UI, and ALL prices.
 *
 * Loaded as the VARIABLE font (no `weight` argument), and that is load-bearing
 * rather than incidental: the scale asks for 550 on prices and 650 on h1, and
 * neither weight exists as a static instance. Pinning `weight: ['400','500']`
 * here would silently round both to the nearest static face and flatten the
 * hierarchy the scale is built on.
 *
 * Chosen over Inter/Geist/system-ui for true tabular lining figures, a `0` that
 * cannot be read as `O` and a `1` that cannot be read as `l` at 13px, and a
 * strong `+`/`−` — every one of which is a legibility requirement on a board of
 * American odds rather than a preference.
 */
export const instrumentSans = Instrument_Sans({
  subsets: ['latin'],
  display: 'swap',
  variable: '--font-instrument-sans',
  fallback: [
    'ui-sans-serif',
    'system-ui',
    '-apple-system',
    'Segoe UI',
    'Roboto',
    'Helvetica Neue',
    'Arial',
    'sans-serif',
  ],
});

/**
 * JetBrains Mono — the engineering layer only. Status rail, connection id,
 * sequence numbers, provenance, staleness. Never a price.
 *
 * Two weights are enough because this register has exactly two jobs: a value
 * (400) and the label that names it (500).
 */
export const jetbrainsMono = JetBrains_Mono({
  subsets: ['latin'],
  weight: ['400', '500'],
  display: 'swap',
  variable: '--font-jetbrains-mono',
  fallback: [
    'ui-monospace',
    'SFMono-Regular',
    'SF Mono',
    'Menlo',
    'Consolas',
    'Liberation Mono',
    'monospace',
  ],
});

/**
 * Clash Grotesk — display only: the landing poster and section heads.
 *
 * Not on Google Fonts, so the woff2 files are committed under `web/public/fonts`
 * (with the Fontshare licence beside them) and loaded from disk. `next/font/local`
 * resolves `src` relative to THIS file, hence the `../../public` prefix.
 *
 * Two static instances, 500 and 600. There is no variable axis here, which is
 * exactly why the type scale keeps its 650 step on Instrument Sans: asking Clash
 * for 650 would either snap to 600 or synthesise a fake bold, and a faked weight
 * on the largest text on the page is visible.
 *
 * DESIGN.md Open Decision #2 names this as the first family to cut if a
 * two-family system is ever preferred; Instrument Sans absorbs the display role
 * at 650. Keeping it isolated to one token is what makes that a one-line change.
 */
export const clashGrotesk = localFont({
  src: [
    {
      path: '../../public/fonts/ClashGrotesk-Medium.woff2',
      weight: '500',
      style: 'normal',
    },
    {
      path: '../../public/fonts/ClashGrotesk-Semibold.woff2',
      weight: '600',
      style: 'normal',
    },
  ],
  display: 'swap',
  variable: '--font-clash-grotesk',
  fallback: ['ui-sans-serif', 'system-ui', 'sans-serif'],
});

/**
 * The three font variable classes, ready to drop on `<html>` in the root layout:
 *
 *   <html lang="en" className={fontVariables}>
 *
 * It goes on `<html>` rather than `<body>` because `globals.css` declares
 * `--font-sans` on `:root`, and a custom property can only reference another
 * that is in scope at the same element or above.
 */
export const fontVariables = [
  instrumentSans.variable,
  jetbrainsMono.variable,
  clashGrotesk.variable,
].join(' ');
