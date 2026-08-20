/**
 * The auth surface's shared vocabulary: the card shell, the two error
 * presentations, the redirect guard, and the field rules.
 *
 * It is one module and not five because every piece here is used by BOTH forms
 * and by the header's account chip, and because the redirect guard in
 * particular is a security control that must have exactly one implementation to
 * be reviewable. The filename says "card"; read it as "the auth surface".
 *
 * NOTE FOR THE SERVER: this module deliberately carries NO `'use client'` and
 * NO hooks. `web/src/app/login/page.tsx` is a server component and calls
 * `safeNextPath` during the render — a `'use client'` boundary here would turn
 * that import into a client reference and the call would fail at request time.
 *
 * Nothing in this file touches a token. The access token lives in memory in the
 * auth store and never reaches a component that renders.
 */

import Link from 'next/link';
import type { ReactNode } from 'react';

import { developerDetail, isApiError } from '@/lib/api/errors';

// -----------------------------------------------------------------------------
// Routes
// -----------------------------------------------------------------------------

export const SIGN_IN_PATH = '/login';
export const REGISTER_PATH = '/register';

/** Where a session lands when the caller did not ask for somewhere specific. */
export const AFTER_AUTH_PATH = '/board';

/**
 * A `?next=` value long enough to be a real path and short enough that nobody
 * is smuggling a payload through it.
 */
const MAX_NEXT_LENGTH = 512;

/**
 * Validates a post-authentication redirect target.
 *
 * THIS IS A SECURITY CONTROL, not a convenience. An unvalidated `?next=` is a
 * textbook open redirect: an attacker sends `/login?next=https://evil.example`,
 * the victim signs in on the real origin — correct URL, correct TLS, correct
 * password-manager prompt — and is then handed to a page that can ask for the
 * credential again with total credibility.
 *
 * The rule is therefore an allowlist, not a denylist: the value must be a
 * SAME-ORIGIN ABSOLUTE PATH and nothing else.
 *
 *   - It must start with exactly one `/`. `//evil.example` is a
 *     protocol-relative URL and leaves this origin, and so does `/\evil.example`
 *     once a browser normalises the backslash — both are rejected, and so is
 *     any string containing a backslash at all.
 *   - No C0 control characters and no space anywhere. The URL parser STRIPS tab,
 *     newline and carriage return before resolving, which is exactly how
 *     `/<tab>/evil.example` becomes `//evil.example` after this function has
 *     looked at it. Rejecting the whole class is the only stable answer.
 *   - `/login` and `/register` are rejected as targets so a stale link cannot
 *     bounce a freshly signed-in user back to the form.
 *
 * Returns the path, or `null` when there is nothing safe to use — callers then
 * fall back to {@link AFTER_AUTH_PATH}.
 */
