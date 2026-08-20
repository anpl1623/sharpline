import type { ComponentProps } from 'react';

import { cn } from '@/lib/utils';

/**
 * Skeleton — a loading placeholder, deliberately WITHOUT a pulse.
 *
 * shadcn's skeleton animates `animate-pulse` forever. That is exactly the
 * motion this design system spends its entire budget avoiding: DESIGN.md puts
 * the whole budget on one element, the delta rail, and the reason a 2px rail
 * lighting up is the loudest event in the viewport is that nothing else on
 * screen is moving. A grid of pulsing rectangles next to a live board competes
 * with the only motion that carries information, and loses the board's meaning
 * to win a loading affordance nobody needed.
 *
 * So this is a static `ground-2` block. "Loading" is still distinguishable from
 * "empty" because the two look nothing alike — an empty board renders a written
 * empty state, not grey bars — and because the container that owns the fetch
 * carries `aria-busy`, which is what actually tells an assistive technology
 * something is in flight.
 *
 * `aria-hidden` because the shape is not content: announcing a placeholder is
 * noise on a surface that already has a throttled live region to protect.
 */
function Skeleton({ className, ...props }: ComponentProps<'div'>) {
  return (
    <div
      data-slot="skeleton"
      aria-hidden="true"
      className={cn('rounded-price bg-ground-2', className)}
      {...props}
    />
  );
}

export { Skeleton };
