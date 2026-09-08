'use client';

/**
 * The bet slip's state.
 *
 * # What lives here and what deliberately does not
 *
 * This store holds the CUSTOMER'S INTENT and nothing else: which selections are
 * on the slip, at what price they were seen, what kind of ticket they are, how
 * much is being staked, and which moved prices have been explicitly accepted.
 *
 * It holds no money the book owes, no payout, no ticket price, no balance and no
 * placeability. Every one of those is the server's answer to a slip, arrives on
 * `POST /slip/quote`, and is cached by TanStack Query beside the request that
 * produced it. Keeping them out of here is what makes the two impossible to
 * disagree: there is exactly one payout figure on screen and it came with the
 * prices it was computed from.
 *
 * # `seen` is frozen, and that is the whole accept/reject mechanism
 *
 * `seenDecimal` and `seenLine` are the numbers that were ON SCREEN when the
 * customer clicked, and nothing updates them afterwards. They are the left-hand
 * side of a comparison and never the price that gets booked. When the market
 * moves, the live price and the frozen one differ, the slip says so, and the
 * button is blocked until the customer either accepts the new number — which
 * writes `acceptedDecimal`/`acceptedLine`, a SEPARATE pair — or removes the leg.
 *
 * The acceptance is a separate field rather than an overwrite of `seen`, and the
 * OpenAPI document is explicit about why: overwriting would make "the customer
 * accepted a move" indistinguishable from "the customer never saw one", so a
 * client that simply echoed back whatever the server last said would silently
 * opt every user into every future move.
 *
 * `acceptedLine` travels with `acceptedDecimal` for the same reason a line move
 * always needs its own acceptance: "yes, book me at 1.95" is not consent to a
 * different handicap, and a book that read it as consent would be moving the
 * customer's bet.
 *
 * # Money is minor units, here and on the wire
 *
 * `stakeMinor` is an integer. Nothing in this store, in the components that read
 * it, or in the request built from it ever divides by 100 — see `@/lib/money`
 * for the parse that keeps a typed `"19.99"` off the floating-point path.
 *
 * # Persistence, and the one field whose persistence is a safety property
 *
 * The slip survives a reload, because losing a six-leg parlay to a stray refresh
 * is the single most annoying thing a betting UI can do. The persisted set
 * includes `attemptKey`, and that is not a convenience: a tab that crashed
 * between sending a submit and seeing its answer comes back holding the SAME
 * idempotency key, so pressing Place again returns the ticket the first attempt
 * booked instead of booking a second one. See `@/lib/betting/idempotency`.
 *
 * `skipHydration` is on, matching the two stores that already exist: the server
 * renders the empty slip, the first client render matches it exactly, and
 * `useSlipHydration()` reads storage in an effect afterwards. Without it every
 * leg on a restored slip would be a hydration mismatch.
 */

import { useEffect } from 'react';
import { create } from 'zustand';
import { createJSONStorage, persist } from 'zustand/middleware';

import type {
  SchemaMarketType,
  SchemaPlacement,
  SchemaPriceMove,
  SchemaSelectionRole,
  SchemaWagerKind,
} from '@/lib/api/schema';
import {
  NO_IDEMPOTENCY_KEY,
  newIdempotencyKey,
} from '@/lib/betting/idempotency';
import {
  MAX_WAGER_LEGS,
  canonicalSizes,
  fallbackKind,
  isValidTeaserPoints,
  kindAvailability,
} from '@/lib/betting/ticket';
import { MAX_STAKE_MINOR, isMoneyMinor } from '@/lib/money';
import { browserStorage } from '@/lib/store/preferences';

/** Bump the suffix AND `version` together when this shape changes. */
const STORAGE_KEY = 'sharpline.slip.v1';

// -----------------------------------------------------------------------------
// A leg
// -----------------------------------------------------------------------------

/**
 * One selection on the slip, with the quote the customer had on screen.
 *
 * Everything needed to RENDER the leg is copied in at add time rather than
 * looked up later: the event name, the selection name, the book. A slip whose
 * rows resolved their own labels would empty itself when the board paged away
 * from the event, and a leg that cannot say what it is on is a leg nobody should
 * be asked to confirm.
 *
 * The one thing NOT copied is the price to book at. That is re-read by the
 * server inside the placement transaction, every time.
 */
