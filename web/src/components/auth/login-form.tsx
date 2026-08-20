'use client';

/**
 * Sign in.
 *
 * Three properties this file exists to get right:
 *
 *  1. IT DOES NOT ENUMERATE USERS. `/auth/login` returns byte-identical
 *     responses for an unknown address and a wrong password, having done the
 *     same argon2id work in both cases. The copy for that arm lives in
 *     `describeAuthFailure` and says "Email or password is incorrect" for both.
 *
 *  2. A `totp_required` 401 IS NOT A CREDENTIAL FAILURE. It is only reachable
 *     after the password has been verified, so it reveals the second-factor
 *     field and re-submits instead of claiming the password was wrong.
 *
 *  3. NO TOKEN TOUCHES THIS COMPONENT. `useAuth.login` puts the access token in
 *     memory and the refresh token in the store's persisted slot; neither value
 *     is read here, rendered, logged, or put in a URL. The only thing this file
 *     learns is whether the call succeeded.
 *
 * The form is UNCONTROLLED and read through `FormData` on submit. See
 * `fieldValue` in ./auth-card for why.
 */

import { useEffect, useId, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import Link from 'next/link';
import { useRouter } from 'next/navigation';

import {
  AuthCard,
  AuthFormError,
  EMAIL_MAX_LENGTH,
  FIELD_NAME,
  FieldError,
  NO_FIELD_ERRORS,
  PASSWORD_MAX_LENGTH,
  REGISTER_PATH,
  TOTP_CODE_LENGTH,
  afterAuthPath,
  currentPasswordProblem,
  describeAuthFailure,
  describedBy,
  emailProblem,
  fieldValue,
  firstFieldWithError,
  focusField,
  hasFieldError,
  totpProblem,
  withNextParam,
} from '@/components/auth/auth-card';
import type { AuthFailure, FieldErrors } from '@/components/auth/auth-card';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { useAuth } from '@/lib/store/auth';

export interface LoginFormProps {
  /**
   * Where to go after a successful sign-in. ALREADY VALIDATED by
   * `safeNextPath` in the page that renders this — a raw `?next=` must never
   * reach a `router.replace`.
   */
  readonly next: string | null;
}

export function LoginForm({ next }: LoginFormProps) {
  const router = useRouter();
  const login = useAuth((state) => state.login);

  const uid = useId();
  const emailId = `${uid}-email`;
  const emailErrorId = `${uid}-email-error`;
  const passwordId = `${uid}-password`;
  const passwordErrorId = `${uid}-password-error`;
  const totpId = `${uid}-totp`;
  const totpHintId = `${uid}-totp-hint`;
  const totpErrorId = `${uid}-totp-error`;
  const alertId = `${uid}-alert`;

  const formRef = useRef<HTMLFormElement | null>(null);
  const [pending, setPending] = useState(false);
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>(NO_FIELD_ERRORS);
  const [failure, setFailure] = useState<AuthFailure | null>(null);
  const [totpRequired, setTotpRequired] = useState(false);

  // The second-factor field is revealed by a failed submit. Focus has to follow
  // it: a field that appears silently below the button is one a keyboard or
  // screen-reader user will not find.
  useEffect(() => {
    if (!totpRequired) return;
    focusField(formRef.current, 'totp');
  }, [totpRequired]);

  async function submit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (pending) return;

    // Captured synchronously: `event.currentTarget` is only valid during
    // dispatch and is gone by the time the await below resolves.
    const form = event.currentTarget;
    const data = new FormData(form);
    const email = fieldValue(data, FIELD_NAME.email).trim();
    const password = fieldValue(data, FIELD_NAME.password);
    const code = fieldValue(data, FIELD_NAME.totp).trim();

    const problems: FieldErrors = {
      email: emailProblem(email),
      password: currentPasswordProblem(password),
      totp: totpRequired ? totpProblem(code) : null,
    };
    if (hasFieldError(problems)) {
      setFieldErrors(problems);
      setFailure(null);
      focusField(form, firstFieldWithError(problems));
      return;
    }

    setFieldErrors(NO_FIELD_ERRORS);
    setFailure(null);
    setPending(true);
    const ok = await login(email, password, totpRequired ? code : undefined);
    setPending(false);

    if (ok) {
      // `replace`, not `push`: the back button must not return to a form the
      // user has already cleared.
      router.replace(afterAuthPath(next));
      return;
    }

    // The store's actions never throw — they record the failure and return
    // false — so the error is read from the store rather than caught.
    const described = describeAuthFailure(useAuth.getState().error, 'login');
    setFailure(described);
    if (described.requiresTotp && !totpRequired) {
      setTotpRequired(true);
      return;
    }
    focusField(form, described.field);
  }

  const emailInvalid = fieldErrors.email !== null || failure?.field === 'email';
  const passwordInvalid =
    fieldErrors.password !== null || failure?.field === 'password';
  const totpInvalid = fieldErrors.totp !== null || failure?.field === 'totp';

  return (
    <AuthCard
      title="Sign in"
      intro="The odds board is public. An account is only for keeping your own history on this simulation."
      footer={
        <p className="t-ui text-ink-muted">
          No account?{' '}
          <Link
            href={withNextParam(REGISTER_PATH, next)}
            className="text-ink-2 underline underline-offset-4 ui-transition hover:text-ink"
          >
            Create one
          </Link>
          .
        </p>
      }
    >
      {/*
        `noValidate` turns off the browser's own bubbles so every message on this
        form comes from one place and is wired to its field with `aria-invalid`
        and `aria-describedby`. The constraint attributes stay on the inputs:
        they are what a password manager reads, and `required` / `type` still
        carry semantics to assistive technology.
      */}
      <form
        ref={formRef}
        noValidate
        onSubmit={(event) => {
          void submit(event);
        }}
        className="flex flex-col gap-4"
      >
        {failure === null ? null : (
          <AuthFormError
            id={alertId}
            message={failure.message}
            detail={failure.detail}
          />
        )}

        <div className="flex flex-col gap-2">
          <Label htmlFor={emailId}>Email</Label>
          <Input
            id={emailId}
            name={FIELD_NAME.email}
            type="email"
            required
            autoComplete="email"
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            inputMode="email"
            maxLength={EMAIL_MAX_LENGTH}
            aria-invalid={emailInvalid}
            aria-describedby={describedBy(
              fieldErrors.email !== null ? emailErrorId : null,
              failure?.field === 'email' ? alertId : null,
            )}
          />
          <FieldError id={emailErrorId} message={fieldErrors.email} />
        </div>

        <div className="flex flex-col gap-2">
          <Label htmlFor={passwordId}>Password</Label>
          <Input
            id={passwordId}
            name={FIELD_NAME.password}
            type="password"
            required
            autoComplete="current-password"
            maxLength={PASSWORD_MAX_LENGTH}
            aria-invalid={passwordInvalid}
            aria-describedby={describedBy(
              fieldErrors.password !== null ? passwordErrorId : null,
              failure?.field === 'password' ? alertId : null,
            )}
          />
          <FieldError id={passwordErrorId} message={fieldErrors.password} />
        </div>

        {/*
          Revealed only after the server says a second factor is required. It is
          not rendered speculatively: most accounts have no TOTP enrolment, and
          a permanently visible code field on a sign-in form reads as a bug.
        */}
        {totpRequired ? (
          <div className="flex flex-col gap-2">
            <Label htmlFor={totpId}>Authentication code</Label>
            <Input
              id={totpId}
              name={FIELD_NAME.totp}
              type="text"
              required
              autoComplete="one-time-code"
              inputMode="numeric"
              pattern="[0-9]*"
              maxLength={TOTP_CODE_LENGTH}
              className="max-w-[9rem] tabular tracking-[0.32em]"
              aria-invalid={totpInvalid}
              aria-describedby={describedBy(
                totpHintId,
                fieldErrors.totp !== null ? totpErrorId : null,
                failure?.field === 'totp' ? alertId : null,
              )}
            />
            <p id={totpHintId} className="t-ui text-ink-muted">
              The {TOTP_CODE_LENGTH}-digit code from your authenticator app.
            </p>
            <FieldError id={totpErrorId} message={fieldErrors.totp} />
          </div>
        ) : null}

        <Button
          type="submit"
          disabled={pending}
          aria-busy={pending}
          className="mt-2 w-full"
        >
          {pending ? 'Signing in…' : 'Sign in'}
        </Button>
      </form>
    </AuthCard>
  );
}
