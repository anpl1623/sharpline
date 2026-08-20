'use client';

/**
 * Session state.
 *
 * # Where the two tokens live, and why
 *
 * The ACCESS TOKEN is held IN MEMORY ONLY and is never persisted. It is a
 * short-lived JWT — minutes, not hours — and putting it in localStorage would
 * make every XSS on any page of this origin a durable account compromise rather
 * than a session-length one.
 *
 * The REFRESH TOKEN is persisted to localStorage. The API returns it in the
 * response body precisely so that a client can store it: the OpenAPI document
 * says both tokens are returned in the body and never set as cookies, because
 * this API is consumed cross-origin through a proxy and a cookie-bearing API is
 * a CSRF surface that would need a second mechanism to defend.
 *
 * That is a real trade and it is made deliberately. A refresh token in
 * localStorage is readable by any script on this origin; the mitigations are
 * that it ROTATES ON EVERY USE and that REUSE REVOKES THE WHOLE FAMILY, so a
 * stolen token is detectable and self-limiting. It is accepted here because this
 * is a play-money simulation with no deposits, no withdrawals, no custody of
 * funds and no personal data beyond an email address (CLAUDE.md §0) — the asset
 * being protected is a leaderboard position. A real book would put the refresh
 * token in a `HttpOnly; Secure; SameSite=Strict` cookie on a same-site API and
 * accept the CSRF work that comes with it.
 *
 * # Nothing in this file logs, traces, or serialises a token
 *
 * No token enters a URL, a query string, a `console` call, a DOM attribute, or
 * an error message. The only place either one is written is the `Authorization`
 * header and the persisted refresh slot.
 */

import { useEffect } from 'react';
import { create } from 'zustand';
import { createJSONStorage, persist } from 'zustand/middleware';

import { browserApi } from '@/lib/api/client';
import { isApiError } from '@/lib/api/errors';
import type { ApiError } from '@/lib/api/errors';
import type { SchemaAccount, SchemaSessionResponse } from '@/lib/api/schema';
import { browserStorage } from '@/lib/store/preferences';

const STORAGE_KEY = 'sharpline.auth.v1';

/**
 * How early the access token is renewed, as a fraction of its lifetime, bounded
 * to a sane number of seconds. Renewing exactly at expiry guarantees at least
 * one 401 per session.
 */
const REFRESH_LEAD_MIN_SECONDS = 5;
const REFRESH_LEAD_MAX_SECONDS = 30;

export type AuthStatus =
  | 'anonymous'
  | 'authenticating'
  | 'authenticated'
  | 'refreshing';

export interface AuthState {
  /** IN MEMORY ONLY. Never persisted, never logged. */
  readonly accessToken: string | null;
  /** Epoch ms at which the access token expires. */
  readonly accessTokenExpiresAt: number | null;
  /** Persisted. Rotates on every use; reuse revokes the whole family. */
  readonly refreshToken: string | null;
  /** RFC 3339. The absolute end of the login lineage, not of this token. */
  readonly refreshExpiresAt: string | null;
  readonly account: SchemaAccount | null;
  readonly status: AuthStatus;
  /**
   * The last failure. Read this after an action returns false rather than
   * catching — the actions do not throw, so a `void login(...)` cannot produce
   * an unhandled rejection.
   */
  readonly error: ApiError | null;
  /** Whether the persisted refresh token has been read. */
  readonly hydrated: boolean;

  readonly register: (email: string, password: string) => Promise<boolean>;
  readonly login: (
    email: string,
    password: string,
    totpCode?: string,
  ) => Promise<boolean>;
  readonly refresh: () => Promise<boolean>;
  readonly logout: () => Promise<void>;
  readonly loadAccount: () => Promise<boolean>;
  readonly clearError: () => void;
}

type PersistedAuth = Pick<AuthState, 'refreshToken' | 'refreshExpiresAt'>;

/**
 * Held outside the store on purpose: a timer handle is not state, it is not
 * serialisable, and putting it in the store would persist a number that means
 * nothing on the next page load.
 */
let refreshTimer: ReturnType<typeof setTimeout> | null = null;

function clearRefreshTimer(): void {
  if (refreshTimer === null) return;
  clearTimeout(refreshTimer);
  refreshTimer = null;
}

function scheduleRefresh(expiresInSeconds: number): void {
  clearRefreshTimer();
  if (typeof window === 'undefined') return;
  const lead = Math.min(
    REFRESH_LEAD_MAX_SECONDS,
    Math.max(REFRESH_LEAD_MIN_SECONDS, Math.floor(expiresInSeconds / 4)),
  );
  const delayMs = Math.max(1_000, (expiresInSeconds - lead) * 1_000);
  refreshTimer = setTimeout(() => {
    void useAuth.getState().refresh();
  }, delayMs);
}

function sessionPatch(session: SchemaSessionResponse): Partial<AuthState> {
  return {
    accessToken: session.access_token,
    accessTokenExpiresAt: Date.now() + session.expires_in * 1_000,
    refreshToken: session.refresh_token,
    refreshExpiresAt: session.refresh_expires_at,
    account: session.account,
    status: 'authenticated',
    error: null,
  };
}

const ANONYMOUS: Partial<AuthState> = {
  accessToken: null,
  accessTokenExpiresAt: null,
  refreshToken: null,
  refreshExpiresAt: null,
  account: null,
  status: 'anonymous',
};

function asApiError(value: unknown): ApiError | null {
  return isApiError(value) ? value : null;
}

