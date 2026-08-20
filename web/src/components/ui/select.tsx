'use client';

import type { ComponentProps } from 'react';
import * as SelectPrimitive from '@radix-ui/react-select';
import { Check, ChevronDown, ChevronUp } from 'lucide-react';

import { cn } from '@/lib/utils';

/**
 * Select — Radix Select.
 *
 * The trigger is an INPUT, not a button: it takes the `ground-3` well, the
 * 2px radius and the 44px height every other field has, because a control that
 * chooses a value and a control that types one should be the same object as far
 * as the eye is concerned. Only the chevron distinguishes them.
 *
 * The listbox is a floating panel and takes the 6px sheet radius, on `ground-2`
 * with a `rule-hi` border. That border is doing the job a drop shadow does in
 * most systems — this system has no shadows, so a floating surface separates
 * itself by being one ground step brighter than what it covers and by carrying
 * the emphasised hairline.
 *
 * Expected phase 7 use: the odds-format toggle (American / decimal / fractional)
 * and the reference-book picker.
 */
function Select(props: ComponentProps<typeof SelectPrimitive.Root>) {
  return <SelectPrimitive.Root data-slot="select" {...props} />;
}

function SelectGroup(props: ComponentProps<typeof SelectPrimitive.Group>) {
  return <SelectPrimitive.Group data-slot="select-group" {...props} />;
}

function SelectValue(props: ComponentProps<typeof SelectPrimitive.Value>) {
  return <SelectPrimitive.Value data-slot="select-value" {...props} />;
}

function SelectTrigger({
  className,
  children,
  ...props
}: ComponentProps<typeof SelectPrimitive.Trigger>) {
  return (
    <SelectPrimitive.Trigger
      data-slot="select-trigger"
      className={cn(
        'flex h-11 w-fit items-center justify-between gap-2 whitespace-nowrap',
        'rounded-price border border-rule bg-ground-3 px-3',
        't-ui text-ink ui-transition',
        'hover:border-rule-hi',
        'data-[placeholder]:text-ink-muted',
        'disabled:pointer-events-none disabled:bg-ground-1 disabled:text-ink-faint',
        "[&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
        className,
      )}
      {...props}
    >
      {children}
      <SelectPrimitive.Icon asChild>
        <ChevronDown className="size-4 text-ink-muted" aria-hidden="true" />
      </SelectPrimitive.Icon>
    </SelectPrimitive.Trigger>
  );
}

function SelectScrollUpButton({
  className,
  ...props
}: ComponentProps<typeof SelectPrimitive.ScrollUpButton>) {
  return (
    <SelectPrimitive.ScrollUpButton
      data-slot="select-scroll-up-button"
      className={cn(
        'flex cursor-default items-center justify-center py-1 text-ink-muted',
        className,
      )}
      {...props}
    >
      <ChevronUp className="size-4" aria-hidden="true" />
    </SelectPrimitive.ScrollUpButton>
  );
}

function SelectScrollDownButton({
  className,
  ...props
}: ComponentProps<typeof SelectPrimitive.ScrollDownButton>) {
  return (
    <SelectPrimitive.ScrollDownButton
      data-slot="select-scroll-down-button"
      className={cn(
        'flex cursor-default items-center justify-center py-1 text-ink-muted',
        className,
      )}
      {...props}
    >
      <ChevronDown className="size-4" aria-hidden="true" />
    </SelectPrimitive.ScrollDownButton>
  );
}

function SelectContent({
  className,
  children,
  position = 'popper',
  ...props
}: ComponentProps<typeof SelectPrimitive.Content>) {
  return (
    <SelectPrimitive.Portal>
      <SelectPrimitive.Content
        data-slot="select-content"
        position={position}
        className={cn(
          'relative z-50 max-h-[var(--radix-select-content-available-height)] min-w-32 overflow-hidden',
          'rounded-sheet border border-rule-hi bg-ground-2',
          'data-[state=open]:anim-pop-in data-[state=closed]:anim-pop-out',
          className,
        )}
        {...props}
      >
        <SelectScrollUpButton />
        <SelectPrimitive.Viewport
          className={cn(
            'p-1',
            position === 'popper' &&
              'h-[var(--radix-select-trigger-height)] w-full min-w-[var(--radix-select-trigger-width)] scroll-my-1',
          )}
        >
          {children}
        </SelectPrimitive.Viewport>
        <SelectScrollDownButton />
      </SelectPrimitive.Content>
    </SelectPrimitive.Portal>
  );
}

function SelectLabel({
  className,
  ...props
}: ComponentProps<typeof SelectPrimitive.Label>) {
  return (
    <SelectPrimitive.Label
      data-slot="select-label"
      className={cn('px-2 py-1.5 t-label text-ink-muted', className)}
      {...props}
    />
  );
}

function SelectItem({
  className,
  children,
  ...props
}: ComponentProps<typeof SelectPrimitive.Item>) {
  return (
    <SelectPrimitive.Item
      data-slot="select-item"
      className={cn(
        'relative flex w-full cursor-default items-center gap-2 rounded-price',
        /* `outline-none` is legal here and needs its replacement named: Radix
         * moves real DOM focus onto the highlighted item, so the focus ring and
         * the highlight would both fire. `data-[highlighted]` below IS the focus
         * indicator — it is not decoration, and it must not be removed. */
        'py-2 pr-8 pl-2 t-ui text-ink-2 outline-none select-none ui-transition',
        'data-[highlighted]:bg-ground-3 data-[highlighted]:text-ink',
        'data-[state=checked]:text-ink',
        'data-[disabled]:pointer-events-none data-[disabled]:text-ink-faint',
        className,
      )}
      {...props}
    >
      <span className="absolute right-2 flex size-4 items-center justify-center">
        <SelectPrimitive.ItemIndicator>
          <Check className="size-4" aria-hidden="true" />
        </SelectPrimitive.ItemIndicator>
      </span>
      <SelectPrimitive.ItemText>{children}</SelectPrimitive.ItemText>
    </SelectPrimitive.Item>
  );
}

function SelectSeparator({
  className,
  ...props
}: ComponentProps<typeof SelectPrimitive.Separator>) {
  return (
    <SelectPrimitive.Separator
      data-slot="select-separator"
      className={cn('-mx-1 my-1 h-px bg-rule', className)}
      {...props}
    />
  );
}

export {
  Select,
  SelectGroup,
  SelectValue,
  SelectTrigger,
  SelectContent,
  SelectLabel,
  SelectItem,
  SelectSeparator,
  SelectScrollUpButton,
  SelectScrollDownButton,
};
