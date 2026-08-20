'use client';

/**
 * The header's account chip. Three states and no fourth.
 *
 * # It never renders the wrong state, and that is the hard part
 *
 * The access token lives in memory and the refresh token in `localStorage`, so
 * the server renders every page signed out — it has no storage to read — and
 * the client only learns who is signed in after two asynchronous steps: the
 * store rehydrates from storage, and then exchanges the stored refresh token
 * for an access token. Rendering "Sign in" during either step is a visible lie
 * that flips to an email a moment later, and rendering the email early is a
 * hydration mismatch.
 *
 * So there is an explicit RESOLVING state covering both steps, and it renders a
 * placeholder of the same size. The signed-out branch is reached only once the
 * store has hydrated AND there is no stored session left to redeem, which means
 * a returning user never sees "Sign in" flash before their own address.
 *
 * # Nothing here holds a token
 *
 * Every selector below returns a BOOLEAN derived inside the store. The token
 * strings are never read into this component, so they cannot reach a DOM
 * attribute, a devtools props panel, or a render log.
 *
 * # This component does NOT call `useAuthHydration`
 *
 * That hook must be called exactly ONCE, in the root client shell. Calling it
 * twice would let two effects each see `status: 'anonymous'` in the same commit
 * and both call `refresh()` with the same token — and a refresh token replayed
 * is, by design, indistinguishable from a stolen one, so the server would
 * revoke the whole family and sign the user out. The shell owns that call.
 */

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { ChevronDown } from 'lucide-react';

import { signInHref } from '@/components/auth/auth-card';
import { Button } from '@/components/ui/button';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { Skeleton } from '@/components/ui/skeleton';
import { cn } from '@/lib/utils';
import { useAuth } from '@/lib/store/auth';

export interface AccountMenuProps {
  /** Composed onto the rendered control in every state, including the placeholder. */
  readonly className?: string | undefined;
}

export function AccountMenu({ className }: AccountMenuProps) {
  const pathname = usePathname();

  // Booleans only — see the note above.
  const hydrated = useAuth((state) => state.hydrated);
  const status = useAuth((state) => state.status);
  const signedIn = useAuth((state) => state.accessToken !== null);
  const hasStoredSession = useAuth(
    (state) => state.refreshToken !== null && state.refreshToken !== '',
  );
  const email = useAuth((state) => state.account?.email ?? null);
  const logout = useAuth((state) => state.logout);

  const resolving =
    !hydrated ||
    status === 'authenticating' ||
    status === 'refreshing' ||
    (!signedIn && hasStoredSession);

  if (resolving) {
    // Same 36px height as the `sm` button below, so the header does not reflow
    // when the answer arrives. `Skeleton` is deliberately static: the whole
    // motion budget belongs to the delta rail.
    return <Skeleton className={cn('h-9 w-28', className)} />;
  }

  if (!signedIn) {
    return (
      <Button asChild variant="ghost" size="sm" className={className}>
        <Link href={signInHref(pathname)}>Sign in</Link>
      </Button>
    );
  }

  // `account` is set from the session response by every path that produces a
  // token, so this is a fallback for a shape that should not occur rather than
  // a state with its own design.
  const label = email ?? 'Account';

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="ghost"
          size="sm"
          className={cn('max-w-[15rem] gap-1', className)}
        >
          <span className="sr-only">Account menu for </span>
          <span className="truncate">{label}</span>
          <ChevronDown aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>

      <DropdownMenuContent
        align="end"
        aria-label={`Signed in as ${label}`}
        className="min-w-56"
      >
        <div className="px-2 py-1.5">
          <p className="t-label text-ink-muted">Signed in as</p>
          <p className="mt-1 t-ui break-all text-ink">{label}</p>
        </div>
        <DropdownMenuSeparator />
        {/* `asChild` so the item IS the link: a `<div role="menuitem">` with an
            onSelect that navigates cannot be opened in a new tab, cannot be
            copied, and has no href for a screen reader to announce. */}
        <DropdownMenuItem asChild>
          <Link href="/bets">Your bets</Link>
        </DropdownMenuItem>
        {/* CLV is per-account and reachable nowhere else — it is not a section
          * of the product, it is a reading of this customer's own tickets, so it
          * belongs beside them rather than in the section nav. */}
        <DropdownMenuItem asChild>
          <Link href="/account/clv">Your closing line value</Link>
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          onSelect={() => {
            void logout();
          }}
        >
          Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
