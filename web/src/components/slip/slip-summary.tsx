'use client';

/**
 * What the ticket costs and what it pays.
 *
 * # THE COLOUR LINE, and the mistake this component exists to not make
 *
 * DESIGN.md: "green means money · cyan and amber mean direction · red means
 * something is wrong. No price is ever green or red."
 *
 * The tempting mistake here is exact and worth naming: a "potential payout"
 * looks like it belongs to the price system because it was computed FROM odds.
 * It does not. It is an AMOUNT OF MONEY the book will pay, denominated in minor
 * units, and it may be green. The TICKET PRICE beside it — `decimal_odds`, the
 * `6.42` on a parlay — is a price, is computed from the same legs, and may not.
 *
 * So this component tints exactly one figure:
 *
 *   Total stake      money, NOT tinted   `ink`   — see below
 *   Ticket price     PRICE               `ink`   — never tinted, ever
 *   Potential payout money                `money` — THE one green number
 *   Potential profit money, NOT tinted   `ink-2`
 *
 * The stake and the profit are money and DESIGN.md permits green on both. They
 * are left plain anyway, because green is a highlight and a panel with three
 * green numbers has highlighted nothing. The payout is the number somebody is
 * playing for, so it gets the colour; the place-bet CTA below is the only other
 * green object in the panel, and the two together read as "this is the money and
 * this is the button that commits to it".
 *
 * # Nothing here is computed
 *
 * Every figure is read off the quote, and this client has no payout arithmetic
 * at all — deliberately, and it is worth stating as an absence rather than
 * leaving it to be noticed. A parlay's price is not the product of its legs when
 * the legs are correlated, a teaser's price is a posted ladder, and the rounding
 * mode is chosen server-side and reported back on the quote. A client that did
 * the arithmetic itself would produce a number of the right magnitude that is
 * quietly wrong — which is the worst kind, because nobody checks it.
 */

import type { SchemaSlipQuote } from '@/lib/api/schema';
import { formatOdds } from '@/lib/odds/format';
import { useOddsFormat } from '@/lib/store/preferences';
import { formatMinor, spokenMinor } from '@/lib/money';

export interface SlipSummaryProps {
  readonly quote: SchemaSlipQuote;
  /**
   * True when the quote on screen was priced from a slip that has since
   * changed. The figures are dimmed and labelled rather than hidden: blanking
   * them on every keystroke would make the panel flicker, and a stale number
   * that says it is stale is more useful than no number.
   */
  readonly settling: boolean;
}

export function SlipSummary({ quote, settling }: SlipSummaryProps) {
  const oddsFormat = useOddsFormat();

  // Null on a round robin, whose combinations are independent tickets at
  // different prices. A single headline number there would be an average nobody
  // is offered, which is why the API returns null rather than one.
  const ticketPrice =
    quote.decimal_odds === null || quote.decimal_odds === undefined
      ? null
      : formatOdds(quote.decimal_odds, oddsFormat);

  return (
    <dl
      className="flex flex-col gap-1.5 border-t border-rule px-3 py-3"
      aria-busy={settling}
      data-settling={settling}
    >
      {quote.ticket_count > 1 ? (
        <Row
          label="Tickets"
          value={String(quote.ticket_count)}
          settling={settling}
        />
      ) : null}

      {ticketPrice === null ? null : (
        /* A PRICE. `ink`, never a hue — see the file comment. */
        <Row label="Ticket price" value={ticketPrice} settling={settling} />
      )}

      <Row
        label={quote.ticket_count > 1 ? 'Total stake' : 'Stake'}
        value={formatMinor(quote.total_stake_minor)}
        spoken={spokenMinor(quote.total_stake_minor)}
        settling={settling}
      />

      <Row
        label="To return"
        value={formatMinor(quote.potential_payout_minor)}
        spoken={spokenMinor(quote.potential_payout_minor)}
        settling={settling}
        /* THE one green figure in the panel. Money, and the number the customer
         * is playing for. */
        tone="money"
      />

      <Row
        label="Profit"
        value={formatMinor(quote.potential_profit_minor)}
        spoken={spokenMinor(quote.potential_profit_minor)}
        settling={settling}
        tone="muted"
      />
    </dl>
  );
}

interface RowProps {
  readonly label: string;
  readonly value: string;
  /** What a screen reader hears, when the visible string omits the unit. */
  readonly spoken?: string | undefined;
  readonly settling: boolean;
  readonly tone?: 'money' | 'muted' | undefined;
}

function Row({ label, value, spoken, settling, tone }: RowProps) {
  const colour =
    tone === 'money' ? 'text-money' : tone === 'muted' ? 'text-ink-2' : 'text-ink';

  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="t-label text-ink-muted">{label}</dt>
      <dd
        className={`t-price-sm tabular whitespace-nowrap ${colour} ${
          // Not an animation and not a skeleton: the number is real, it is just
          // real for a slip that has moved on. Dimming says so without the panel
          // jumping, which is what a spinner in this slot would do on every
          // keystroke of the stake field.
          settling ? 'opacity-50' : ''
        }`}
      >
        {spoken === undefined ? (
          value
        ) : (
          <>
            <span aria-hidden="true">{value}</span>
            <span className="sr-only">{spoken}</span>
          </>
        )}
      </dd>
    </div>
  );
}
