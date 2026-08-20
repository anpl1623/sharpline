'use client';

/**
 * User display preferences: the odds format toggle and the book filter.
 *
 * # Hydration is explicit, not automatic
 *
 * The server has no localStorage, so the server renders the DEFAULTS. If the
 * store read localStorage during the first client render, a user whose stored
 * format is decimal would see the server's "+150" replaced mid-hydration by
 * "2.50" and React would report a mismatch on every price cell on the board.
 *
 * So persistence runs with `skipHydration`, the first client render matches the
 * server exactly, and `usePreferencesHydration()` rehydrates in an effect —
 * after mount, where a state change is an ordinary update rather than a
 * mismatch. Anything that must not flicker can gate on the returned flag.
 *
 * # American is the default
 *
 * DESIGN.md, "Category conventions deliberately kept": American odds default,
 * with a format toggle in the header. Innovating on the default format buys
 * nothing and costs literacy.
 */

import { useEffect } from 'react';
import { create } from 'zustand';
import { createJSONStorage, persist } from 'zustand/middleware';
import type { StateStorage } from 'zustand/middleware';

import { nextOddsFormat } from '@/lib/odds/format';
import type { OddsFormat } from '@/lib/odds/format';

/**
 * Storage that exists but does nothing.
 *
 * Returned instead of `window.localStorage` when there is no window (server
 * rendering) or when storage is unavailable (Safari private browsing throws on
 * access, not on use). Persistence then degrades to "preferences reset on
 * reload", which is a mild loss; the alternative — an uncaught ReferenceError or
 * SecurityError during module evaluation — takes the whole page down.
 */
const NOOP_STORAGE: StateStorage = {
  getItem: () => null,
  setItem: () => undefined,
  removeItem: () => undefined,
};

export function browserStorage(): StateStorage {
  if (typeof window === 'undefined') return NOOP_STORAGE;
  try {
    return window.localStorage;
  } catch {
    return NOOP_STORAGE;
  }
}

/**
 * Versioned so a future shape change can be migrated or discarded rather than
 * decoded wrongly. Bump the suffix AND `version` together.
 */
const STORAGE_KEY = 'sharpline.preferences.v1';

export interface PreferencesState {
  readonly oddsFormat: OddsFormat;
  /**
   * Book slugs to restrict prices to. EMPTY MEANS EVERY BOOK — it is not
   * "no books". Kept sorted so the value is stable across two selections made
   * in different orders, which keeps query keys and React identities stable.
   */
  readonly bookFilter: readonly string[];
  /** Whether stored preferences have been read. False during the first render. */
  readonly hydrated: boolean;

  readonly setOddsFormat: (format: OddsFormat) => void;
  readonly cycleOddsFormat: () => void;
  readonly setBookFilter: (slugs: readonly string[]) => void;
  readonly toggleBook: (slug: string) => void;
  readonly clearBookFilter: () => void;
}

type PersistedPreferences = Pick<PreferencesState, 'oddsFormat' | 'bookFilter'>;

export const usePreferences = create<PreferencesState>()(
  persist<PreferencesState, [], [], PersistedPreferences>(
    (set, get) => ({
      oddsFormat: 'american',
      bookFilter: [],
      hydrated: false,

      setOddsFormat: (format) => {
        set({ oddsFormat: format });
      },

      cycleOddsFormat: () => {
        set({ oddsFormat: nextOddsFormat(get().oddsFormat) });
      },

      setBookFilter: (slugs) => {
        set({ bookFilter: [...new Set(slugs)].sort() });
      },

      toggleBook: (slug) => {
        const current = get().bookFilter;
        const next = current.includes(slug)
          ? current.filter((entry) => entry !== slug)
          : [...current, slug].sort();
        set({ bookFilter: next });
      },

      clearBookFilter: () => {
        set({ bookFilter: [] });
      },
    }),
    {
      name: STORAGE_KEY,
      version: 1,
      skipHydration: true,
      storage: createJSONStorage<PersistedPreferences>(browserStorage),
      partialize: (state) => ({
        oddsFormat: state.oddsFormat,
        bookFilter: state.bookFilter,
      }),
      onRehydrateStorage: () => () => {
        usePreferences.setState({ hydrated: true });
      },
    },
  ),
);

/**
 * Rehydrates stored preferences after mount and reports whether that has
 * happened. Call it ONCE, high in the client tree.
 *
 * Returning the flag rather than nothing lets a surface that would flicker —
 * a persisted book filter changing which columns are rendered, say — hold its
 * default until the real value is known.
 */
export function usePreferencesHydration(): boolean {
  const hydrated = usePreferences((state) => state.hydrated);

  useEffect(() => {
    if (hydrated) return;
    void usePreferences.persist.rehydrate();
  }, [hydrated]);

  return hydrated;
}

/** The current odds format. The value every price cell renders through. */
export function useOddsFormat(): OddsFormat {
  return usePreferences((state) => state.oddsFormat);
}

export function useSetOddsFormat(): (format: OddsFormat) => void {
  return usePreferences((state) => state.setOddsFormat);
}

export function useCycleOddsFormat(): () => void {
  return usePreferences((state) => state.cycleOddsFormat);
}

/** The selected book slugs. Empty means every book. */
export function useBookFilter(): readonly string[] {
  return usePreferences((state) => state.bookFilter);
}

export function useToggleBook(): (slug: string) => void {
  return usePreferences((state) => state.toggleBook);
}

export function useSetBookFilter(): (slugs: readonly string[]) => void {
  return usePreferences((state) => state.setBookFilter);
}

export function useClearBookFilter(): () => void {
  return usePreferences((state) => state.clearBookFilter);
}

/**
 * Whether a book's prices are shown. An empty filter selects everything, so this
 * is true for every book until the user narrows it.
 */
export function useIsBookSelected(slug: string): boolean {
  return usePreferences(
    (state) => state.bookFilter.length === 0 || state.bookFilter.includes(slug),
  );
}

/**
 * The filter as an API parameter: `undefined` when empty, because omitting
 * `book` means "every book" and sending an empty value would be a different
 * request.
 */
export function bookFilterParam(
  bookFilter: readonly string[],
): readonly string[] | undefined {
  return bookFilter.length === 0 ? undefined : bookFilter;
}

/** The same, as a hook, for a component wiring a query. */
export function useBookFilterParam(): readonly string[] | undefined {
  const bookFilter = useBookFilter();
  return bookFilterParam(bookFilter);
}

export type { OddsFormat };
