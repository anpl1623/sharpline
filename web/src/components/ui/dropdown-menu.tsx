'use client';

import type { ComponentProps } from 'react';
import * as DropdownMenuPrimitive from '@radix-ui/react-dropdown-menu';
import { Check, ChevronRight, Circle } from 'lucide-react';

import { cn } from '@/lib/utils';

/**
 * DropdownMenu — Radix DropdownMenu.
 *
 * Same floating-surface language as Select: `ground-2` panel, `rule-hi`
 * hairline, 6px radius, and no shadow. A menu is one ground step above what it
 * covers, and that step plus the emphasised border is the entire separation
 * mechanism.
 *
 * Items take the 2px control radius, not the panel's 6px. Nesting a 6px corner
 * inside a 6px corner is what makes a menu look inflated; the item is a control
 * and takes the control radius.
 *
 * Highlight is `ground-3` — the input-well step — rather than an accent fill.
 * No hue means "hovered" anywhere in this product.
 */
function DropdownMenu(
  props: ComponentProps<typeof DropdownMenuPrimitive.Root>,
) {
  return <DropdownMenuPrimitive.Root data-slot="dropdown-menu" {...props} />;
}

function DropdownMenuPortal(
  props: ComponentProps<typeof DropdownMenuPrimitive.Portal>,
) {
  return (
    <DropdownMenuPrimitive.Portal data-slot="dropdown-menu-portal" {...props} />
  );
}

function DropdownMenuTrigger(
  props: ComponentProps<typeof DropdownMenuPrimitive.Trigger>,
) {
  return (
    <DropdownMenuPrimitive.Trigger
      data-slot="dropdown-menu-trigger"
      {...props}
    />
  );
}

function DropdownMenuGroup(
  props: ComponentProps<typeof DropdownMenuPrimitive.Group>,
) {
  return (
    <DropdownMenuPrimitive.Group data-slot="dropdown-menu-group" {...props} />
  );
}

function DropdownMenuRadioGroup(
  props: ComponentProps<typeof DropdownMenuPrimitive.RadioGroup>,
) {
  return (
    <DropdownMenuPrimitive.RadioGroup
      data-slot="dropdown-menu-radio-group"
      {...props}
    />
  );
}

function DropdownMenuSub(
  props: ComponentProps<typeof DropdownMenuPrimitive.Sub>,
) {
  return <DropdownMenuPrimitive.Sub data-slot="dropdown-menu-sub" {...props} />;
}

const panelClasses = [
  'z-50 min-w-32 overflow-hidden p-1',
  'rounded-sheet border border-rule-hi bg-ground-2',
  'data-[state=open]:anim-pop-in data-[state=closed]:anim-pop-out',
];

const itemClasses = [
  'relative flex cursor-default items-center gap-2 rounded-price px-2 py-2',
  /* `outline-none` is legal here and needs its replacement named: Radix
   * moves real DOM focus onto the highlighted item, so the focus ring and
   * the highlight would both fire. `data-[highlighted]` below IS the focus
   * indicator — it is not decoration, and it must not be removed. */
  't-ui text-ink-2 outline-none select-none ui-transition',
  'data-[highlighted]:bg-ground-3 data-[highlighted]:text-ink',
  'data-[disabled]:pointer-events-none data-[disabled]:text-ink-faint',
  "[&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
];

function DropdownMenuContent({
  className,
  sideOffset = 4,
  ...props
}: ComponentProps<typeof DropdownMenuPrimitive.Content>) {
  return (
    <DropdownMenuPrimitive.Portal>
      <DropdownMenuPrimitive.Content
        data-slot="dropdown-menu-content"
        sideOffset={sideOffset}
        className={cn(
          panelClasses,
          'max-h-[var(--radix-dropdown-menu-content-available-height)]',
          className,
        )}
        {...props}
      />
    </DropdownMenuPrimitive.Portal>
  );
}