export interface SlipLeg {
  readonly selectionId: string;
  readonly selectionName: string;
  readonly role: SchemaSelectionRole;
  readonly marketId: string;
  readonly marketType: SchemaMarketType;
  readonly eventId: string;
  readonly eventName: string;
  /**
   * Which book's line the customer took. Required by the API rather than
   * defaulted: "best price" is a rendering decision this client already made
   * when it put a number on screen, and re-deriving it server-side could book a
   * different book's line than the one that was clicked.
   */
  readonly bookId: string;
  readonly bookSlug: string;
  /** The decimal price that was on screen. FROZEN — see the file comment. */
  readonly seenDecimal: number;
  /**
   * The line that was on screen, from THIS SELECTION'S OWN perspective —
   * already inverted for an away spread, exactly as the board rendered it.
   * `null` on a moneyline or futures market; a present `0` is a traded pick'em,
   * which is a different fact from "no line".
   */
  readonly seenLine: number | null;
  /** The re-quoted price explicitly agreed to, or null. Never overwrites `seen`. */
  readonly acceptedDecimal: number | null;
  /** The re-quoted line agreed to, alongside `acceptedDecimal`. */
  readonly acceptedLine: number | null;
  /** Epoch ms. Orders the slip so the newest leg is not buried. */
  readonly addedAt: number;
}

/** What a price cell hands over. The store supplies `addedAt` and the acceptances. */
export type SlipLegInput = Omit<
  SlipLeg,
  'acceptedDecimal' | 'acceptedLine' | 'addedAt'
>;

/**
 * The number a leg will actually be submitted at: the acceptance if there is
 * one, otherwise the frozen seen price.
 *
 * The comparison a live price is checked against, and the value the placement
 * request echoes.
 */
export function legEffectiveDecimal(leg: SlipLeg): number {
  return leg.acceptedDecimal ?? leg.seenDecimal;
}

/** The same for the line. */
export function legEffectiveLine(leg: SlipLeg): number | null {
  return leg.acceptedLine ?? leg.seenLine;
}

// -----------------------------------------------------------------------------
// The receipt
// -----------------------------------------------------------------------------
//
// What a completed placement leaves behind is the `Placement` itself, held in
// the store rather than in the component that placed it: the placement CLEARS
// the slip, and a panel that unmounted its own success state along with the legs
// would flash the ticket and lose it. It is not persisted — a receipt is a
// moment, and the tickets themselves live at `/bets`.

// -----------------------------------------------------------------------------
// The store
// -----------------------------------------------------------------------------

export interface SlipState {
  readonly legs: readonly SlipLeg[];
  readonly kind: SchemaWagerKind;
  /** The stake on ONE ticket, in minor units. For a round robin, per combination. */
  readonly stakeMinor: number;
  /** Round-robin combination sizes: `[2]` is "by 2s". Canonical order. */
  readonly roundRobinSizes: readonly number[];
  /** Null until the customer names them. Required on a teaser and nothing else. */
  readonly teaserPoints: number | null;
  /**
   * Opt in to booking a LONGER price at an UNCHANGED line without a further
   * round trip. Off by default, and it never covers a shorter price or a line
   * move — the API's own note explains that "accept when the new price is
   * longer" and "accept when it is shorter" are one comparison operator apart,
   * so the concession is made explicit rather than assumed.
   */
  readonly acceptBetterPrice: boolean;
  /** The whole-ticket price explicitly agreed to after a ticket-level move. */
  readonly acceptedTicketDecimal: number | null;
  /**
   * The idempotency key for this slip's submits. Minted lazily on the first
   * client mutation and rotated only when the slip empties. See the file
   * comment for why it does NOT rotate on an edit.
   */
  readonly attemptKey: string;
  /** Whether the mobile bottom sheet is showing. Never persisted. */
  readonly open: boolean;
  /** A one-line refusal from the store itself, e.g. the slip is full. */
  readonly notice: string | null;
  readonly receipt: SchemaPlacement | null;
  /** Whether persisted state has been read. False during the first render. */
  readonly hydrated: boolean;

  readonly add: (leg: SlipLegInput) => void;
  readonly remove: (selectionId: string) => void;
  readonly toggle: (leg: SlipLegInput) => void;
  readonly clear: () => void;
  readonly setKind: (kind: SchemaWagerKind) => void;
  readonly setStakeMinor: (minor: number) => void;
  readonly toggleRoundRobinSize: (size: number) => void;
  readonly setTeaserPoints: (points: number | null) => void;
  readonly setAcceptBetterPrice: (accept: boolean) => void;
  readonly acceptLeg: (
    selectionId: string,
    decimal: number,
    line: number | null,
  ) => void;
  readonly acceptTicket: (decimal: number) => void;
  readonly applyPriceMoves: (moves: readonly SchemaPriceMove[]) => void;
  readonly setOpen: (open: boolean) => void;
  readonly dismissNotice: () => void;
  readonly recordPlacement: (placement: SchemaPlacement) => void;
  readonly dismissReceipt: () => void;
}

