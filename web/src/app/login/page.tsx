import type { Metadata } from 'next';

import { firstParam, safeNextPath } from '@/components/auth/auth-card';
import { LoginForm } from '@/components/auth/login-form';

/**
 * Sign in.
 *
 * A server component, and it is one for a single reason: `?next=` is validated
 * HERE, before it reaches a component that could navigate with it. Reading it
 * on the client with `useSearchParams` would work equally well, but it would
 * put the open-redirect guard behind a Suspense boundary and one refactor away
 * from being skipped. The page hands the form a value that is already known to
 * be a same-origin path, or `null`.
 *
 * Awaiting `searchParams` makes this route dynamic, which is correct — there is
 * nothing to prerender on a form whose only variable is a redirect target.
 */
export const metadata: Metadata = {
  title: 'Sign in',
};

type SearchParams = Record<string, string | string[] | undefined>;

export default async function LoginPage({
  searchParams,
}: {
  searchParams: Promise<SearchParams>;
}) {
  const params = await searchParams;
  const next = safeNextPath(firstParam(params['next']));

  return <LoginForm next={next} />;
}
