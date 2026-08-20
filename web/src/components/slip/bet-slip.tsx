'use client';

/**
 * WHERE the bet slip lives. DESIGN.md § Layout: "right rail ≥1000px, bottom
 * sheet below" — Open Decision #5, which phase 8 owns and closes.
 *
 * # The 1000px breakpoint is written out, and that is on purpose
 *
 * Tailwind's `lg` is 1024px and DESIGN.md says 1000. Rounding a design-system
 * number to the nearest utility is how a spec and an implementation start
 * disagreeing quietly, so the arbitrary variant `min-[1000px]:` is used
 * throughout this file and nowhere is `lg:` substituted for it.
 *
 * # The rail is PERSISTENT and the dock is not
 *
 * At the rail width the slip is always on screen, empty or not — DESIGN.md keeps
 * the category's persistent-slip convention deliberately, and a rail that
 * appeared on the first selection would shove the board sideways at the worst
 * possible moment.
 *
 * Below it, an empty slip shows NOTHING. A dock bar reading "0 selections" is a
 * permanent 56px of viewport spent saying that nothing has happened, on the
 * screen size that can least afford it. The first selection brings the dock in;
 * clearing the slip takes it away.
 *
 * # What the sheet contributes, beyond being a sheet
 *
 * Its mechanics were settled before it had contents — 6px radius, 180ms `short`
 * enter on the `enter` curve, focus trap — and `ui/sheet.tsx` already provides
 * all three. The one piece the primitive does not provide is DRAG-TO-DISMISS,
 * and it is implemented here on the HANDLE ALONE rather than on the whole
 * surface. Dragging anywhere would fight the leg list's own scrolling, and a
 * sheet that closes when somebody tries to scroll their four-leg parlay is worse
 * than one that does not drag at all.
 *
 * The drag animates `transform` and nothing else, which is the motion rule for
 * the whole product. It is not an entrance animation and does not spend from the
 * delta rail's budget: it is a direct response to a finger, one-to-one, and it
 * stops the instant the finger does.
 */

import { useCallback, useRef, useState } from 'react';

import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetTitle,
} from '@/components/ui';
import { formatMinor } from '@/lib/money';
import { slipActions, useSlip } from '@/lib/store/slip';
import { cn } from '@/lib/utils';
import { SlipPanel } from './slip-panel';

/** Past this many pixels of downward drag, releasing dismisses the sheet. */
const DISMISS_THRESHOLD_PX = 96;

/** The dock's height, mirrored by the spacer that keeps it off the last row. */
const DOCK_HEIGHT = 'h-14';

/**
 * THE RAIL. Rendered as a SIBLING OF THE PAGE, inside the shell's flex row.
 *
 * `self-start` is load-bearing: a flex child stretches to its container's height
 * by default, and a full-height element has nothing to stick to. Sizing the
 * aside to its own content is what lets `sticky` work at all.
 *
 * The height budget is the viewport minus the 48px header and the 24px status
 * rail, so the slip's own footer — the stake, the summary and the CTA — is
 * always on screen and the legs scroll inside it. A slip whose Place button was
 * below the fold on a long parlay would be the one control in the product that
 * has to be hunted for.
 */
export function BetSlipRail() {
  return (
    <aside
      aria-label="Bet slip"
      className={cn(
        'hidden w-[336px] shrink-0 self-start',
        'sticky top-12 max-h-[calc(100dvh-4.5rem)]',
        'border-l border-rule bg-ground-1',
        'min-[1000px]:flex min-[1000px]:flex-col',
      )}
    >
      <SlipPanel />
    </aside>
  );
}

// -----------------------------------------------------------------------------
// The dock and the sheet
// -----------------------------------------------------------------------------

/**
 * THE DOCK AND THE SHEET. Rendered as a CHILD OF THE SHELL'S COLUMN, after the
 * page row, so its flow spacer reserves HEIGHT — inside the row it would reserve
 * width instead and the bar would still sit on the last board row.
 */
