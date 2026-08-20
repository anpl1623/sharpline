'use client';

import type { ComponentProps } from 'react';
import * as TabsPrimitive from '@radix-ui/react-tabs';

import { cn } from '@/lib/utils';

/**
 * Tabs — Radix Tabs, restyled as an underlined set rather than a pill group.
 *
 * shadcn's default is a filled track with a rounded "thumb" behind the active
 * tab. That is two extra surfaces and a bubble radius to say one thing, and on
 * a dark industrial ground it reads as a segmented control from a phone
 * settings screen. An underline says the same thing with a hairline: the rule
 * under the list is already there to separate the tabs from their panel, and
 * the active tab simply owns its segment of it.
 *
 * The active mark is `ink` — not an accent colour. No hue in this product means
 * "selected"; the five that exist are spoken for (money, direction ×2, error,
 * system state), and borrowing one here would make a tab look like a signal.
 */
function Tabs({ className, ...props }: ComponentProps<typeof TabsPrimitive.Root>) {
  return (
    <TabsPrimitive.Root
      data-slot="tabs"
      className={cn('flex flex-col gap-4', className)}
      {...props}
    />
  );
}

function TabsList({
  className,
  ...props
}: ComponentProps<typeof TabsPrimitive.List>) {
  return (
    <TabsPrimitive.List
      data-slot="tabs-list"
      className={cn(
        'inline-flex w-full items-stretch gap-4 border-b border-rule',
        className,
      )}
      {...props}
    />
  );
}

function TabsTrigger({
  className,
  ...props
}: ComponentProps<typeof TabsPrimitive.Trigger>) {
  return (
    <TabsPrimitive.Trigger
      data-slot="tabs-trigger"
      className={cn(
        'inline-flex h-11 items-center justify-center gap-2 whitespace-nowrap',
        /* -1px so the active underline sits ON the list's rule, not under it. */
        '-mb-px border-b-2 border-transparent px-1 t-ui ui-transition',
        'text-ink-muted hover:text-ink-2',
        'data-[state=active]:border-ink data-[state=active]:text-ink',
        'disabled:pointer-events-none disabled:text-ink-faint',
        "[&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
        className,
      )}
      {...props}
    />
  );
}

function TabsContent({
  className,
  ...props
}: ComponentProps<typeof TabsPrimitive.Content>) {
  // No `outline-none` here, unlike stock shadcn. Radix makes the panel
  // focusable, so stripping the outline would remove a focus indicator with
  // nothing replacing it; the global :focus-visible ring in globals.css is the
  // right answer for a panel that carries no highlight state of its own.
  return (
    <TabsPrimitive.Content
      data-slot="tabs-panel"
      className={cn(className)}
      {...props}
    />
  );
}

export { Tabs, TabsList, TabsTrigger, TabsContent };
