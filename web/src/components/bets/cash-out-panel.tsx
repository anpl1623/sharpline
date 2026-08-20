'use client';

/**
 * Closing a ticket early, with the book's take shown as a number.
 *
 * # This panel exists to make ONE question answerable
 *
 * "What did the book charge me to close early?"
 *
 * The price is built from the FAIR value — the product of the devigged fair
 * probabilities of the legs still pending, devigged against the sharp reference
 * book per ADR 0006, times the graded multiplier of the legs already decided —
 * and then an explicitly NAMED haircut is subtracted:
 *
 *     fair_value = round(potential_payout × survival_probability)
 *     value      = round(fair_value × (1 − margin_bps / 10000))
 *     margin     = fair_value − value
 *
 * Quoting off the OFFERED price instead would take the same money and hide it
 * inside the vig, entangled with the market's own margin, where the two are not
 * separable from the outside. The take is identical either way; only its
 * auditability changes — and this panel is where that auditability is spent, so
 * every one of those four numbers is rendered rather than just the last.
 *
 * # Confirm before taking
 *
 * Two clicks, deliberately. It is an irreversible state transition on a placed
 * ticket that pays a number the customer may not have read yet, and the second
 * click NAMES the amount rather than saying "Confirm" — so the thing being
 * agreed to is on the control that agrees to it.
 *
 * # The colour line
 *
 *   value        money — this is what the customer receives
 *   fair value   plain — money, but a reference figure, not the payment
 *   the take     plain — money, and nothing is WRONG about the book charging it
 *   net return   money when positive, `loss` when negative — closing early can
 *                crystallise a loss, and that is the "something is wrong" the
 *                red token is for rather than a price being tinted
 *   survival     a PROBABILITY. Not money, not a price, never tinted.
 */

import { Badge, Button } from '@/components/ui';
import { formatMinor, formatMinorSigned, spokenMinor } from '@/lib/money';
import { renderPercent } from '@/lib/odds/format';
import { formatAbsolute } from '@/lib/time';
import type { SchemaCashOutQuote } from '@/lib/api/schema';
import { useDisplayTimeZone } from '@/lib/client-value';
import { cn } from '@/lib/utils';
import { useCashOut } from './use-cash-out';

/** Basis points to a percentage: 500 -> "5.00%". */
function formatBps(bps: number): string {
  return renderPercent(bps / 10_000, 2);
}

export interface CashOutPanelProps {
  readonly wagerId: string;
}

export function CashOutPanel({ wagerId }: CashOutPanelProps) {
  const cashOut = useCashOut(wagerId);

  if (!cashOut.asked) {
    return (
      <Button type="button" size="sm" variant="outline" onClick={cashOut.ask}>
        Cash out
      </Button>
    );
  }

  return (
    <section
      aria-label="Cash out"
      className="flex flex-col gap-3 rounded-card border border-rule bg-ground-2 p-3"
    >
      {cashOut.quote === undefined ? (
        <Pending cashOut={cashOut} />
      ) : (
        <Quoted quote={cashOut.quote} cashOut={cashOut} />
      )}
    </section>
  );
}

function Pending({ cashOut }: { readonly cashOut: ReturnType<typeof useCashOut> }) {
  if (cashOut.isQuoting) {
    return (
      <p className="t-ui text-ink-2" role="status">
        Pricing this ticket…
      </p>
    );
  }

  if (cashOut.quoteError !== null) {
    return (
      <div className="flex flex-col gap-2">
        {/* Rendered as a STATE, not a fault. `409 cash_out_unavailable` means the
            ticket is already terminal, a leg is void or pushed, a pending leg's
            reference price is missing or stale, or the computed value is not
            positive — and the server names which in a fixed message. It is a
            correct answer to "what will you pay", so it gets no warning colour. */}
        <p className="t-ui text-ink-2">{cashOut.quoteError.message}</p>
        <div className="flex gap-2">
          <Button type="button" size="sm" variant="ghost" onClick={cashOut.refresh}>
            Try again
          </Button>
          <Button type="button" size="sm" variant="ghost" onClick={cashOut.dismiss}>
            Close
          </Button>
        </div>
      </div>
    );
  }

  return (
    <Button type="button" size="sm" variant="ghost" onClick={cashOut.dismiss}>
      Close
    </Button>
  );
}