export function BetSlipDock() {
  const count = useSlip((state) => state.legs.length);
  const stakeMinor = useSlip((state) => state.stakeMinor);
  const open = useSlip((state) => state.open);
  const hydrated = useSlip((state) => state.hydrated);
  const placed = useSlip((state) => state.receipt !== null);

  // The bar appears for a slip with something on it, and for the moment after a
  // placement — a receipt with no bar and no sheet would be a booked ticket the
  // customer never got to read.
  //
  // Nothing at all until storage has been read, so a restored slip does not
  // arrive by having the bar appear a frame late underneath the content.
  const bar = hydrated && (count > 0 || placed);

  return (
    <>
      {bar ? (
        <>
          {/* Reserves the bar's height in NORMAL FLOW. The bar itself is fixed,
              so without this the last row of the board would sit permanently
              under it at the bottom of the page. */}
          <div
            aria-hidden="true"
            className={cn(DOCK_HEIGHT, 'shrink-0 min-[1000px]:hidden')}
          />

          <div
            className={cn(
              // `bottom-6` from 768px up, where the 24px status rail is showing
              // and the bar must sit on top of it rather than over it. Below
              // 768px the rail has collapsed to the header pip and the bar takes
              // the edge.
              'fixed inset-x-0 bottom-0 z-40 md:bottom-6 min-[1000px]:hidden',
              'border-t border-rule bg-ground-1',
            )}
          >
            <button
              type="button"
              aria-expanded={open}
              onClick={() => {
                slipActions().setOpen(true);
              }}
              className={cn(
                DOCK_HEIGHT,
                'flex w-full items-center justify-between gap-3 px-4 t-ui text-ink',
                'ui-transition hover:bg-ground-2',
              )}
            >
              <span className="flex items-baseline gap-2">
                <span>{count === 0 && placed ? 'Bet placed' : 'Bet slip'}</span>
                {count === 0 ? null : (
                  <span className="t-label text-ink-muted">
                    {count === 1
                      ? '1 selection'
                      : `${String(count)} selections`}
                  </span>
                )}
              </span>
              {stakeMinor > 0 ? (
                <span className="t-price-sm tabular text-ink-2">
                  <span className="t-label mr-1 text-ink-muted">Stake</span>
                  {formatMinor(stakeMinor)}
                </span>
              ) : null}
            </button>
          </div>
        </>
      ) : null}

      {/* ALWAYS rendered, even with no bar. Radix portals nothing while closed,
          and unmounting the sheet at the same moment the slip empties would tear
          the surface out from under a placement that has just landed on it. */}
      <SlipSheet open={open} />
    </>
  );
}

function SlipSheet({ open }: { readonly open: boolean }) {
  const contentRef = useRef<HTMLDivElement | null>(null);
  const [dragging, setDragging] = useState(false);
  const dragOrigin = useRef<number | null>(null);

  const close = useCallback(() => {
    slipActions().setOpen(false);
  }, []);

  /**
   * Writes the drag offset straight to the node rather than through state.
   *
   * A pointermove fires at the display's refresh rate; routing each one through
   * React would re-render the whole slip — the leg list, the summary, the
   * quote's subscriptions — sixty times a second while a finger moves. The
   * transform is a visual response to a gesture and owns nothing, so the DOM is
   * the right place for it, and it is torn down on release.
   */
  const setOffset = (pixels: number): void => {
    const node = contentRef.current;
    if (node === null) return;
    node.style.transform = pixels === 0 ? '' : `translateY(${String(pixels)}px)`;
  };

  return (
    <Sheet
      open={open}
      onOpenChange={(next) => {
        if (!next) close();
      }}
    >
      <SheetContent
        ref={contentRef}
        side="bottom"
        /* No `aria-label` here: `SheetTitle` below is the accessible name, and
         * an explicit label on the Content would override the title Radix
         * already wires to it — leaving two names for one surface, one of which
         * is not visible. */
        className={cn(
          'flex max-h-[85dvh] flex-col gap-0 p-0',
          // No transition WHILE dragging — the transform must track the finger
          // exactly — and a 180ms `enter` glide back when it is released.
          dragging
            ? 'transition-none'
            : 'transition-transform duration-[180ms] ease-enter',
        )}
      >
        <div
          // The drag surface, and only this. See the file comment.
          onPointerDown={(event) => {
            event.currentTarget.setPointerCapture(event.pointerId);
            dragOrigin.current = event.clientY;
            setDragging(true);
          }}
          onPointerMove={(event) => {
            const origin = dragOrigin.current;
            if (origin === null) return;
            // Downward only. An upward drag on a sheet that is already at its
            // maximum height has nowhere to go, and letting it lift would open a
            // gap under the sheet.
            setOffset(Math.max(0, event.clientY - origin));
          }}
          onPointerUp={(event) => {
            const origin = dragOrigin.current;
            dragOrigin.current = null;
            setDragging(false);
            const travelled = origin === null ? 0 : event.clientY - origin;
            setOffset(0);
            if (travelled > DISMISS_THRESHOLD_PX) close();
          }}
          onPointerCancel={() => {
            dragOrigin.current = null;
            setDragging(false);
            setOffset(0);
          }}
          className="flex shrink-0 cursor-grab touch-none items-center justify-center py-3"
        >
          {/* The grab handle. Full radius is legal on exactly two objects in this
              product — the connection pip and an avatar — so this is a 2px
              `price` radius like every other small surface. */}
          <span
            aria-hidden="true"
            className="h-1 w-10 rounded-price bg-rule-hi"
          />
        </div>

        {/* `pr-12` clears the sheet's own close button, which the primitive
            pins at `top-4 right-4`. */}
        <SheetTitle className="shrink-0 px-4 pr-12 pb-2">Bet slip</SheetTitle>
        {/* Radix warns when a Dialog has no description, and the warning is
            worth answering rather than silencing: this names the sheet's whole
            job for a screen reader arriving at it cold. */}
        <SheetDescription className="sr-only">
          Your selections, the stake, and what the ticket pays. Play money — this
          is a simulation, not a licensed sportsbook.
        </SheetDescription>

        {/* `headless`: the sheet supplies the heading, so the panel must not
            render a second one. */}
        <div className="flex min-h-0 flex-1 flex-col">
          <SlipPanel headless />
        </div>
      </SheetContent>
    </Sheet>
  );
}