export function safeNextPath(raw: unknown): string | null {
  if (typeof raw !== 'string') return null;
  if (raw === '' || raw.length > MAX_NEXT_LENGTH) return null;
  if (!raw.startsWith('/')) return null;
  if (raw.startsWith('//') || raw.startsWith('/\\')) return null;
  if (raw.includes('\\')) return null;

  for (let i = 0; i < raw.length; i += 1) {
    const code = raw.charCodeAt(i);
    if (code <= 0x20 || code === 0x7f) return null;
  }

  const cut = raw.search(/[?#]/);
  const pathOnly = cut === -1 ? raw : raw.slice(0, cut);
  if (pathOnly === SIGN_IN_PATH || pathOnly === REGISTER_PATH) return null;

  return raw;
}

/** The first value of a `searchParams` entry, which may be repeated. */
export function firstParam(
  value: string | readonly string[] | undefined,
): string | null {
  if (typeof value === 'string') return value;
  if (value === undefined) return null;
  return value[0] ?? null;
}

/** `base`, carrying a validated `next` if there is one. */
export function withNextParam(base: string, next: string | null): string {
  if (next === null) return base;
  return `${base}?next=${encodeURIComponent(next)}`;
}

/**
 * The href for a "Sign in" control rendered on `currentPath`, so that signing
 * in returns the user to where they were. The path goes through the same guard
 * as an inbound `?next=`, because `usePathname()` on a catch-all route is still
 * a value this app did not choose.
 */
export function signInHref(currentPath: string): string {
  const next = safeNextPath(currentPath);
  if (next === null || next === '/') return SIGN_IN_PATH;
  return withNextParam(SIGN_IN_PATH, next);
}

/** Where to send a session that has just been opened. */
export function afterAuthPath(next: string | null): string {
  return next ?? AFTER_AUTH_PATH;
}

// -----------------------------------------------------------------------------
// Field rules
// -----------------------------------------------------------------------------

export type AuthField = 'email' | 'password' | 'totp';

/** The `name` attribute each field carries, so focus can find it in the form. */
export const FIELD_NAME: Readonly<Record<AuthField, string>> = {
  email: 'email',
  password: 'password',
  totp: 'totp_code',
};

export type FieldErrors = { readonly [K in AuthField]: string | null };

export const NO_FIELD_ERRORS: FieldErrors = {
  email: null,
  password: null,
  totp: null,
};

export function hasFieldError(errors: FieldErrors): boolean {
  return (
    errors.email !== null || errors.password !== null || errors.totp !== null
  );
}

export function firstFieldWithError(errors: FieldErrors): AuthField | null {
  if (errors.email !== null) return 'email';
  if (errors.password !== null) return 'password';
  if (errors.totp !== null) return 'totp';
  return null;
}

/**
 * `internal/auth/email.go`: 3–254 bytes, exactly one `@`, both sides non-empty
 * and whitespace-free. Mirrored here so the common typo is caught without a
 * round trip — the server is still the authority and still re-checks.
 */
const EMAIL_MIN_LENGTH = 3;
export const EMAIL_MAX_LENGTH = 254;

/**
 * `internal/auth/password.go`: 12–1024 bytes, and NO composition rules. Length
 * beats character classes, which push people toward `Password1!` and toward
 * reuse.
 */
export const PASSWORD_MIN_LENGTH = 12;
export const PASSWORD_MAX_LENGTH = 1024;

export const TOTP_CODE_LENGTH = 6;

export function emailProblem(value: string): string | null {
  if (value === '') return 'Enter your email address.';
  if (value.length < EMAIL_MIN_LENGTH || value.length > EMAIL_MAX_LENGTH) {
    return 'That does not look like an email address.';
  }
  const at = value.indexOf('@');
  if (at <= 0 || at !== value.lastIndexOf('@') || at === value.length - 1) {
    return 'That does not look like an email address.';
  }
  for (let i = 0; i < value.length; i += 1) {
    const code = value.charCodeAt(i);
    if (code <= 0x20 || code === 0x7f) {
      return 'That does not look like an email address.';
    }
  }
  return null;
}

/** Sign-in checks only that something was typed: the rules are the server's. */
export function currentPasswordProblem(value: string): string | null {
  return value === '' ? 'Enter your password.' : null;
}

export function newPasswordProblem(value: string): string | null {
  if (value === '') return 'Choose a password.';
  if (value.length < PASSWORD_MIN_LENGTH) {
    return `Use at least ${String(PASSWORD_MIN_LENGTH)} characters.`;
  }
  if (value.length > PASSWORD_MAX_LENGTH) {
    return `Use at most ${String(PASSWORD_MAX_LENGTH)} characters.`;
  }
  return null;
}

export function totpProblem(value: string): string | null {
  if (value === '') return 'Enter the code from your authenticator app.';
  if (value.length !== TOTP_CODE_LENGTH || !/^[0-9]+$/.test(value)) {
    return `The code is ${String(TOTP_CODE_LENGTH)} digits.`;
  }
  return null;
}

// -----------------------------------------------------------------------------
// Form plumbing
// -----------------------------------------------------------------------------

/**
 * Reads one field out of a submitted form.
 *
 * The forms are UNCONTROLLED and read through `FormData` on submit rather than
 * mirrored into React state on every keystroke. That is deliberate: a password
 * manager that fills a field without dispatching the events React listens for
 * still produces the right `FormData`, and the values survive the re-render
 * that reveals the second-factor field without any state to carry them.
 */
export function fieldValue(data: FormData, name: string): string {
  const raw = data.get(name);
  return typeof raw === 'string' ? raw : '';
}

/**
 * Moves focus to a field by its `name`, found inside the submitted form.
 *
 * Focus IS the announcement mechanism for a field-scoped failure: landing on
 * the control makes a screen reader read its label, its invalid state and its
 * description in one go, with no second live region competing with the board's.
 * It also finds the second-factor field on the render that reveals it, which a
 * ref captured before that render could not.
 */
export function focusField(
  form: HTMLFormElement | null,
  field: AuthField | null,
): void {
  if (form === null || field === null) return;
  const element = form.querySelector<HTMLInputElement>(
    `[name="${FIELD_NAME[field]}"]`,
  );
  element?.focus();
}

/** An `aria-describedby` list, omitting the attribute when it would be empty. */
export function describedBy(
  ...ids: readonly (string | null)[]
): string | undefined {
  const present = ids.filter((id): id is string => id !== null);
  return present.length > 0 ? present.join(' ') : undefined;
}

// -----------------------------------------------------------------------------
// Failure copy
// -----------------------------------------------------------------------------

export type AuthSurface = 'login' | 'register';

export interface AuthFailure {
  /** The sentence a person reads. Never the server's own string verbatim. */
  readonly message: string;
  /** The field to mark invalid, or `null` when the failure is not a field's. */
  readonly field: AuthField | null;
  /** Whether the second-factor field must now be shown. */
  readonly requiresTotp: boolean;
  /** Developer-facing: code, status, request id. Belongs in a disclosure. */
  readonly detail: string | null;
}

/**
 * Maps a failed call onto copy.
 *
 * TWO ARMS ARE LOAD-BEARING AND MUST NOT BE "IMPROVED":
 *
 *  1. `invalid_credentials` says "Email or password is incorrect" and says the
 *     SAME THING for an address that has no account. `/auth/login` does not
 *     enumerate users — it runs a full argon2id verification against a decoy
 *     hash when no user is found so the timing matches — and copy that said
 *     "no account with that email" would hand back on the client exactly what
 *     the server spent that work hiding.
 *
 *  2. `totp_required` is NOT a credential failure. It is only reachable AFTER
 *     the password has been verified, so the right response is to ask for the
 *     code, not to claim the password was wrong.
 *
 * `already_exists` is the one place this surface may confirm an address exists,
 * and it does so plainly: `/auth/register` leaks that fact deliberately and
 * boundedly, because a registration form that silently accepts a duplicate is
 * unusable. Saying it vaguely would keep none of the secret and lose the user.
 */
export function describeAuthFailure(
  error: unknown,
  surface: AuthSurface,
): AuthFailure {
  const detail = developerDetail(error);

  if (!isApiError(error)) {
    return {
      message: 'Something went wrong. Try again.',
      field: null,
      requiresTotp: false,
      detail,
    };
  }

  const fromParams = fieldFromInvalidParams(error.invalidParams);

  switch (error.code) {
    case 'totp_required':
      return {
        message: 'Enter the 6-digit code from your authenticator app.',
        field: 'totp',
        requiresTotp: true,
        detail,
      };

    case 'invalid_totp_code':
      return {
        message:
          'That code was not accepted. Codes change every 30 seconds — try the current one.',
        field: 'totp',
        requiresTotp: true,
        detail,
      };

    case 'invalid_credentials':
    case 'unauthenticated':
      return {
        message: 'Email or password is incorrect.',
        field: null,
        requiresTotp: false,
        detail,
      };

    case 'already_exists':
      return {
        message: 'An account already exists for that address.',
        field: 'email',
        requiresTotp: false,
        detail,
      };

    case 'conflict':
      return {
        message: 'That conflicts with the current state of the account.',
        field: null,
        requiresTotp: false,
        detail,
      };

    case 'account_not_active':
    case 'forbidden':
      return {
        message:
          'This account cannot start a session. If you set a self-exclusion, it is still in force.',
        field: null,
        requiresTotp: false,
        detail,
      };

    case 'rate_limited':
      return {
        message: 'Too many attempts. Wait a moment, then try again.',
        field: null,
        requiresTotp: false,
        detail,
      };

    case 'bad_request':
    case 'invalid_parameter':
    case 'unprocessable':
    case 'invalid_cursor':
      return {
        message:
          surface === 'register'
            ? `Check the email address, and use a password of at least ${String(PASSWORD_MIN_LENGTH)} characters.`
            : 'Check the email address and password, then try again.',
        field: fromParams,
        requiresTotp: false,
        detail,
      };

    case 'not_found':
      return {
        message: 'That endpoint is not available.',
        field: null,
        requiresTotp: false,
        detail,
      };

    case 'timeout':
      return {
        message: 'The server did not answer in time. Try again.',
        field: null,
        requiresTotp: false,
        detail,
      };

    case 'aborted':
      return {
        message: 'That request was cancelled.',
        field: null,
        requiresTotp: false,
        detail,
      };

    case 'network':
      return {
        message: 'The server could not be reached. Check your connection.',
        field: null,
        requiresTotp: false,
        detail,
      };

    case 'internal':
    case 'malformed_response':
      return {
        message: 'The server failed to answer. Try again shortly.',
        field: null,
        requiresTotp: false,
        detail,
      };

    default:
      return {
        message: 'Something went wrong. Try again.',
        field: null,
        requiresTotp: false,
        detail,
      };
  }
}

/**
 * `invalid_params` is present only on `400`/`422`, and the auth handlers do not
 * currently emit it — a malformed email and a short password both arrive as a
 * bare `invalid_parameter`, on purpose, because naming the violated rule on the
 * LOGIN path would let an attacker probing with a deliberately invalid password
 * distinguish "no such account" from "wrong password". This reads the list
 * anyway so the day one appears, it lands on the right field instead of being
 * silently dropped.
 */
function fieldFromInvalidParams(
  params: readonly { readonly name: string }[],
): AuthField | null {
  for (const param of params) {
    const name = param.name.replace(/^\/+/, '').toLowerCase();
    if (name === 'email') return 'email';
    if (name === 'password') return 'password';
    if (name === 'totp_code' || name === 'totp') return 'totp';
  }
  return null;
}

// -----------------------------------------------------------------------------
// Presentation
// -----------------------------------------------------------------------------

const HEADING_ID = 'auth-card-heading';

export interface AuthCardProps {
  readonly title: string;
  readonly intro: string;
  readonly children: ReactNode;
  readonly footer: ReactNode;
}

/**
 * The shell both auth pages sit in: one 380px card on the page ground, a
 * 4px radius, a hairline, and nothing else. No hero, no illustration, no
 * social-login row for providers that do not exist.
 *
 * It renders a `<section>` and not a `<main>` on purpose — the app shell owns
 * the page's landmark, and two `<main>` elements is an accessibility defect
 * that is invisible until someone tabs through with a screen reader.
 */
export function AuthCard({ title, intro, children, footer }: AuthCardProps) {
  return (
    <div className="flex min-h-[32rem] w-full flex-col items-center justify-center gap-6 px-4 py-16">
      <section
        aria-labelledby={HEADING_ID}
        className="w-full max-w-[380px] rounded-card border border-rule bg-ground-1 p-6"
      >
        <p className="t-label text-ink-muted">Sharpline</p>
        <h1 id={HEADING_ID} className="mt-2 t-h2 text-ink">
          {title}
        </h1>
        <p className="mt-2 t-body text-ink-2">{intro}</p>
        <div className="mt-6">{children}</div>
      </section>

      <div className="flex w-full max-w-[380px] flex-col gap-2">
        {footer}
        {/*
          CLAUDE.md section 0: the "simulation, not a licensed sportsbook"
          distinction is stated wherever an account is created or entered, not
          only on the landing page. It is the sentence that makes this project
          a credential rather than a liability.
        */}
        <p className="t-ui text-ink-muted">
          Play money only.{' '}
          <Link
            href="/"
            className="text-ink-2 underline underline-offset-4 ui-transition hover:text-ink"
          >
            Sharpline is a simulation, not a licensed sportsbook
          </Link>
          .
        </p>
      </div>
    </div>
  );
}

export interface AuthFormErrorProps {
  readonly id: string;
  readonly message: string;
  readonly detail: string | null;
}

/**
 * The form-level failure.
 *
 * `role="alert"` sits on the SENTENCE and not on the box around it. An alert is
 * atomic — assistive technology reads the whole element — so putting it on the
 * container would read the disclosure's "Details" summary out loud alongside
 * the message every time a sign-in fails.
 *
 * `loss` is the token for "something is wrong" and is the only red on this
 * surface. DESIGN.md keeps red off every price and out of every other job.
 *
 * The `request_id` lives in a closed disclosure and never in the sentence. It
 * is the handle on the server log line and the trace span, and it is meaningless
 * to the person trying to sign in.
 */
export function AuthFormError({ id, message, detail }: AuthFormErrorProps) {
  return (
    <div className="rounded-price border border-loss/40 bg-loss/8 px-3 py-2">
      <p id={id} role="alert" className="t-ui text-loss">
        {message}
      </p>
      {detail === null ? null : (
        <details className="mt-2">
          <summary className="t-label text-ink-muted">Details</summary>
          <p className="mt-1 t-mono break-all text-ink-muted">{detail}</p>
        </details>
      )}
    </div>
  );
}

export interface FieldErrorProps {
  readonly id: string;
  readonly message: string | null;
}

/** A per-field message, referenced by that field's `aria-describedby`. */
export function FieldError({ id, message }: FieldErrorProps) {
  if (message === null) return null;
  return (
    <p id={id} className="t-ui text-loss">
      {message}
    </p>
  );
}