function DropdownMenuItem({
  className,
  inset = false,
  ...props
}: ComponentProps<typeof DropdownMenuPrimitive.Item> & { inset?: boolean }) {
  return (
    <DropdownMenuPrimitive.Item
      data-slot="dropdown-menu-item"
      className={cn(itemClasses, inset && 'pl-8', className)}
      {...props}
    />
  );
}

function DropdownMenuCheckboxItem({
  className,
  children,
  ...props
}: ComponentProps<typeof DropdownMenuPrimitive.CheckboxItem>) {
  return (
    <DropdownMenuPrimitive.CheckboxItem
      data-slot="dropdown-menu-checkbox-item"
      className={cn(itemClasses, 'pr-2 pl-8', className)}
      {...props}
    >
      <span className="absolute left-2 flex size-4 items-center justify-center">
        <DropdownMenuPrimitive.ItemIndicator>
          <Check className="size-4" aria-hidden="true" />
        </DropdownMenuPrimitive.ItemIndicator>
      </span>
      {children}
    </DropdownMenuPrimitive.CheckboxItem>
  );
}

function DropdownMenuRadioItem({
  className,
  children,
  ...props
}: ComponentProps<typeof DropdownMenuPrimitive.RadioItem>) {
  return (
    <DropdownMenuPrimitive.RadioItem
      data-slot="dropdown-menu-radio-item"
      className={cn(itemClasses, 'pr-2 pl-8', className)}
      {...props}
    >
      <span className="absolute left-2 flex size-4 items-center justify-center">
        <DropdownMenuPrimitive.ItemIndicator>
          <Circle className="size-2 fill-current" aria-hidden="true" />
        </DropdownMenuPrimitive.ItemIndicator>
      </span>
      {children}
    </DropdownMenuPrimitive.RadioItem>
  );
}

function DropdownMenuLabel({
  className,
  inset = false,
  ...props
}: ComponentProps<typeof DropdownMenuPrimitive.Label> & { inset?: boolean }) {
  return (
    <DropdownMenuPrimitive.Label
      data-slot="dropdown-menu-label"
      className={cn('px-2 py-1.5 t-label text-ink-muted', inset && 'pl-8', className)}
      {...props}
    />
  );
}

function DropdownMenuSeparator({
  className,
  ...props
}: ComponentProps<typeof DropdownMenuPrimitive.Separator>) {
  return (
    <DropdownMenuPrimitive.Separator
      data-slot="dropdown-menu-separator"
      className={cn('-mx-1 my-1 h-px bg-rule', className)}
      {...props}
    />
  );
}

/**
 * A keyboard hint. `ink-faint` is legal here and only here in this file: it is
 * a decorative echo of a shortcut the user can already discover, never the only
 * place information lives.
 */
function DropdownMenuShortcut({ className, ...props }: ComponentProps<'span'>) {
  return (
    <span
      data-slot="dropdown-menu-shortcut"
      className={cn('ml-auto t-mono text-ink-faint', className)}
      {...props}
    />
  );
}

function DropdownMenuSubTrigger({
  className,
  inset = false,
  children,
  ...props
}: ComponentProps<typeof DropdownMenuPrimitive.SubTrigger> & {
  inset?: boolean;
}) {
  return (
    <DropdownMenuPrimitive.SubTrigger
      data-slot="dropdown-menu-sub-trigger"
      className={cn(
        itemClasses,
        'data-[state=open]:bg-ground-3 data-[state=open]:text-ink',
        inset && 'pl-8',
        className,
      )}
      {...props}
    >
      {children}
      <ChevronRight className="ml-auto size-4" aria-hidden="true" />
    </DropdownMenuPrimitive.SubTrigger>
  );
}

function DropdownMenuSubContent({
  className,
  ...props
}: ComponentProps<typeof DropdownMenuPrimitive.SubContent>) {
  return (
    <DropdownMenuPrimitive.SubContent
      data-slot="dropdown-menu-sub-content"
      className={cn(panelClasses, className)}
      {...props}
    />
  );
}

export {
  DropdownMenu,
  DropdownMenuPortal,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuCheckboxItem,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuShortcut,
  DropdownMenuSub,
  DropdownMenuSubTrigger,
  DropdownMenuSubContent,
};
