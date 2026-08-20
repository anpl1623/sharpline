'use client';

import type { ComponentProps } from 'react';
import { Slot } from '@radix-ui/react-slot';
import { cva, type VariantProps } from 'class-variance-authority';

import { cn } from '@/lib/utils';

/**
 * Button — shadcn/ui, restyled to DESIGN.md.
 *
 * Four things differ from stock shadcn and each is a design-system rule rather
 * than taste:
 *
 *   - 2px radius, not 6px. `rounded-price` is the control radius across the
 *     product. Uniform bubble-radius is the primary AI-slop tell, so radii here
 *     are small and hierarchical: 2px control, 4px card, 6px sheet.
 *   - 44px default height, not 36px. DESIGN.md sets 44–48px for every control
 *     OUTSIDE the board; the 36px `sm` size exists for dense chrome (header
 *     controls, filter bars) and nothing else.
 *   - No shadow, no ring-offset glow. Elevation is a ground step and a hairline.
 *     Hover raises the border from `rule` to `rule-hi` — that is the whole
 *     hover language.
 *   - `primary` is green and is RESERVED. DESIGN.md keeps saturated `money` for
 *     the one irreversible click (place bet, phase 8). Phase 7 has essentially
 *     no use for it; reaching for it to make something look important is how
 *     the signal gets spent.
 */
const buttonVariants = cva(
  [
    'inline-flex shrink-0 items-center justify-center gap-2 whitespace-nowrap',
    'rounded-price border t-ui select-none ui-transition',
    'disabled:pointer-events-none disabled:text-ink-faint',
    "[&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  ],
  {
    variants: {
      variant: {
        /* The neutral action. This is the one to reach for by default. */
        default:
          'border-rule bg-ground-2 text-ink hover:border-rule-hi hover:bg-ground-3 disabled:border-rule disabled:bg-ground-1',
        /* Reserved for the single irreversible click. See the note above. */
        primary:
          'border-transparent bg-money text-on-money hover:bg-money/90 disabled:bg-ground-2',
        outline:
          'border-rule bg-transparent text-ink hover:border-rule-hi hover:bg-ground-1 disabled:border-rule',
        ghost:
          'border-transparent bg-transparent text-ink-2 hover:bg-ground-2 hover:text-ink',
        link: 'border-transparent bg-transparent text-ink-2 underline underline-offset-4 hover:text-ink',
      },
      size: {
        /* Dense chrome only — header controls, filter bars, table toolbars. */
        sm: 'h-9 px-3',
        default: 'h-11 px-4',
        icon: 'size-11',
      },
    },
    defaultVariants: {
      variant: 'default',
      size: 'default',
    },
  },
);

export type ButtonProps = ComponentProps<'button'> &
  VariantProps<typeof buttonVariants> & {
    /** Render the child element instead of a `<button>`, keeping these styles. */
    asChild?: boolean;
  };

function Button({
  className,
  variant,
  size,
  asChild = false,
  ...props
}: ButtonProps) {
  const Comp = asChild ? Slot : 'button';

  return (
    <Comp
      data-slot="button"
      className={cn(buttonVariants({ variant, size }), className)}
      {...props}
    />
  );
}

export { Button, buttonVariants };
