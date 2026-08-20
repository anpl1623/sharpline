'use client';

import type { ComponentProps } from 'react';
import * as TooltipPrimitive from '@radix-ui/react-tooltip';

import { cn } from '@/lib/utils';

/**
 * Tooltip — Radix Tooltip.
 *
 * 4px radius, not 6px. DESIGN.md's radius table names the modal/sheet step for
 * panels; a tooltip is a 24px annotation, and 6px on an object that small stops
 * reading as a hierarchy step and starts reading as a bubble. 4px — the card
 * step — is the honest size for it.
 *
 * A tooltip is hover- and focus-only, so it can never be the sole carrier of
 * anything. Provenance, staleness and sequence state that a keyboard or screen
 * reader user must be able to reach belong in the status rail or in
 * `aria-describedby`, not in here.
 *
 * `delayDuration` is 200ms rather than Radix's 700ms: on a dense board the
 * pointer is already over the thing it is asking about, and a long delay reads
 * as the app not responding.
 */
function TooltipProvider({
  delayDuration = 200,
  ...props
}: ComponentProps<typeof TooltipPrimitive.Provider>) {
  return (
    <TooltipPrimitive.Provider
      data-slot="tooltip-provider"
      delayDuration={delayDuration}
      {...props}
    />
  );
}

function Tooltip(props: ComponentProps<typeof TooltipPrimitive.Root>) {
  return <TooltipPrimitive.Root data-slot="tooltip" {...props} />;
}

function TooltipTrigger(
  props: ComponentProps<typeof TooltipPrimitive.Trigger>,
) {
  return <TooltipPrimitive.Trigger data-slot="tooltip-trigger" {...props} />;
}

function TooltipContent({
  className,
  sideOffset = 6,
  children,
  ...props
}: ComponentProps<typeof TooltipPrimitive.Content>) {
  return (
    <TooltipPrimitive.Portal>
      <TooltipPrimitive.Content
        data-slot="tooltip-content"
        sideOffset={sideOffset}
        className={cn(
          'z-50 max-w-64 rounded-card border border-rule-hi bg-ground-2 px-2 py-1',
          't-ui text-ink',
          'data-[state=delayed-open]:anim-pop-in data-[state=instant-open]:anim-pop-in',
          'data-[state=closed]:anim-pop-out',
          className,
        )}
        {...props}
      >
        {children}
      </TooltipPrimitive.Content>
    </TooltipPrimitive.Portal>
  );
}

export { Tooltip, TooltipTrigger, TooltipContent, TooltipProvider };