type PersistedSlip = Pick<
  SlipState,
  | 'legs'
  | 'kind'
  | 'stakeMinor'
  | 'roundRobinSizes'
  | 'teaserPoints'
  | 'acceptBetterPrice'
  | 'attemptKey'
>;

/**
 * The state a cleared slip returns to.
 *
 * `attemptKey` is reset to the sentinel rather than to a fresh key: minting one
 * here would run `crypto` during a module evaluation that also happens on the
 * server. It is minted on the next mutation instead, which is the first moment a
 * key can possibly be needed and is guaranteed to be in a browser.
 */
const EMPTY: Omit<PersistedSlip, 'acceptBetterPrice'> & {
  readonly acceptedTicketDecimal: null;
  readonly receipt: null;
  readonly notice: null;
} = {
  legs: [],
  kind: 'straight',
  stakeMinor: 0,
  roundRobinSizes: [],
  teaserPoints: null,
  attemptKey: NO_IDEMPOTENCY_KEY,
  acceptedTicketDecimal: null,
  receipt: null,
  notice: null,
};

/**
 * The key to submit under, minting one if this slip has never needed a key.
 *
 * Called from every mutation rather than from the component, so a slip built by
 * any route ends up with a key without that route having to remember to ask for
 * one.
 */
function ensureKey(current: string): string {
  return current === NO_IDEMPOTENCY_KEY ? newIdempotencyKey() : current;
}

/*
 * A NOTE ON WHAT A SHAPE CHANGE DOES AND DOES NOT INVALIDATE, because the first
 * version of this file got it wrong in a way that was invisible in testing and
 * miserable on a moving market.
 *
 * A LEG acceptance survives every change to the rest of the slip. It says "book
 * me on this selection, at this book, at this price and this line" — and adding
 * a fourth leg, switching from parlay to round robin, or setting teaser points
 * changes none of those four things. The book's quote on that leg is what it is.
 *
 * Clearing them on every edit, which is what this store did first, produces a
 * loop nobody can escape on a market that is actually moving: accept leg two,
 * add leg three, leg two reverts to its original price and reports itself moved
 * again, accept it, add leg four, and so on. The safety it buys is imaginary,
 * because the server re-checks every acceptance against the CURRENT quote anyway
 * and refuses with the newer numbers if it is stale.
 *
 * The TICKET acceptance is the opposite and is dropped on every shape change. It
 * says "book me at 6.42 for the whole ticket", and a ticket with another leg on
 * it is not that ticket — its price is not even the product of its legs when the
 * legs are correlated. Consent to a whole-ticket number cannot survive the ticket
 * changing underneath it.
 *
 * The one thing that DOES invalidate a leg acceptance is the server saying so,
 * on a `409 price_moved`. `applyPriceMoves` handles exactly that.
 */

/**
 * Reconciles the kind and its parameters after the leg set changes.
 *
 * Three things can stop being true when a leg comes or goes: the kind's arity
 * (a straight with two legs), the round robin's sizes (by 4s on three
 * selections), and a teaser's teasability (a moneyline joined the slip). Each is
 * corrected to the nearest legal state rather than left to be refused on submit,
 * because a slip that shows an impossible ticket and only says so at the button
 * has wasted the customer's decision.
 *
 * The correction never UPGRADES: it falls back to straight or parlay, never into
 * a round robin, because silently converting a parlay into a round robin would
 * multiply what somebody is risking without them asking.
 */
function reconcile(
  legs: readonly SlipLeg[],
  kind: SchemaWagerKind,
  sizes: readonly number[],
  teaserPoints: number | null,
): Pick<SlipState, 'kind' | 'roundRobinSizes' | 'teaserPoints'> {
  const availability = kindAvailability(
    legs.map((leg) => ({
      eventId: leg.eventId,
      marketType: leg.marketType,
      line: legEffectiveLine(leg),
    })),
  );
  const stillLegal =
    availability.find((entry) => entry.kind === kind)?.available ?? false;
  const nextKind = stillLegal ? kind : fallbackKind(legs.length);

  const nextSizes =
    nextKind === 'round_robin'
      ? canonicalSizes(sizes.filter((size) => size >= 2 && size <= legs.length))
      : [];

  const nextPoints = nextKind === 'teaser' ? teaserPoints : null;

  return {
    kind: nextKind,
    roundRobinSizes: nextSizes,
    teaserPoints: nextPoints,
  };
}

