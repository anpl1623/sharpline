import type { ComponentProps } from 'react';

import { cn } from '@/lib/utils';

/**
 * Label — a native `<label>`.
 *
 * shadcn wraps `@radix-ui/react-label`; that package is not a dependency here
 * and adding one would be a dependency for nothing. Radix Label exists to make
 * a label click-forward to its control on legacy Safari — `htmlFor` on a native
 * label has done that everywhere for years. The native element is also the one
 * a screen reader already understands, with no client bundle attached.
 *
 * Styled at the `label` step: 11px, 600, 0.08em, uppercase. That treatment is
 * what marks a field name as chrome rather than content, and it is the same
 * treatment column heads on the board use.
 */
function Label({ className, ...props }: ComponentProps<'label'>) {
  return (
    <label
      data-slot="label"
      className={cn(
        't-label text-ink-muted select-none',
        'peer-disabled:text-ink-faint',
        className,
      )}
      {...props}
    />
  );
}

export { Label };