function Quoted({
  quote,
  cashOut,
}: {
  readonly quote: SchemaCashOutQuote;
  readonly cashOut: ReturnType<typeof useCashOut>;
}) {
  const timeZone = useDisplayTimeZone();
  const profitable = quote.net_return_minor >= 0;

  // A `409 price_moved` on the take carries the value the service now holds.
  // Shown as the new number rather than as an error, because it is the same
  // fact as a moved price on the slip: the market changed while somebody was
  // deciding, and the decision is still theirs.
  const moved =
    cashOut.takeError !== null && cashOut.takeError.isPriceMoved
      ? (cashOut.takeError.priceMoves.find((move) => move.scope === 'cash_out')
          ?.current_value_minor ?? null)
      : null;

  return (
    <>
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <span className="t-label text-ink-muted">The book will pay</span>
        <span className="t-price-lg tabular text-money">
          <span aria-hidden="true">{formatMinor(quote.value_minor)}</span>
          <span className="sr-only">{spokenMinor(quote.value_minor)}</span>
        </span>
      </div>

      <dl className="flex flex-col gap-1">
        <Line
          label="Ticket is worth"
          value={formatMinor(quote.fair_value_minor)}
          note="fair value, before the book's take"
        />
        <Line
          label={`The book takes (${formatBps(quote.margin_bps)})`}
          value={formatMinor(quote.margin_minor)}
        />
        <Line
          label="Against a stake of"
          value={formatMinor(quote.stake_minor)}
        />
        <Line
          label="Net"
          value={formatMinorSigned(quote.net_return_minor)}
          tone={profitable ? 'money' : 'loss'}
        />
        <Line
          label="Chance every remaining leg lands"
          /* A PROBABILITY — devigged against the sharp reference book, never
             derived from the offered price. Not money and not a price, so it is
             never tinted. */
          value={renderPercent(quote.survival_probability, 1)}
        />
        <Line
          label="Legs still running"
          value={String(quote.pending_leg_count)}
        />
      </dl>

      <p className="t-mono text-ink-muted">
        {`quoted ${formatAbsolute(quote.quoted_at, timeZone)} · a snapshot, not an offer held open`}
      </p>

      {moved === null ? null : (
        <div className="flex flex-col gap-2 rounded-price border border-rule-hi bg-ground-3 p-2">
          <Badge variant="neutral">Value moved</Badge>
          <p className="t-ui text-ink-2">
            The book now offers{' '}
            <span className="t-price-sm text-ink">{formatMinor(moved)}</span>.
            Re-read the quote to take it.
          </p>
        </div>
      )}

      {cashOut.takeError !== null && !cashOut.takeError.isPriceMoved ? (
        <p className="t-ui text-loss" role="alert">
          {cashOut.takeError.message}
        </p>
      ) : null}

      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          size="sm"
          variant="primary"
          disabled={cashOut.isTaking || moved !== null}
          onClick={cashOut.take}
        >
          {/* The amount is ON the control that agrees to it. */}
          {cashOut.isTaking
            ? 'Closing…'
            : `Take ${formatMinor(quote.value_minor)}`}
        </Button>
        <Button
          type="button"
          size="sm"
          variant="ghost"
          disabled={cashOut.isTaking}
          onClick={cashOut.refresh}
        >
          Re-read
        </Button>
        <Button
          type="button"
          size="sm"
          variant="ghost"
          disabled={cashOut.isTaking}
          onClick={cashOut.dismiss}
        >
          Keep the ticket
        </Button>
      </div>
    </>
  );
}

function Line({
  label,
  value,
  note,
  tone,
}: {
  readonly label: string;
  readonly value: string;
  readonly note?: string | undefined;
  readonly tone?: 'money' | 'loss' | undefined;
}) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="t-label text-ink-muted">
        {label}
        {note === undefined ? null : (
          <span className="block t-label normal-case tracking-normal text-ink-faint">
            {note}
          </span>
        )}
      </dt>
      <dd
        className={cn(
          't-price-sm tabular whitespace-nowrap',
          tone === 'money'
            ? 'text-money'
            : tone === 'loss'
              ? 'text-loss'
              : 'text-ink-2',
        )}
      >
        {value}
      </dd>
    </div>
  );
}