export const useSlip = create<SlipState>()(
  persist<SlipState, [], [], PersistedSlip>(
    (set, get) => ({
      ...EMPTY,
      acceptBetterPrice: false,
      open: false,
      hydrated: false,

      add: (input) => {
        const state = get();
        if (state.legs.some((leg) => leg.selectionId === input.selectionId)) {
          return;
        }
        if (state.legs.length >= MAX_WAGER_LEGS) {
          set({
            notice: `A ticket takes at most ${String(MAX_WAGER_LEGS)} selections.`,
          });
          return;
        }

        const legs: readonly SlipLeg[] = [
          ...state.legs,
          {
            ...input,
            acceptedDecimal: null,
            acceptedLine: null,
            addedAt: Date.now(),
          },
        ];

        set({
          legs,
          ...reconcile(
            legs,
            state.kind,
            state.roundRobinSizes,
            state.teaserPoints,
          ),
          acceptedTicketDecimal: null,
          attemptKey: ensureKey(state.attemptKey),
          notice: null,
          receipt: null,
        });
      },

      remove: (selectionId) => {
        const state = get();
        const legs = state.legs.filter((leg) => leg.selectionId !== selectionId);
        if (legs.length === state.legs.length) return;

        // The last leg leaving IS an empty slip, and an empty slip is a spent
        // attempt: rotating the key here is what stops the next ticket the
        // customer builds from colliding with the one they just abandoned.
        if (legs.length === 0) {
          set({ ...EMPTY, open: state.open });
          return;
        }

        set({
          legs,
          ...reconcile(
            legs,
            state.kind,
            state.roundRobinSizes,
            state.teaserPoints,
          ),
          acceptedTicketDecimal: null,
          notice: null,
        });
      },

      toggle: (input) => {
        const present = get().legs.some(
          (leg) => leg.selectionId === input.selectionId,
        );
        if (present) {
          get().remove(input.selectionId);
          return;
        }
        get().add(input);
      },

      clear: () => {
        set({ ...EMPTY, open: false });
      },

      setKind: (kind) => {
        const state = get();
        set({
          ...reconcile(
            state.legs,
            kind,
            state.roundRobinSizes,
            state.teaserPoints,
          ),
          acceptedTicketDecimal: null,
          attemptKey: ensureKey(state.attemptKey),
          notice: null,
        });
      },

      setStakeMinor: (minor) => {
        // Guarded rather than trusted: this value ends up in a request body, and
        // a non-integer here would be a float that reached a money field.
        if (!isMoneyMinor(minor) || minor < 0 || minor > MAX_STAKE_MINOR) {
          return;
        }
        set({
          stakeMinor: minor,
          attemptKey: ensureKey(get().attemptKey),
          notice: null,
        });
      },

      toggleRoundRobinSize: (size) => {
        const state = get();
        if (size < 2 || size > state.legs.length) return;
        const present = state.roundRobinSizes.includes(size);
        const next = present
          ? state.roundRobinSizes.filter((entry) => entry !== size)
          : [...state.roundRobinSizes, size];
        set({
          roundRobinSizes: canonicalSizes(next),
          attemptKey: ensureKey(state.attemptKey),
          notice: null,
        });
      },

      setTeaserPoints: (points) => {
        if (points !== null && !isValidTeaserPoints(points)) return;
        set({
          teaserPoints: points,
          // The LEG acceptances stand: teaser points move the lines the ticket
          // GRADES at, which the server derives, and they do not touch the
          // book's quote on any leg. The ticket acceptance does not — a ticket
          // teased by six points is not the one priced at zero.
          acceptedTicketDecimal: null,
          attemptKey: ensureKey(get().attemptKey),
          notice: null,
        });
      },

      setAcceptBetterPrice: (accept) => {
        set({ acceptBetterPrice: accept, notice: null });
      },

      acceptLeg: (selectionId, decimal, line) => {
        const state = get();
        set({
          legs: state.legs.map((leg) =>
            leg.selectionId === selectionId
              ? { ...leg, acceptedDecimal: decimal, acceptedLine: line }
              : leg,
          ),
          // A leg's price is an input to the ticket's, so a ticket acceptance
          // taken before this one is consent to a number that has changed.
          acceptedTicketDecimal: null,
          notice: null,
        });
      },

      acceptTicket: (decimal) => {
        set({ acceptedTicketDecimal: decimal, notice: null });
      },

      applyPriceMoves: (moves) => {
        const state = get();
        let legs = state.legs;
        let ticket = state.acceptedTicketDecimal;

        for (const move of moves) {
          if (move.scope === 'ticket') {
            // NOT accepted here. A reported move is the server telling the
            // client what changed; accepting it is the CUSTOMER'S act, and a
            // client that folded the two together would take every move on the
            // customer's behalf. This only drops a stale acceptance so the UI
            // asks again.
            ticket = null;
            continue;
          }
          if (move.scope !== 'leg') continue;
          const selectionId = move.selection_id;
          if (selectionId === undefined || selectionId === null) continue;
          legs = legs.map((leg) =>
            leg.selectionId === selectionId
              ? { ...leg, acceptedDecimal: null, acceptedLine: null }
              : leg,
          );
        }

        set({ legs, acceptedTicketDecimal: ticket });
      },

      setOpen: (open) => {
        set({ open });
      },

      dismissNotice: () => {
        set({ notice: null });
      },

      recordPlacement: (placement) => {
        // The slip empties and the key rotates, in one commit with the receipt.
        // Splitting them would leave a window in which the tickets are booked,
        // the legs are still on screen, and the spent key is still armed.
        //
        // `open` is deliberately NOT reset. On a phone the slip IS the sheet, so
        // closing it here would empty the legs, set a receipt, and then take the
        // only surface showing that receipt off screen — the customer would see
        // their slip vanish and never learn what was booked.
        set({ ...EMPTY, receipt: placement });
      },

      dismissReceipt: () => {
        set({ receipt: null });
      },
    }),
    {
      name: STORAGE_KEY,
      version: 1,
      skipHydration: true,
      storage: createJSONStorage<PersistedSlip>(browserStorage),
      // `open`, `notice` and `receipt` are session state. A slip that restored
      // itself already open would take over the viewport on every page load,
      // and a restored receipt would announce a placement that happened in a
      // previous session as if it had just landed.
      partialize: (state) => ({
        legs: state.legs,
        kind: state.kind,
        stakeMinor: state.stakeMinor,
        roundRobinSizes: state.roundRobinSizes,
        teaserPoints: state.teaserPoints,
        acceptBetterPrice: state.acceptBetterPrice,
        attemptKey: state.attemptKey,
      }),
      onRehydrateStorage: () => () => {
        useSlip.setState({ hydrated: true });
      },
    },
  ),
);

