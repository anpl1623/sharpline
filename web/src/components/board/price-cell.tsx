'use client';

/**
 * ONE PRICE. The signature element of the whole product.
 *
 * # The delta rail
 *
 * DESIGN.md spends its entire motion budget here, and the reason a 2px rule is
 * the loudest thing in the viewport is that nothing else on screen moves:
 *
 *   0ms     the rail snaps to full chroma with NO transition in — a hard onset
 *           the eye catches peripherally.
 *   180ms   the numeral rolls once, old digit out and new in.
 *   2500ms  the rail decays on `cubic-bezier(0.7, 0, 0.84, 0)`, holding bright
 *           and then dropping fast.
 *
 * Across the board that decay reads as a RECENCY GRADIENT: a glance shows what
 * moved in the last few seconds without reading a single number. The animation
 * is restarted IMPERATIVELY on the DOM node — remove the class, force a reflow,
 * add it back — because DESIGN.md requires per-cell decay timers and forbids
 * re-rendering the row to drive them. Only `opacity` is animated; no layout
 * property is touched.
 *
 * # This cell re-renders. Its row does not.
 *
 * `usePriceCell` subscribes to exactly one `(market, selection, book)` triple,
 * so a tick re-renders this component and nothing above it. A byte-identical
 * price is not a change and produces no notification, no re-render and no rail —
 * lighting up on republication would make the rail mean "a frame arrived", which
 * is the "noise, not information" failure DESIGN.md names by name.
 *
 * # Direction, and why the arrow points the way it does
 *
 * `in` means the implied probability ROSE — the price SHORTENED, which is steam,
 * and which is amber. `out` means it fell — the price lengthened, cyan. Colour
 * is never the sole carrier: the arrow and the numeral carry it too.
 *
 * The arrow follows the NUMERAL, not the probability. Decimal odds and American
 * odds are monotonic in each other across the whole range, so a shortened price
 * is a numerically smaller price in every format the toggle offers, and an arrow
 * that agrees with the digits on screen is verifiable at a glance. An arrow that
 * pointed up beside a falling number would need a legend to be read at all.
 *
 * The arrow is `ink-muted`, not tinted. It carries direction by SHAPE, which is
 * what the accessibility rule asks for, and leaves ALL of the chroma to the
 * decaying rail. A board where every cell keeps a coloured arrow forever is a
 * board at rest with visual energy, and then a moving price has nothing to be
 * loud against.
 *
 * # The tap target, stated rather than faked
 *
 * DESIGN.md asks for a 44px board tap target "carried by padding" and also for a
 * 36px row holding two stacked 15px cells. Those cannot both be true: two 44px
 * targets need 90px of row. This component takes the largest honest target
 * available — the link fills the full width of its column and the full half of
 * the row, which is about 25px tall at the 56px mobile row and about 15px at the
 * 36px desktop one — and does NOT overlap the two stacked links into each other,
 * which would make the upper cell steal the lower one's taps. The conflict is a
 * design decision, not something to paper over locally.
 */

import Link from 'next/link';
import { useEffect, useId, useRef, useState } from 'react';

import type {
  SchemaMarketStatus,
  SchemaMarketType,
  SchemaPrice,
  SchemaSelectionRole,
} from '@/lib/api/schema';
import { formatOdds } from '@/lib/odds/format';
import { formatLine, formatLineNumber, marketTypeLabel } from '@/lib/odds/line';
import { formatDurationWords } from '@/lib/time';
import { useOddsFormat } from '@/lib/store/preferences';
import { cn } from '@/lib/utils';
import { usePriceCell } from '@/lib/ws/provider';
import type { DeltaDirection } from '@/lib/ws/provider';

/** Every state of the cell sits in this box, so the row's rhythm never shifts. */
const SLOT = 'flex min-h-[15px] flex-1 items-center';

/** Reserved width for the arrow, present whether or not an arrow is drawn. */
const GLYPH = 'flex w-[7px] shrink-0 items-center justify-center';

export interface PriceCellProps {
  readonly eventId: string;
  readonly marketId: string;
  readonly marketType: SchemaMarketType;
  readonly marketStatus: SchemaMarketStatus;
  readonly selectionId: string;
  /**
   * The selection's own display name. It arrives on the payload and is the only
   * correct source — a name is never derived from a role.
   */
  readonly selectionName: string;
  readonly selectionRole: SchemaSelectionRole;
  /** The REST price, shown until the stream delivers one for the same triple. */
  readonly restPrice: SchemaPrice | null;
  /** The quoting book's display name, for the on-focus description. */
  readonly bookLabel: string | null;
}

