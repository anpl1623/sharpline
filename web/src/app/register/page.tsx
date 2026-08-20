import type { Metadata } from 'next';

import { firstParam, safeNextPath } from '@/components/auth/auth-card';
import { RegisterForm } from '@/components/auth/register-form';

/**
 * Create an account.
 *
 * Same shape as the sign-in page and for the same reason: `?next=` is validated
 * on the server, before anything can navigate with it. Registration opens a
 * session on success, so it honours the same redirect target rather than
 * handing the user to the sign-in form they just avoided.
 */
export const metadata: Metadata = {
  title: 'Create an account',
};

type SearchParams = Record<string, string | string[] | undefined>;

export default async function RegisterPage({
  searchParams,
}: {
  searchParams: Promise<SearchParams>;
}) {
  const params = await searchParams;
  const next = safeNextPath(firstParam(params['next']));

  return <RegisterForm next={next} />;
}