export const useAuth = create<AuthState>()(
  persist<AuthState, [], [], PersistedAuth>(
    (set, get) => ({
      accessToken: null,
      accessTokenExpiresAt: null,
      refreshToken: null,
      refreshExpiresAt: null,
      account: null,
      status: 'anonymous',
      error: null,
      hydrated: false,

      register: async (email, password) => {
        set({ status: 'authenticating', error: null });
        try {
          const session = await browserApi.register(email, password);
          set(sessionPatch(session));
          scheduleRefresh(session.expires_in);
          return true;
        } catch (cause) {
          clearRefreshTimer();
          set({ ...ANONYMOUS, error: asApiError(cause) });
          return false;
        }
      },

      login: async (email, password, totpCode) => {
        set({ status: 'authenticating', error: null });
        try {
          const session = await browserApi.login(email, password, totpCode);
          set(sessionPatch(session));
          scheduleRefresh(session.expires_in);
          return true;
        } catch (cause) {
          clearRefreshTimer();
          // A `totp_required` 401 is reachable only AFTER the password has been
          // verified, so it is not a credential failure and the form should ask
          // for a code rather than say the password was wrong. The code is on
          // the error; this store keeps the whole error so the form can branch.
          set({ ...ANONYMOUS, error: asApiError(cause) });
          return false;
        }
      },

      refresh: async () => {
        const token = get().refreshToken;
        if (token === null || token === '') {
          clearRefreshTimer();
          set({ ...ANONYMOUS });
          return false;
        }
        set({ status: 'refreshing' });
        try {
          const session = await browserApi.refresh(token);
          set(sessionPatch(session));
          scheduleRefresh(session.expires_in);
          return true;
        } catch (cause) {
          // Unknown, expired, revoked and already-redeemed are reported
          // identically and all four are terminal for this session. Holding on
          // to a dead token would retry it forever and, on the reuse path,
          // repeatedly revoke a family that is already gone.
          clearRefreshTimer();
          set({ ...ANONYMOUS, error: asApiError(cause) });
          return false;
        }
      },

      logout: async () => {
        const token = get().refreshToken;
        clearRefreshTimer();
        set({ ...ANONYMOUS, error: null });
        if (token === null || token === '') return;
        try {
          await browserApi.logout(token);
        } catch {
          // Best effort. The local session is already gone, and the family
          // expires on its own; surfacing a failure here would tell the user
          // their sign-out did not work when locally it did.
        }
      },

      loadAccount: async () => {
        const token = get().accessToken;
        if (token === null) return false;
        try {
          const account = await browserApi.getAccount({ accessToken: token });
          set({ account, error: null });
          return true;
        } catch (cause) {
          if (isApiError(cause) && cause.isUnauthenticated) {
            // One retry, and only after a successful refresh. Retrying without
            // one would loop against a token that is not going to start working.
            const refreshed = await get().refresh();
            if (!refreshed) return false;
            const renewed = get().accessToken;
            if (renewed === null) return false;
            try {
              const account = await browserApi.getAccount({
                accessToken: renewed,
              });
              set({ account, error: null });
              return true;
            } catch (retryCause) {
              set({ error: asApiError(retryCause) });
              return false;
            }
          }
          set({ error: asApiError(cause) });
          return false;
        }
      },

      clearError: () => {
        set({ error: null });
      },
    }),
    {
      name: STORAGE_KEY,
      version: 1,
      skipHydration: true,
      storage: createJSONStorage<PersistedAuth>(browserStorage),
      // ONLY the refresh token is persisted. The access token, the account, the
      // status and the last error are all in-memory session state.
      partialize: (state) => ({
        refreshToken: state.refreshToken,
        refreshExpiresAt: state.refreshExpiresAt,
      }),
      onRehydrateStorage: () => () => {
        useAuth.setState({ hydrated: true });
      },
    },
  ),
);

/**
 * Reads the persisted refresh token after mount and, if there is one, exchanges
 * it for an access token. Call it ONCE, high in the client tree.
 *
 * Returns whether hydration has run — not whether the user is signed in.
 *
 * Hydration is deferred to an effect for the same reason preferences are: the
 * server renders signed-out because it has no storage, and reading storage
 * during the first client render would change the tree mid-hydration.
 */
export function useAuthHydration(): boolean {
  const hydrated = useAuth((state) => state.hydrated);
  const refreshToken = useAuth((state) => state.refreshToken);
  const status = useAuth((state) => state.status);

  useEffect(() => {
    if (!hydrated) {
      void useAuth.persist.rehydrate();
      return;
    }
    // A persisted refresh token and no session yet: exchange it. Guarded on
    // `anonymous` so a login already in flight is not raced by this effect.
    if (refreshToken === null || refreshToken === '') return;
    if (status !== 'anonymous') return;
    void useAuth.getState().refresh();
  }, [hydrated, refreshToken, status]);

  return hydrated;
}

// -----------------------------------------------------------------------------
// Selectors
// -----------------------------------------------------------------------------

/** The in-memory access token, or null. Never render it, never log it. */
export function useAccessToken(): string | null {
  return useAuth((state) => state.accessToken);
}

export function useIsAuthenticated(): boolean {
  return useAuth((state) => state.accessToken !== null);
}

export function useAccount(): SchemaAccount | null {
  return useAuth((state) => state.account);
}

export function useAuthStatus(): AuthStatus {
  return useAuth((state) => state.status);
}

export function useAuthError(): ApiError | null {
  return useAuth((state) => state.error);
}

/** Plain selectors, for `useAuth(selectIsAuthenticated)` and for tests. */
export const selectIsAuthenticated = (state: AuthState): boolean =>
  state.accessToken !== null;

export const selectAccessToken = (state: AuthState): string | null =>
  state.accessToken;
