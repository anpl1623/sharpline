'use client';

import type { ComponentProps } from 'react';
import * as SheetPrimitive from '@radix-ui/react-dialog';
import { cva, type VariantProps } from 'class-variance-authority';
import { X } from 'lucide-react';

import { cn } from '@/lib/utils';

/**
 * Sheet — a Radix Dialog that enters from an edge.
 *
 * All four sides exist because this product uses three of them:
 *   - `top`    the mobile status rail. Tapping the connection pip expands the
 *              full engineering readout — connection id, sequence number,
 *              channel count, staleness, provenance — from the top, in the same
 *              mono register and the same order as the desktop rail. Nothing is
 *              lost on mobile, only folded.
 *   - `right`  desktop side panels.
 *   - `bottom` the mobile slip container (phase 8 — its CONTENT is undesigned
 *              and deliberately so; DESIGN.md Open Decision #5).
 *
 * Radius: 6px, and only on the corners that meet the viewport INTERIOR. The
 * edges flush with the screen stay square, because a rounded corner against a
 * device bezel reads as a rendering mistake.
 *
 * Motion: 180ms in on the `enter` curve, 120ms out on `exit`. Transform and
 * opacity only.
 *
 * A11y: Radix requires a Title inside every Content. If a sheet has no visible
 * title, render `<SheetTitle className="sr-only">`; do not drop it.
 */
function Sheet(props: ComponentProps<typeof SheetPrimitive.Root>) {
  return <SheetPrimitive.Root data-slot="sheet" {...props} />;
}

function SheetTrigger(props: ComponentProps<typeof SheetPrimitive.Trigger>) {
  return <SheetPrimitive.Trigger data-slot="sheet-trigger" {...props} />;
}

function SheetClose(props: ComponentProps<typeof SheetPrimitive.Close>) {
  return <SheetPrimitive.Close data-slot="sheet-close" {...props} />;
}

function SheetPortal(props: ComponentProps<typeof SheetPrimitive.Portal>) {
  return <SheetPrimitive.Portal data-slot="sheet-portal" {...props} />;
}

function SheetOverlay({
  className,
  ...props
}: ComponentProps<typeof SheetPrimitive.Overlay>) {
  return (
    <SheetPrimitive.Overlay
      data-slot="sheet-overlay"
      className={cn(
        'fixed inset-0 z-50 bg-ground-0/80',
        'data-[state=open]:anim-fade-in data-[state=closed]:anim-fade-out',
        className,
      )}
      {...props}
    />
  );
}

const sheetVariants = cva(
  'fixed z-50 flex flex-col gap-4 border-rule bg-ground-1 p-6',
  {
    variants: {
      side: {
        top: [
          'inset-x-0 top-0 max-h-[85dvh] border-b rounded-b-sheet',
          'data-[state=open]:anim-slide-in-top data-[state=closed]:anim-slide-out-top',
        ],
        bottom: [
          'inset-x-0 bottom-0 max-h-[85dvh] border-t rounded-t-sheet',
          'data-[state=open]:anim-slide-in-bottom data-[state=closed]:anim-slide-out-bottom',
        ],
        right: [
          'inset-y-0 right-0 h-full w-3/4 max-w-sm border-l rounded-l-sheet',
          'data-[state=open]:anim-slide-in-right data-[state=closed]:anim-slide-out-right',
        ],
        left: [
          'inset-y-0 left-0 h-full w-3/4 max-w-sm border-r rounded-r-sheet',
          'data-[state=open]:anim-slide-in-left data-[state=closed]:anim-slide-out-left',
        ],
      },
    },
    defaultVariants: {
      side: 'right',
    },
  },
);

export type SheetContentProps = ComponentProps<typeof SheetPrimitive.Content> &
  VariantProps<typeof sheetVariants>;

function SheetContent({
  className,
  children,
  side = 'right',
  ...props
}: SheetContentProps) {
  return (
    <SheetPortal>
      <SheetOverlay />
      <SheetPrimitive.Content
        data-slot="sheet-content"
        className={cn(sheetVariants({ side }), className)}
        {...props}
      >
        {children}
        <SheetPrimitive.Close
          data-slot="sheet-close-button"
          aria-label="Close"
          className={cn(
            'absolute top-4 right-4 inline-flex size-8 items-center justify-center',
            'rounded-price text-ink-muted ui-transition',
            'hover:bg-ground-2 hover:text-ink',
          )}
        >
          <X className="size-4" aria-hidden="true" />
        </SheetPrimitive.Close>
      </SheetPrimitive.Content>
    </SheetPortal>
  );
}

function SheetHeader({ className, ...props }: ComponentProps<'div'>) {
  return (
    <div
      data-slot="sheet-header"
      className={cn('flex flex-col gap-1', className)}
      {...props}
    />
  );
}

function SheetFooter({ className, ...props }: ComponentProps<'div'>) {
  return (
    <div
      data-slot="sheet-footer"
      className={cn('mt-auto flex flex-col gap-2', className)}
      {...props}
    />
  );
}

function SheetTitle({
  className,
  ...props
}: ComponentProps<typeof SheetPrimitive.Title>) {
  return (
    <SheetPrimitive.Title
      data-slot="sheet-title"
      className={cn('t-h3 text-ink', className)}
      {...props}
    />
  );
}

function SheetDescription({
  className,
  ...props
}: ComponentProps<typeof SheetPrimitive.Description>) {
  return (
    <SheetPrimitive.Description
      data-slot="sheet-description"
      className={cn('t-body text-ink-muted', className)}
      {...props}
    />
  );
}

export {
  Sheet,
  SheetTrigger,
  SheetClose,
  SheetPortal,
  SheetOverlay,
  SheetContent,
  SheetHeader,
  SheetFooter,
  SheetTitle,
  SheetDescription,
  sheetVariants,
};
