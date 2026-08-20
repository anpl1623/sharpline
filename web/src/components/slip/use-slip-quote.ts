'use client';

/**
 * The priced slip: one request, debounced, keyed by its own body.
 *
 * # Debounced, because the stake field is a keystroke stream
 *
 * Typing "125.00" is six edits, and quoting each of them would put five requests
 * on the wire whose answers are all superseded before they render. The debounce
 * is on the REQUEST BODY rather than on the stake alone, so it also absorbs a
 * customer clicking three prices in quick succession, and it is short enough
 * that the pause between typing and looking is where the request lands.
 *
 * # `placeable` is advisory and the slip renders it as advisory
 *
 * The quote's impediments are read OUTSIDE a transaction and can be stale by the
 * time Place is pressed. They exist so the button can be disabled with a reason
 * rather than failing on submit — `POST /wagers` re-evaluates every one of them
 * inside the placement transaction, under a lock on the customer's own row, and
 * that is the only evaluation that decides anything.
 *
 * One check is deliberately NOT attempted on this path at all: responsible-gaming
 * limits. Evaluating one is a period-scoped sum over the ledger taken under the
 * placement lock, and computing it a second time on a read path would put two
 * answers to a responsible-gaming control in the tree. So a slip that would
 * breach a self-imposed limit quotes as `placeable: true` here and is refused
 * `422 limit_exceeded` on submit. That is the safe direction to be wrong in — the
 * control still binds and only the button's disabled state is optimistic — and
 * the slip's refusal copy is written on the assumption that a limit refusal
 * arrives at the button rather than before it.
 */

import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { browserApi } from '@/lib/api/client';
import { queryKeys } from '@/lib/api/queries';
import type { SchemaSlipQuote, SchemaSlipQuoteRequest } from '@/lib/api/schema';
import { useAccessToken } from '@/lib/store/auth';
import { slipQuoteRequest } from './slip-model';
import type { SlipRequestState } from './slip-model';

/**
 * How long the slip waits before pricing an edit.
 *
 * Long enough to swallow a burst of keystrokes, short enough that the answer is
 * there when the eye arrives — the same 200ms neighbourhood the search box uses,
 * chosen for the same reason and not tuned separately, because two controls in
 * one product that feel differently responsive read as one of them being broken.
 */
export const QUOTE_DEBOUNCE_MS = 200;

export interface SlipQuoteResult {
  /** The body the quote on screen was computed from, or null if unpriceable. */
  readonly request: SchemaSlipQuoteRequest | null;
  readonly quote: SchemaSlipQuote | undefined;
  /** The thrown value, for the panel's own error rendering. */
  readonly error: unknown;
  readonly isFetching: boolean;
  /**
   * True when the slip on screen has changed and its price is not known yet.
   *
   * DIFFERENT from `isFetching`: a background refetch of an unchanged slip is
   * fetching but not settling, and the numbers on screen are still correct for
   * what is on screen. This is what gates the button, because placing against a
   * quote for a different slip is placing against a number the customer was
   * never shown.
   */
  readonly settling: boolean;
}

/**
 * Prices the slip.
 *
 * Returns `quote: undefined` with `request: null` for a slip that cannot be
 * priced yet — no legs, no stake, a round robin with no sizes, a teaser with no
 * points. That is not an error state and the panel does not render one; it is
 * simply a slip the customer has not finished describing.
 */
export function useSlipQuote(state: SlipRequestState): SlipQuoteResult {
  const accessToken = useAccessToken();
  const request = slipQuoteRequest(state);

  // The body is compared by VALUE, through its serialisation, not by identity:
  // the store rebuilds `legs` on every mutation, so an identity comparison would
  // restart the debounce on edits that changed nothing this request can see.
  const signature = request === null ? '' : JSON.stringify(request);
  const [settled, setSettled] = useState(signature);

  useEffect(() => {
    if (settled === signature) return;
    const timer = setTimeout(() => {
      setSettled(signature);
    }, QUOTE_DEBOUNCE_MS);
    return () => {
      clearTimeout(timer);
    };
  }, [signature, settled]);

  // Parsed back rather than held as a second piece of state, so there is exactly
  // one source for the body and the two cannot disagree about which slip is
  // being priced. Memoised because this component re-renders on every price tick
  // of every watched leg, and re-parsing a settled body on each of those is work
  // whose answer cannot have changed.
  const debounced = useMemo<SchemaSlipQuoteRequest | null>(
    () =>
      settled === '' ? null : (JSON.parse(settled) as SchemaSlipQuoteRequest),
    [settled],
  );

  const enabled =
    debounced !== null && accessToken !== null && accessToken !== '';

  const query = useQuery({
    queryKey: queryKeys.slipQuote(
      debounced ?? { kind: 'straight', legs: [], stake_minor: 0 },
    ),
    queryFn: ({ signal }) => {
      // Unreachable while `enabled` is false; the throw is here so the closure
      // has no `!` in it and the impossible case is loud rather than coerced.
      if (debounced === null) throw new Error('no slip to quote');
      return browserApi.quoteSlip(debounced, {
        accessToken: accessToken ?? undefined,
        signal,
      });
    },
    staleTime: 0,
    enabled,
  });

  return {
    request: debounced,
    quote: query.data,
    error: query.error,
    isFetching: query.isFetching,
    settling:
      request !== null && (signature !== settled || (enabled && query.isPending)),
  };
}
