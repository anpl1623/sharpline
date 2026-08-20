'use client';

/**
 * The one irreversible click.
 *
 * # There is no optimistic update here, and there will not be one
 *
 * TanStack Query makes optimistic mutation cheap and it is the wrong tool for
 * this button. An optimistic wager is a ticket on screen that the book has not
 * agreed to: it would show a stake that has not left the balance and a payout
 * nobody has promised, and the rollback path — a `409 price_moved`, a `422
 * limit_exceeded` — is the COMMON case rather than the exceptional one, because
 * the whole point of the price-change interstitial is that prices move while
 * people decide. The button therefore shows pending, and the receipt is built
 * from what the server actually booked.
 *
 * # The retry is the key, not the query client
 *
 * `retry` is off. A placement is not idempotent from the query client's point of
 * view — it is idempotent from the SERVER's, through the `Idempotency-Key`, and
 * that is a much stronger property than an automatic re-send. Leaving the retry
 * on would send a second request without a human deciding to, which is exactly
 * the situation the key exists to make safe rather than one to create on
 * purpose. A customer who wants to try again presses the button again, with the
 * same key, and gets the same ticket.
 *
 * # What a 409 does, and what it deliberately does not do
 *
 * A `409 price_moved` carries `price_moves` — the number the customer saw AND
 * the number the service holds now, for every leg that moved. The store drops
 * the stale acceptances so the interstitial asks again, and the panel renders
 * the new numbers with an accept control. It does NOT accept them: taking a move
 * on the customer's behalf because the customer had already pressed a button
 * once is precisely the behaviour the two-field acceptance design exists to
 * prevent.
 */

import { useMutation, useQueryClient } from '@tanstack/react-query';

import { browserApi } from '@/lib/api/client';
import { isApiError } from '@/lib/api/errors';
import type { ApiError } from '@/lib/api/errors';
import { queryKeys } from '@/lib/api/queries';
import type { SchemaPlacement, SchemaSlipQuote } from '@/lib/api/schema';
import { isUsableIdempotencyKey } from '@/lib/betting/idempotency';
import { useAccessToken } from '@/lib/store/auth';
import { useSlip } from '@/lib/store/slip';
import { placeWagerRequest } from './slip-model';

export interface PlaceWagerResult {
  /** Submits the slip as it stands. A no-op while pending or unplaceable. */
  readonly place: () => void;
  readonly isPending: boolean;
  /** The last refusal, typed. Null when the last attempt succeeded or none ran. */
  readonly error: ApiError | null;
  /** Clears the last refusal, so an edit takes the interstitial off screen. */
  readonly reset: () => void;
}

/**
 * Places the slip against a quote the customer is looking at.
 *
 * `quote` is passed in rather than re-fetched because it is the SOURCE of
 * `seen_ticket_decimal` — the whole-ticket price the customer was shown. Fetching
 * a fresh one at click time would echo back a number nobody had seen, which is
 * the same defect as booking at a moved leg price and is refused for the same
 * reason.
 */
export function usePlaceWager(
  quote: SchemaSlipQuote | undefined,
): PlaceWagerResult {
  const accessToken = useAccessToken();
  const queryClient = useQueryClient();

  const mutation = useMutation<SchemaPlacement, unknown, void>({
    mutationFn: async () => {
      // Read at SUBMIT time, not at render time. Between the render that built
      // this callback and the click that fires it, the customer may have
      // accepted a move or changed the stake, and placing the render-time slip
      // would book a ticket that is no longer the one on screen.
      const state = useSlip.getState();
      if (quote === undefined) throw new Error('no quote to place against');
      if (!isUsableIdempotencyKey(state.attemptKey)) {
        throw new Error('no idempotency key');
      }

      const body = placeWagerRequest(state, quote);
      if (body === null) throw new Error('slip is not placeable');

      return browserApi.placeWager(body, state.attemptKey, {
        accessToken: accessToken ?? undefined,
      });
    },

    retry: false,

    onSuccess: (placement) => {
      // Empties the slip, rotates the key and records the receipt in one commit.
      useSlip.getState().recordPlacement(placement);

      // The stake has left the cash balance for escrow and there is a new ticket
      // in the history, and neither of those arrives on the socket — the stream
      // carries market data, not this customer's money. Both are invalidated
      // rather than written, so the next read is the ledger's own answer and not
      // this client's guess at it.
      void queryClient.invalidateQueries({ queryKey: queryKeys.balance() });
      void queryClient.invalidateQueries({
        queryKey: ['sharpline', 'wagers'],
      });
    },

    onError: (error) => {
      if (!isApiError(error) || !error.isPriceMoved) return;
      // Drops acceptances the server has just told us are stale. It does not
      // accept the new numbers — see the file comment.
      useSlip.getState().applyPriceMoves(error.priceMoves);
    },
  });

  return {
    place: () => {
      if (mutation.isPending) return;
      mutation.mutate();
    },
    isPending: mutation.isPending,
    error: isApiError(mutation.error) ? mutation.error : null,
    reset: () => {
      mutation.reset();
    },
  };
}
