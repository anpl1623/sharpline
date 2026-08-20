/**
 * Barrel for the design-system primitives.
 *
 * These are shadcn/ui components, vendored and restyled to DESIGN.md — not a
 * theme layered over stock shadcn. The defaults that differ (2px control radius,
 * 44px control height, no shadows, no accent-coloured hover) are design-system
 * rules, and each one is argued at the top of its own file.
 *
 * This list is closed. Adding a primitive nobody asked for is how a design
 * system turns into a component library with no product behind it.
 */
export { Badge, badgeVariants } from './badge';
export type { BadgeProps } from './badge';
export { Button, buttonVariants } from './button';
export type { ButtonProps } from './button';
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
} from './dropdown-menu';
export { Input } from './input';
export { Label } from './label';
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
} from './select';
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
} from './sheet';
export type { SheetContentProps } from './sheet';
export { Skeleton } from './skeleton';
export { Tabs, TabsList, TabsTrigger, TabsContent } from './tabs';
export {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
  TooltipProvider,
} from './tooltip';
