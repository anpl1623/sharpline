'use client';

/**
 * Create an account.
 *
 * Two fields, and there will never be more. `/auth/register` takes an email and
 * a password because CLAUDE.md section 0 rules out KYC entirely and migration
 * 00005 has no column for a name, an address, a date of birth or a document.
 * The absence is the point, and it is stated on the card rather than left to be
 * noticed.
 *
 * WHERE THIS DELIBERATELY DIFFERS FROM SIGN-IN: a duplicate address is reported
 * plainly. `/auth/register` leaks the existence of an address on purpose and
 * boundedly — a registration form that silently accepts a duplicate is
 * unusable, and the same fact falls out of any password-reset flow. `/auth/login`
 * leaks nothing, which is where it matters, so the two surfaces are allowed to
 * say different things.
 *
 * No token is read, rendered or logged here; see ./login-form.
 */

import { useId, useState } from 'react';
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
  PASSWORD_MIN_LENGTH,
  SIGN_IN_PATH,
  afterAuthPath,
  describeAuthFailure,
  describedBy,
  emailProblem,
  fieldValue,
  firstFieldWithError,
  focusField,
  hasFieldError,
  newPasswordProblem,
  withNextParam,
} from '@/components/auth/auth-card';
import type { AuthFailure, FieldErrors } from '@/components/auth/auth-card';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { useAuth } from '@/lib/store/auth';

export interface RegisterFormProps {
  /** Already validated by `safeNextPath` in the page that renders this. */
  readonly next: string | null;
}

export function RegisterForm({ next }: RegisterFormProps) {
  const router = useRouter();
  const register = useAuth((state) => state.register);

  const uid = useId();
  const emailId = `${uid}-email`;
  const emailErrorId = `${uid}-email-error`;
  const passwordId = `${uid}-password`;
  const passwordHintId = `${uid}-password-hint`;
  const passwordErrorId = `${uid}-password-error`;
  const alertId = `${uid}-alert`;

  const [pending, setPending] = useState(false);
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>(NO_FIELD_ERRORS);
  const [failure, setFailure] = useState<AuthFailure | null>(null);

  async function submit(event: FormEvent<HTMLFormElement>): Promise<void> {
    event.preventDefault();
    if (pending) return;

    const form = event.currentTarget;
    const data = new FormData(form);
    const email = fieldValue(data, FIELD_NAME.email).trim();
    const password = fieldValue(data, FIELD_NAME.password);

    const problems: FieldErrors = {
      email: emailProblem(email),
      password: newPasswordProblem(password),
      totp: null,
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
    const ok = await register(email, password);
    setPending(false);

    if (ok) {
      // Registration opens a session, so this lands on the board signed in
      // rather than bouncing through the sign-in form.
      router.replace(afterAuthPath(next));
      return;
    }

    const described = describeAuthFailure(useAuth.getState().error, 'register');
    setFailure(described);
    focusField(form, described.field);
  }

  const emailInvalid = fieldErrors.email !== null || failure?.field === 'email';
  const passwordInvalid =
    fieldErrors.password !== null || failure?.field === 'password';

  return (
    <AuthCard
      title="Create an account"
      intro="Email and password only. No identity checks, no deposits, and no real money — this is a simulation."
      footer={
        <p className="t-ui text-ink-muted">
          Already have an account?{' '}
          <Link
            href={withNextParam(SIGN_IN_PATH, next)}
            className="text-ink-2 underline underline-offset-4 ui-transition hover:text-ink"
          >
            Sign in
          </Link>
          .
        </p>
      }
    >
      {/*
        `noValidate` turns off the browser's own bubbles so every message on
        this form comes from one place and is wired to its field with
        `aria-invalid` and `aria-describedby`. The constraint attributes stay on
        the inputs: they are what a password manager reads, and `required` /
        `minLength` still carry semantics to assistive technology.
      */}
      <form
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
            autoComplete="new-password"
            minLength={PASSWORD_MIN_LENGTH}
            maxLength={PASSWORD_MAX_LENGTH}
            aria-invalid={passwordInvalid}
            aria-describedby={describedBy(
              passwordHintId,
              fieldErrors.password !== null ? passwordErrorId : null,
              failure?.field === 'password' ? alertId : null,
            )}
          />
          {/*
            The rule is stated up front rather than sprung as an error. There is
            one rule and it is length: `internal/auth/password.go` sets a 12
            character floor and no composition requirements, because character
            classes push people toward `Password1!` and toward reuse.
          */}
          <p id={passwordHintId} className="t-ui text-ink-muted">
            At least {PASSWORD_MIN_LENGTH} characters. Length is the only rule.
          </p>
          <FieldError id={passwordErrorId} message={fieldErrors.password} />
        </div>

        <Button
          type="submit"
          disabled={pending}
          aria-busy={pending}
          className="mt-2 w-full"
        >
          {pending ? 'Creating account…' : 'Create account'}
        </Button>
      </form>
    </AuthCard>
  );
}
