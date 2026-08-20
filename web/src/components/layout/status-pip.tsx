'use client';

/**
 * THE COLLAPSED STATUS PIP — DESIGN.md § "Resolved: the collapsed status pip".
 *
 * Below 768px the persistent 24px mono rail is 6% of an 812px viewport and it is
 * the first thing to cut. It collapses to a single 8px pip in the header — the
 * only full-radius object in the product besides an avatar, exactly as the radius
 * table already allows.
 *
 * # Colour is never the sole carrier
 *
 *   Streaming     money      "Live — streaming"
 *   Resyncing     info       "Resyncing"
 *   Reconnecting  info + 1.2s pulse  "Reconnecting"
 *   Disconnected  loss       "Disconnected"
 *   Idle          ink-faint  "Not connected"
 *
 * The fill and the pulse live in `globals.css` under `.status-pip[data-state]`;
 * the words come from `describeStream` and are used VERBATIM as the control's
 * accessible name, so a screen reader user and a colourblind user both get the
 * fact stated rather than shown. The pulse is disabled under
 * `prefers-reduced-motion` — its meaning is already in the name.
 *
 * `money` on the pip is not a violation of "green means money": the pip is not a
 * price and carries no quantity. DESIGN.md records it as a decision.
 *
 * # Tapping it folds the rail back out
 *
 * The sheet enters FROM THE TOP, not the bottom. DESIGN.md is explicit — "expands
 * the full rail content ... into a 6px-radius sheet from the top" — and
 * `ui/sheet.tsx` documents its `top` side as being for exactly this. It carries
 * the same fields, in the same order, in the same mono register as the desktop
 * rail. Nothing is lost on mobile, only folded.
 */

import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetTitle,
  SheetTrigger,
} from '@/components/ui';
import { StatusReadout } from '@/components/layout/status-rail';
import { cn } from '@/lib/utils';
import { useStreamDescription } from '@/lib/ws/provider';

export interface StatusPipProps {
  readonly className?: string | undefined;
}

export function StatusPip({ className }: StatusPipProps) {
  const { tone, label } = useStreamDescription();

  return (
    <Sheet>
      <SheetTrigger asChild>
        <button
          type="button"
          /* Verbatim from DESIGN.md's table. `aria-haspopup` supplies the
           * "and it opens something" half without diluting the state name. */
          aria-label={label}
          aria-haspopup="dialog"
          title={label}
          className={cn(
            'inline-flex size-9 shrink-0 items-center justify-center rounded-price',
            'text-ink-2 ui-transition hover:bg-ground-2',
            className,
          )}
        >
          <span className="status-pip" data-state={tone} aria-hidden="true" />
        </button>
      </SheetTrigger>

      <SheetContent side="top" className="gap-4">
        <SheetTitle className="t-label text-ink-muted">Connection</SheetTitle>
        <SheetDescription className="sr-only">
          Live connection state, sequence number, channels held, odds staleness
          and provenance — the same readout the desktop status rail carries.
        </SheetDescription>
        <StatusReadout layout="stacked" />
      </SheetContent>
    </Sheet>
  );
}
