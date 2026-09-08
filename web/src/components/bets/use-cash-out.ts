'use client';

/**
 * Quoting and taking a cash-out.
 *
 * # The quote is fetched when somebody ASKS, and never on a timer
 *
 * A cash-out quote is a SNAPSHOT at `quoted_at`, not an offer held open. The API
 * says so by omission and explains the omission: there is deliberately no expiry
 * field, "an expiry would imply the book stands behind the number until then,
 * and it does not". Polling it would animate a number the book is not committed
 * to, and it would do so on a surface where the customer is deciding whether to
 * take it — which is the worst possible place for a figure that moves on its
 * own. The customer opens the panel, sees a number with the instant it was
 * computed, and re-reads it deliberately.
 *
 * It is also not fetched for a list. Quoting every open ticket on the history
 * page would be a request per row for a number most rows will never show.
 *
 * # `accepted_value_minor` is the number that was ON SCREEN
 *
 * Not a freshly fetched one. The service re-prices while holding the wager row
 * and refuses with `409 price_moved` when the value has changed; echoing a
 * re-read quote instead of the displayed one defeats the control entirely, which
 * is the API's own phrasing and the same defect as booking a bet at a moved
 * line.
 *
 * # One key per attempt, minted when the panel opens
 *
 * Same discipline as placement: the key survives a `409`, a timeout and a retry
 * of the SAME intention, and rotates only once the ticket is actually closed. A
 * replay answers `200` with the wager as it already stands rather than paying
 * twice.
 */

import { useCallback, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { browserApi } from '@/lib/api/client';
import { isApiError } from '@/lib/api/errors';
import type { ApiError } from '@/lib/api/errors';
import { cashOutQuoteQueryOptions, queryKeys } from '@/lib/api/queries';
import type { SchemaCashOutQuote, SchemaWager } from '@/lib/api/schema';
import { newIdempotencyKey } from '@/lib/betting/idempotency';
import { useAccessToken } from '@/lib/store/auth';

export interface CashOut {
  /** Whether the customer has asked for a quote on this ticket. */
  readonly asked: boolean;
  readonly ask: () => void;
  readonly dismiss: () => void;
  /** Re-reads the quote. The customer's own act, never a timer's. */
  readonly refresh: () => void;

  readonly quote: SchemaCashOutQuote | undefined;
  readonly quoteError: ApiError | null;
  readonly isQuoting: boolean;

  /** Takes the value the customer is looking at. */
  readonly take: () => void;
  readonly isTaking: boolean;
  readonly takeError: ApiError | null;
}

export function useCashOut(wagerId: string): CashOut {
  const accessToken = useAccessToken();
  const queryClient = useQueryClient();

  const [asked, setAsked] = useState(false);
  // Minted once per opening of the panel. Held in state rather than a ref so a
  // remount cannot silently reuse a key from a previous attempt on a ticket
  // that has since been closed.
  const [attemptKey, setAttemptKey] = useState<string | null>(null);

  const quoteQuery = useQuery(
    cashOutQuoteQueryOptions(accessToken, wagerId, asked),
  );

  const mutation = useMutation<SchemaWager, unknown, void>({
    mutationFn: async () => {
      const quote = quoteQuery.data;
      if (quote === undefined) throw new Error('no quote to take');
      if (attemptKey === null) throw new Error('no idempotency key');

      return browserApi.takeCashOut(
        wagerId,
        // The value ON SCREEN. See the file comment.
        quote.value_minor,
        attemptKey,
        { accessToken: accessToken ?? undefined },
      );
    },

    // Same reasoning as placement: idempotency is a property of the key and the
    // server's unique index, not a licence to re-send without a human deciding
    // to.
    retry: false,

    onSuccess: (wager) => {
      // The escrowed stake has been released and the value paid into cash, and
      // the ticket is terminal. None of that arrives on the socket — the stream
      // carries market data, not this customer's money — so all three reads are
      // invalidated rather than patched.
      void queryClient.invalidateQueries({ queryKey: queryKeys.balance() });
      void queryClient.invalidateQueries({ queryKey: ['sharpline', 'wagers'] });
      void queryClient.invalidateQueries({
        queryKey: queryKeys.wager(wager.id),
      });
      void queryClient.removeQueries({
        queryKey: queryKeys.cashOutQuote(wager.id),
      });
      setAsked(false);
      setAttemptKey(null);
    },
  });

  const ask = useCallback(() => {
    setAttemptKey((current) => current ?? newIdempotencyKey());
    setAsked(true);
  }, []);

  const dismiss = useCallback(() => {
    setAsked(false);
    mutation.reset();
    // The key is NOT cleared here. Dismissing the panel is not evidence that
    // nothing was submitted — a customer who pressed Take, saw nothing, and
    // closed the panel is exactly the case the key protects, and reopening it
    // must present the same one.
  }, [mutation]);

  const refresh = useCallback(() => {
    mutation.reset();
    void queryClient.invalidateQueries({
      queryKey: queryKeys.cashOutQuote(wagerId),
    });
  }, [mutation, queryClient, wagerId]);

  return {
    asked,
    ask,
    dismiss,
    refresh,
    quote: quoteQuery.data,
    quoteError: isApiError(quoteQuery.error) ? quoteQuery.error : null,
    isQuoting: quoteQuery.isFetching,
    take: () => {
      if (mutation.isPending) return;
      mutation.mutate();
    },
    isTaking: mutation.isPending,
    takeError: isApiError(mutation.error) ? mutation.error : null,
  };
}
