'use client';

import type { ComponentProps } from 'react';
import { Slot } from '@radix-ui/react-slot';
import { cva, type VariantProps } from 'class-variance-authority';

import { cn } from '@/lib/utils';

/**
 * Badge — the signal treatment from DESIGN.md § Color / Signals.
 *
 * Two tiers, and the gap between them is the point:
 *
 *   - Tinted (8% fill, 40% border) for everything that is a *reading*: +EV,
 *     steam, suspension, book kind. Quiet enough that a board full of them is
 *     still a board and not a Christmas tree.
 *   - `arb` is the ONLY saturated fill in the entire interface. Full `money`
 *     ground, `on-money` ink. Arbitrage is rare enough to earn a shout, and
 *     making it the single loudest object on screen means it is never missed —
 *     and, just as importantly, never imitated by anything less rare.
 *
 * `delta-in` / `delta-out` name direction the same way the tokens and the rail
 * do: `in` = implied probability rose (amber, shortened), `out` = it fell
 * (cyan, lengthened). A badge is never the only carrier of direction; the arrow
 * glyph and the numeral carry it too.
 */
const badgeVariants = cva(
  [
    'inline-flex w-fit shrink-0 items-center gap-1 whitespace-nowrap',
    'rounded-price border px-1.5 py-0.5 t-label',
    "[&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-3",
  ],
  {
    variants: {
      variant: {
        neutral: 'border-rule bg-ground-2 text-ink-2',
        'delta-in': 'border-delta-in/40 bg-delta-in/8 text-delta-in',
        'delta-out': 'border-delta-out/40 bg-delta-out/8 text-delta-out',
        money: 'border-money/40 bg-money/8 text-money',
        info: 'border-info/40 bg-info/8 text-info',
        loss: 'border-loss/40 bg-loss/8 text-loss',
        /* The one saturated fill in the product. Spend it on nothing else. */
        arb: 'border-transparent bg-money text-on-money',
      },
    },
    defaultVariants: {
      variant: 'neutral',
    },
  },
);

export type BadgeProps = ComponentProps<'span'> &
  VariantProps<typeof badgeVariants> & {
    asChild?: boolean;
  };

function Badge({ className, variant, asChild = false, ...props }: BadgeProps) {
  const Comp = asChild ? Slot : 'span';

  return (
    <Comp
      data-slot="badge"
      className={cn(badgeVariants({ variant }), className)}
      {...props}
    />
  );
}

export { Badge, badgeVariants };
