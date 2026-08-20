// ---------------------------------------------------------------------------
// The critical path's first half: getting a real account.
// ---------------------------------------------------------------------------
// There is no seeding, no fixture user and no back door. Every run registers a
// brand-new account against the live API, which is the only way to assert the
// signed-in board is genuinely reachable rather than mocked. Registering the
// same address twice is a 409, hence the unique address per call.
// ---------------------------------------------------------------------------

import { expect, type Locator, type Page } from '@playwright/test';
import { LOGIN_PATHS, REGISTER_PATHS, ROUTES, uniqueEmail, uniquePassword } from './env';
import {
  accountMenu,
  emailField,
  LOGIN_SUBMIT,
  passwordField,
  REGISTER_LINK,
  REGISTER_SUBMIT,
  signInControl,
  signOutControl,
  submitControl,
} from './selectors';

export interface Credentials {
  readonly email: string;
  readonly password: string;
}

export type AuthFormKind = 'register' | 'login';

/**
 * Open the register or login form.
 *
 * Canonical route first (`/register`, `/login`); if the app puts it elsewhere,
 * fall back to following the header link by accessible name. Two strategies,
 * both declared — a route rename degrades to the slower path instead of
 * failing, and the failure message names both when neither works.
 */
export async function openAuthForm(page: Page, kind: AuthFormKind): Promise<void> {
  const paths = kind === 'register' ? REGISTER_PATHS : LOGIN_PATHS;

  for (const path of paths) {
    if (await tryPath(page, path)) return;
  }

  // Fallback: from the landing page, follow the link a signed-out visitor is
  // offered. This is the path a real user takes, so it must work regardless.
  await page.goto(ROUTES.landing, { waitUntil: 'domcontentloaded' });
  const entry = kind === 'register' ? page.getByRole('link', { name: REGISTER_LINK }).first() : signInControl(page);
  await entry.click();
  await expect(emailField(page)).toBeVisible({ timeout: 15_000 });
}

async function tryPath(page: Page, path: string): Promise<boolean> {
  const response = await page.goto(path, { waitUntil: 'domcontentloaded' }).catch(() => null);
  if (response !== null && response.status() >= 400) return false;
  try {
    await emailField(page).waitFor({ state: 'visible', timeout: 5_000 });
    return true;
  } catch {
    return false;
  }
}

/**
 * Register a fresh account and end up signed in.
 * Returns the credentials so a caller can log back in with them if it wants.
 */
export async function registerNewAccount(page: Page): Promise<Credentials> {
  const credentials: Credentials = { email: uniqueEmail(), password: uniquePassword() };

  await openAuthForm(page, 'register');
  await emailField(page).fill(credentials.email);
  await passwordField(page).fill(credentials.password);

  // Optional second field; filled only if the form has one.
  const confirm = page.getByLabel(/confirm password|repeat password|password again/iu);
  if ((await confirm.count()) > 0) {
    await confirm.first().fill(credentials.password);
  }

  await submitControl(page, REGISTER_SUBMIT).click();
  await expectSignedIn(page);

  return credentials;
}

/** Sign in with credentials that already exist. */
export async function signIn(page: Page, credentials: Credentials): Promise<void> {
  await openAuthForm(page, 'login');
  await emailField(page).fill(credentials.email);
  await passwordField(page).fill(credentials.password);
  await submitControl(page, LOGIN_SUBMIT).click();
  await expectSignedIn(page);
}

/**
 * The sign-out control, revealed if it lives inside an account menu.
 * Returns null when the page offers none — i.e. we are not signed in.
 */
export async function resolveSignOut(page: Page): Promise<Locator | null> {
  const direct = signOutControl(page);
  if (await direct.isVisible().catch(() => false)) return direct;

  const menu = accountMenu(page);
  if (await menu.isVisible().catch(() => false)) {
    await menu.click();
    try {
      await direct.waitFor({ state: 'visible', timeout: 5_000 });
      return direct;
    } catch {
      // fall through — the menu was something else
    }
  }
  return null;
}

/**
 * Signed-in is detected by the control that only exists in that state. This is
 * the least brittle available signal: a product cannot offer a working sign-out
 * without one, whatever it is called or wherever it is folded.
 */
export async function expectSignedIn(page: Page): Promise<void> {
  await expect
    .poll(async () => (await resolveSignOut(page)) !== null, {
      timeout: 25_000,
      message: 'expected a sign-out control to appear, proving the session is established',
    })
    .toBe(true);
}

export async function expectSignedOut(page: Page): Promise<void> {
  await expect(signInControl(page)).toBeVisible({ timeout: 20_000 });
  await expect(signOutControl(page)).toBeHidden();
}

/** Sign out and confirm the signed-out state. */
export async function signOut(page: Page): Promise<void> {
  const control = await resolveSignOut(page);
  expect(control, 'a sign-out control must be reachable while signed in').not.toBeNull();
  await control?.click();
  await expectSignedOut(page);
}
