import type { ComponentProps } from 'react';

import { cn } from '@/lib/utils';

/**
 * Input — shadcn/ui, restyled to DESIGN.md.
 *
 * `ground-3` is the input well: the only place that ground step is used, so a
 * field is identifiable as a field before its border is read. 44px tall and 2px
 * radius, matching every other non-board control.
 *
 * Type size is 15px (the `body` step). Noted, because it has a real cost: iOS
 * Safari zooms the viewport on focus for any input under 16px, and this scale
 * has no 16px step. The alternative — an off-scale font size on one element —
 * was judged worse than a zoom on an iPhone form, and the forms in this product
 * are short (search, sign in). If it proves annoying in QA the fix is a design
 * decision, not a local override.
 */
function Input({ className, type, ...props }: ComponentProps<'input'>) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        'h-11 w-full min-w-0 rounded-price border border-rule bg-ground-3 px-3',
        'text-[15px] leading-none font-normal text-ink ui-transition',
        'placeholder:text-ink-muted',
        'hover:border-rule-hi',
        'disabled:pointer-events-none disabled:bg-ground-1 disabled:text-ink-faint',
        'file:h-9 file:border-0 file:bg-transparent file:text-[13px] file:font-medium file:text-ink-2',
        'aria-invalid:border-loss',
        className,
      )}
      {...props}
    />
  );
}

export { Input };