/**
 * Reads the persisted slip after mount and reports whether that has happened.
 * Call it ONCE, high in the client tree, beside the other two hydrations.
 *
 * The flag matters here more than it does for preferences: an empty slip and an
 * unread slip look identical, and rendering the designed empty state during the
 * one frame before storage is read would flash "Your slip is empty" at somebody
 * who has six legs on it.
 */
export function useSlipHydration(): boolean {
  const hydrated = useSlip((state) => state.hydrated);

  useEffect(() => {
    if (hydrated) return;
    void useSlip.persist.rehydrate();
  }, [hydrated]);

  return hydrated;
}

// -----------------------------------------------------------------------------
// Selectors
// -----------------------------------------------------------------------------

/**
 * Every selector below returns a PRIMITIVE or a stable reference held in the
 * store, never a freshly-built object or array. Zustand compares snapshots by
 * identity, so a selector that mapped or filtered would return a new value on
 * every store change and re-render its component on every keystroke anywhere in
 * the slip.
 */

/** Whether one selection is on the slip. THE price-cell selector. */
export function useIsOnSlip(selectionId: string): boolean {
  return useSlip((state) =>
    state.legs.some((leg) => leg.selectionId === selectionId),
  );
}

/**
 * The whole action set, as one stable object.
 *
 * Zustand's actions are defined once at store creation and never replaced, so
 * selecting them individually is free — but a component that wants six of them
 * would otherwise write six hook calls. This is a plain function rather than a
 * hook returning a new object, so there is no snapshot to compare.
 */
export function slipActions(): Pick<
  SlipState,
  | 'add'
  | 'remove'
  | 'toggle'
  | 'clear'
  | 'setKind'
  | 'setStakeMinor'
  | 'toggleRoundRobinSize'
  | 'setTeaserPoints'
  | 'setAcceptBetterPrice'
  | 'acceptLeg'
  | 'acceptTicket'
  | 'applyPriceMoves'
  | 'setOpen'
  | 'dismissNotice'
  | 'recordPlacement'
  | 'dismissReceipt'
> {
  return useSlip.getState();
}
