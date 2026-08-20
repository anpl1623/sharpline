import { clsx, type ClassValue } from 'clsx';
import { extendTailwindMerge } from 'tailwind-merge';

/**
 * `cn` — conditional class names, with later Tailwind utilities beating earlier
 * conflicting ones. The standard shadcn helper, with one required extension.
 *
 * tailwind-merge ships knowing Tailwind's DEFAULT scales. This design system
 * clears two namespaces and replaces them with named steps of its own
 * (`--radius-*: initial` then price/card/sheet; three named easings), so those
 * classes are ones tailwind-merge has never heard of. Left unregistered it
 * treats them as unrelated and lets `cn('rounded-sheet', 'rounded-price')` emit
 * BOTH, leaving the winner to stylesheet order — the one thing this function
 * exists to prevent.
 *
 * Registering them against the THEME scale rather than a single class group is
 * deliberate: `radius` feeds every rounded group at once, so `rounded-l-sheet`
 * and `rounded-tl-card` resolve correctly too, which they would not if only the
 * bare `rounded` group were extended.
 *
 * Colour utilities need no registration: tailwind-merge treats any value in a
 * colour position as a colour, so `bg-ground-2` and `text-delta-in` already
 * conflict-resolve.
 */
const twMerge = extendTailwindMerge({
  extend: {
    theme: {
      radius: ['price', 'card', 'sheet'],
      ease: ['enter', 'exit', 'decay'],
      container: ['content'],
    },
  },
});

export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