export function PriceCell({
  eventId,
  marketId,
  marketType,
  marketStatus,
  selectionId,
  selectionName,
  selectionRole,
  restPrice,
  bookLabel,
}: PriceCellProps) {
  const oddsFormat = useOddsFormat();
  const descriptionId = useId();

  const bookSlug = restPrice?.book_slug ?? null;
  const live = usePriceCell(marketId, selectionId, bookSlug);

  // The stream wins outright when it holds this cell — including when it holds a
  // null line. Coalescing the two sources field by field with `??` would let a
  // stale REST line survive a stream that has since said there is none.
  const decimal = live !== undefined ? live.decimal : (restPrice?.decimal_odds ?? null);
  const line = live !== undefined ? live.line : (restPrice?.line ?? null);
  const previousDecimal = live?.previousDecimal ?? null;
  const direction = live?.direction ?? null;
  const changedAt = live?.changedAt ?? null;

  // A change this component was present for. Anything the slate already held at
  // mount is history, not movement: navigating back to the board must not fire
  // every rail on it at once.
  const [mountedChangedAt] = useState<number | null>(() => changedAt);
  const moved = changedAt !== null && changedAt !== mountedChangedAt;

  const railRef = useRef<HTMLAnchorElement | null>(null);
  const lastRailAt = useRef<number | null>(changedAt);

  useEffect(() => {
    const node = railRef.current;
    if (node === null) return;
    if (changedAt === null || lastRailAt.current === changedAt) return;
    lastRailAt.current = changedAt;
    // Remove, force a synchronous layout, re-add. A CSS animation only restarts
    // when the class is genuinely re-applied across a reflow boundary.
    node.classList.remove('rail-decaying');
    node.getBoundingClientRect();
    node.classList.add('rail-decaying');
  }, [changedAt]);

  // Read on focus, so the age in the description is the age at the moment a
  // keyboard user asks for it rather than the age at the moment the price moved.
  // This is the client's own clock compared against its own observation instant,
  // which is a different measurement from staleness — that one is a provider
  // instant against the server's anchor and never touches `Date.now()`.
  const [askedAt, setAskedAt] = useState<number | null>(null);

  const priceText = decimal === null ? '' : formatOdds(decimal, oddsFormat);
  const previousText =
    previousDecimal === null ? null : formatOdds(previousDecimal, oddsFormat);
  const lineText = formatLine(marketType, selectionRole, line);
  const open = marketStatus === 'open';

  const name = accessibleName({
    marketType,
    selectionName,
    selectionRole,
    line,
    priceText,
    marketStatus,
    hasPrice: decimal !== null,
  });

  // ---------------------------------------------------------------------------
  // No book has quoted this selection. An empty well is the correct answer.
  // ---------------------------------------------------------------------------
  if (decimal === null) {
    return (
      <span className={SLOT}>
        {/* An empty well. The slot keeps its shape so the column's rhythm holds,
            and NOTHING is drawn in it — a dash in a price column reads as data,
            and "no book has quoted this selection" is a correct answer rather
            than a missing one. */}
        <span className="price-cell w-full" data-price-cell="none">
          <span className="sr-only">{name}</span>
        </span>
      </span>
    );
  }

  const numeral =
    moved && previousText !== null ? (
      // Keyed on the change instant so React remounts the pair and the CSS
      // animation runs again. Reduced motion collapses its duration to zero in
      // globals.css, which is an instant swap rather than a removed one.
      <span key={changedAt ?? 0} className="t-price digit-roll whitespace-nowrap text-ink">
        <span className="digit-roll-previous" aria-hidden="true">
          {previousText}
        </span>
        <span className="digit-roll-current">{priceText}</span>
      </span>
    ) : (
      <span className="t-price whitespace-nowrap text-ink">{priceText}</span>
    );

  // ---------------------------------------------------------------------------
  // The market is not open. The price stays on screen — a suspension is usually
  // seconds long and blanking the cell would make the board flicker — but it is
  // struck through, dimmed, and NOT offered as something to act on.
  // ---------------------------------------------------------------------------
  if (!open) {
    return (
      <span className={SLOT}>
        <span className="price-cell w-full justify-between gap-1" data-price-cell="suspended">
          <span className="sr-only">{name}</span>
          <span
            aria-hidden="true"
            className="pointer-events-none absolute inset-y-0 start-0 w-[2px] rounded-l-price bg-info"
          />
          <span aria-hidden="true" className="t-price-sm tabular whitespace-nowrap text-ink-faint">
            {lineText}
          </span>
          <span aria-hidden="true" className="flex shrink-0 items-center gap-1">
            <span className="t-price whitespace-nowrap text-ink-faint line-through">{priceText}</span>
            <span className={GLYPH} />
          </span>
        </span>
      </span>
    );
  }

  // The link IS the price cell. One node carries the accessible name, the focus
  // ring, the delta rail and the geometry, because they all describe the same
  // object: splitting them across a focusable wrapper and a styled child is how
  // a cell ends up named but not focusable, or focusable but not named.
  return (
    <>
      <Link
        ref={railRef}
        href={`/events/${eventId}`}
        aria-label={name}
        aria-describedby={descriptionId}
        /* The three price-cell states are marked so a caller can tell them
         * apart WITHOUT inferring it from styling. Only `quoted` is interactive:
         * a suspended market and an unquoted selection are deliberately not
         * focusable and deliberately have no accessible name to act on, so an
         * assertion that "every price cell is focusable and named" has to be
         * able to select the ones that are. Without this the distinction is
         * only visible as a `line-through` class, which is exactly the brittle
         * inference a data attribute exists to remove. */
        data-price-cell="quoted"
        data-direction={direction ?? undefined}
        onFocus={() => {
          setAskedAt(Date.now());
        }}
        className={cn(
          SLOT,
          'price-cell ui-transition justify-between gap-1 hover:bg-ground-3',
        )}
      >
        <span aria-hidden="true" className="t-price-sm tabular whitespace-nowrap text-ink-muted">
          {lineText}
        </span>
        <span aria-hidden="true" className="flex shrink-0 items-center gap-1">
          {numeral}
          <DirectionGlyph direction={direction} />
        </span>
      </Link>
      <span id={descriptionId} className="sr-only">
        {changeDescription({
          direction,
          previousText,
          priceText,
          changedAt,
          askedAt,
          bookLabel,
        })}
      </span>
    </>
  );
}

