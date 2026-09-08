'use client';

/**
 * What the book actually booked.
 *
 * # Every number here came back from the server
 *
 * The stake, the payout and the ticket price are read off the `Placement`, not
 * carried over from the slip that produced it. They can legitimately differ: an
 * `accept_better_price` improvement books a longer price than the slip held, and
 * a round robin's total is the sum across combinations. A receipt that echoed
 * the request would be a receipt for what was asked rather than for what
 * happened, which is the one thing a receipt must not be.
 *
 * # `replayed` is reported, not hidden
 *
 * A `200` with `replayed: true` means this `Idempotency-Key` had already placed
 * and the tickets were read back rather than written. It is NOT an error and it
 * renders as an ordinary placement — but it is said out loud, because the
 * situation it describes is "your earlier submit did land, and this one changed
 * nothing", and that is precisely what somebody who pressed the button twice
 * after a timeout needs to be told. Silently showing them a ticket would leave
 * them unsure whether they now hold one or two.
 */

import Link from 'next/link';

import { Badge, Button } from '@/components/ui';
import type { SchemaPlacement } from '@/lib/api/schema';
import { wagerKindLabel } from '@/lib/betting/ticket';
import { formatMinor, spokenMinor } from '@/lib/money';
import { formatOdds } from '@/lib/odds/format';
import { useOddsFormat } from '@/lib/store/preferences';

export interface SlipReceiptPanelProps {
  readonly placement: SchemaPlacement;
  readonly onDismiss: () => void;
}

export function SlipReceiptPanel({
  placement,
  onDismiss,
}: SlipReceiptPanelProps) {
  const oddsFormat = useOddsFormat();

  const tickets = placement.wagers;
  const first = tickets[0];
  const roundRobin = placement.round_robin ?? null;

  return (
    <div className="flex flex-col gap-3 px-4 py-4">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="t-h3 text-ink">
          {placement.replayed ? 'Already placed' : 'Bet placed'}
        </h3>
        {placement.replayed ? <Badge variant="info">Replay</Badge> : null}
      </div>

      {placement.replayed ? (
        <p className="t-body text-ink-2">
          This submit had already been booked, so nothing new was written. The
          ticket below is the one that exists.
        </p>
      ) : null}

      <dl className="flex flex-col gap-1.5">
        <ReceiptRow
          label="Tickets"
          value={
            roundRobin === null
              ? `${String(tickets.length)} · ${first === undefined ? '' : wagerKindLabel(first.kind)}`
              : `${String(roundRobin.combination_count)} · round robin by ${roundRobin.sizes.map(String).join('s and ')}s`
          }
        />

        {/* A PRICE. Not tinted — the money/price line holds on the receipt
            exactly as it does on the summary. Absent on a round robin, whose
            combinations are separate tickets at separate prices. */}
        {tickets.length === 1 && first !== undefined ? (
          <ReceiptRow
            label="Ticket price"
            value={formatOdds(first.decimal_odds, oddsFormat)}
          />
        ) : null}

        <ReceiptRow
          label="Staked"
          value={formatMinor(placement.total_stake_minor)}
          spoken={spokenMinor(placement.total_stake_minor)}
        />
        <ReceiptRow
          label="To return"
          value={formatMinor(placement.potential_payout_minor)}
          spoken={spokenMinor(placement.potential_payout_minor)}
          tone="money"
        />
      </dl>

      <p className="t-ui text-ink-muted">
        The stake has left your cash balance and is held in escrow until the
        ticket settles.
      </p>

      <div className="flex flex-wrap gap-2">
        <Button asChild size="sm" variant="outline">
          <Link
            href={
              tickets.length === 1 && first !== undefined
                ? `/bets/${first.id}`
                : '/bets'
            }
          >
            {tickets.length === 1 ? 'View ticket' : 'View tickets'}
          </Link>
        </Button>
        <Button type="button" size="sm" variant="ghost" onClick={onDismiss}>
          Build another
        </Button>
      </div>
    </div>
  );
}

function ReceiptRow({
  label,
  value,
  spoken,
  tone,
}: {
  readonly label: string;
  readonly value: string;
  readonly spoken?: string | undefined;
  readonly tone?: 'money' | undefined;
}) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="t-label text-ink-muted">{label}</dt>
      <dd
        className={`t-price-sm tabular whitespace-nowrap ${
          tone === 'money' ? 'text-money' : 'text-ink'
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