/**
 * The direction arrow. Pure shape, drawn as an SVG rather than as a unicode
 * triangle so its metrics are the same in every font fallback and its slot never
 * resizes when it appears.
 */
function DirectionGlyph({ direction }: { readonly direction: DeltaDirection | null }) {
  if (direction === null) return <span className={GLYPH} />;
  return (
    <span className={cn(GLYPH, 'text-ink-muted')}>
      <svg
        viewBox="0 0 8 6"
        className="h-[6px] w-[8px]"
        fill="currentColor"
        aria-hidden="true"
        focusable="false"
      >
        <path d={direction === 'in' ? 'M0 0h8L4 6z' : 'M0 6h8L4 0z'} />
      </svg>
    </span>
  );
}

// -----------------------------------------------------------------------------
// Accessible text
// -----------------------------------------------------------------------------

/**
 * Screen readers do not agree on how to pronounce a leading `+` or `-` on a
 * price, and "minus 110" versus "110" is the difference between a favourite and
 * an underdog. The sign is spelled out.
 */
function spokenPrice(text: string): string {
  if (text.startsWith('+')) return `plus ${text.slice(1)}`;
  if (text.startsWith('-')) return `minus ${text.slice(1)}`;
  return text;
}

function spokenLine(role: SchemaSelectionRole, line: number | null): string {
  if (line === null) return '';
  const magnitude = formatLineNumber(line);
  if (magnitude === '') return '';
  // Over and under selections already say which side they are, so the number
  // alone is enough. A handicap's sign is the whole meaning and is spelled out.
  if (role === 'over' || role === 'under') return magnitude;
  if (line === 0) return 'pick em';
  return line > 0 ? `plus ${magnitude}` : `minus ${magnitude}`;
}

interface AccessibleNameInput {
  readonly marketType: SchemaMarketType;
  readonly selectionName: string;
  readonly selectionRole: SchemaSelectionRole;
  readonly line: number | null;
  readonly priceText: string;
  readonly marketStatus: SchemaMarketStatus;
  readonly hasPrice: boolean;
}

/** "Moneyline, Ridgeport Thistles, plus 145" — market, selection, price. */
function accessibleName(input: AccessibleNameInput): string {
  const parts: string[] = [marketTypeLabel(input.marketType), input.selectionName];

  const line = spokenLine(input.selectionRole, input.line);
  if (line !== '') parts.push(line);

  parts.push(input.hasPrice ? spokenPrice(input.priceText) : 'no price');
  if (input.marketStatus !== 'open') parts.push(input.marketStatus);

  return parts.join(', ');
}

interface ChangeDescriptionInput {
  readonly direction: DeltaDirection | null;
  readonly previousText: string | null;
  readonly priceText: string;
  readonly changedAt: number | null;
  readonly askedAt: number | null;
  readonly bookLabel: string | null;
}

/**
 * The on-focus description. Exposed through `aria-describedby` and never
 * announced: individual price changes are read on demand, and the only thing
 * this surface pushes into a live region is one batched sentence every five
 * seconds.
 */
function changeDescription(input: ChangeDescriptionInput): string {
  const sentences: string[] = [];

  if (input.direction !== null && input.previousText !== null) {
    const verb = input.direction === 'in' ? 'Shortened' : 'Lengthened';
    const ago =
      input.askedAt === null || input.changedAt === null
        ? ''
        : `, ${formatDurationWords((input.askedAt - input.changedAt) / 1000)} ago`;
    sentences.push(
      `${verb} from ${spokenPrice(input.previousText)} to ${spokenPrice(input.priceText)}${ago}.`,
    );
  } else {
    sentences.push('No change since this board loaded.');
  }

  if (input.bookLabel !== null) sentences.push(`Price from ${input.bookLabel}.`);

  return sentences.join(' ');
}
