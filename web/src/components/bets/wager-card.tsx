'use client';

/**
 * One placed ticket.
 *
 * # Every number here is the one that was FROZEN AT PLACEMENT
 *
 * `decimal_odds`, `rounding` and `potential_payout_minor` are frozen on the
 * wager and are never recomputed, and this component never recomputes them
 * either. A parlay's price is not always the product of its legs — same-game
 * legs carry a correlation adjustment, a teaser's price is a posted ladder that
 * has nothing to do with the underlying prices at all — so re-deriving one here
 * would produce a different number than the customer was shown and accepted.
 * "To win X" is a promise, and a promise recomputed later is not one.
 *
 * The same holds per leg: `decimal_odds` on a `WagerLeg` is a COPIED price
 * value, not a reference into the price series, and the database freezes it
 * after insert. It describes what the customer took; it does not track the
 * market, and nothing on this surface subscribes it to the stream.
 *
 * # `returned_minor` is the only authority on what a ticket paid
 *
 * Not the payout. A partially-voided parlay returns less than
 * `potential_payout_minor`, and a cash-out returns whatever price was taken. So
 * a running ticket shows what it COULD return and a settled one shows what it
 * DID, and the two are never rendered in the same slot with the same label.
 *
 * # The colour line
 *
 *   ticket price, leg price   PRICES. Never tinted, on any surface.
 *   stake                     money, left plain — see `slip-summary.tsx`.
 *   to return / returned      money, `money` when there is something to receive.
 *   net return                signed: `money` above zero, `loss` below.
 *   leg and ticket status     a settled win is `money`, a loss is `loss`;
 *                             everything else is neutral. This is the one place
 *                             red appears on an ordinary, non-error screen, and
 *                             DESIGN.md lists "settled loss" as exactly that.
 */

import Link from 'next/link';

import { Badge, Button } from '@/components/ui';
import type {
  SchemaLegStatus,
  SchemaWager,
  SchemaWagerLeg,
  SchemaWagerStatus,
} from '@/lib/api/schema';
import { isWagerRunning, wagerKindLabel, wagerStatusLabel } from '@/lib/betting/ticket';
import { useDisplayTimeZone } from '@/lib/client-value';
import { formatMinor, formatMinorSigned, spokenMinor } from '@/lib/money';
import { formatOdds } from '@/lib/odds/format';
import {
  formatLine,
  marketTypeLabel,
  selectionRoleLabel,
} from '@/lib/odds/line';
import { useOddsFormat } from '@/lib/store/preferences';
import { formatAbsolute, toDateTimeAttribute } from '@/lib/time';
import { cn } from '@/lib/utils';
import { CashOutPanel } from './cash-out-panel';

type BadgeTone = 'neutral' | 'money' | 'loss' | 'info';

function wagerTone(status: SchemaWagerStatus): BadgeTone {
  switch (status) {
    case 'won':
      return 'money';
    case 'lost':
      return 'loss';
    case 'cashed_out':
      return 'info';
    // `void` and `push` both return the stake and are NOT the same fact — void
    // is the book cancelling the bet, push is the bet graded as a tie — so they
    // keep different WORDS. They share a tone because neither is a win or a
    // loss, and inventing a fifth hue to separate them would break the palette's
    // one-job-per-colour rule for a distinction the label already carries.
    case 'void':
    case 'push':
    case 'placed':
    case 'open':
    default:
      return 'neutral';
  }
}

function legTone(status: SchemaLegStatus): BadgeTone {
  switch (status) {
    case 'won':
      return 'money';
    case 'lost':
      return 'loss';
    case 'pending':
    case 'void':
    case 'push':
    default:
      return 'neutral';
  }
}

function legStatusLabel(status: SchemaLegStatus): string {
  switch (status) {
    case 'pending':
      return 'Pending';
    case 'won':
      return 'Won';
    case 'lost':
      return 'Lost';
    case 'void':
      return 'Void';
    case 'push':
      return 'Push';
    default:
      return 'Unknown';
  }
}

export interface WagerCardProps {
  readonly wager: SchemaWager;
  /**
   * Whether this is the detail view. The list links its heading to the detail
   * and shows the cash-out control there rather than inline: a page of open
   * tickets each holding an expanded quote panel is a page that has asked the
   * book for a dozen prices nobody requested.
   */
  readonly detail?: boolean | undefined;
}

export function WagerCard({ wager, detail = false }: WagerCardProps) {
  const timeZone = useDisplayTimeZone();
  const oddsFormat = useOddsFormat();

  const running = isWagerRunning(wager.status);
  const returned = wager.returned_minor ?? null;
  const net = wager.net_return_minor ?? null;
  const teaser = wager.teaser_points ?? null;

  const heading = `${wagerKindLabel(wager.kind)} · ${
    wager.legs.length === 1 ? '1 leg' : `${String(wager.legs.length)} legs`
  }`;

  return (
    <article className="flex flex-col rounded-card border border-rule bg-ground-1">
      <header className="flex flex-wrap items-start justify-between gap-2 border-b border-rule px-4 py-3">
        <div className="flex min-w-0 flex-col gap-1">
          <h3 className="t-h3 text-ink">
            {detail ? (
              heading
            ) : (
              <Link
                href={`/bets/${wager.id}`}
                className="rounded-price ui-transition hover:text-ink-2"
              >
                {heading}
              </Link>
            )}
          </h3>
          <p className="t-mono text-ink-muted">
            <time dateTime={toDateTimeAttribute(wager.placed_at)}>
              {formatAbsolute(wager.placed_at, timeZone)}
            </time>
            {teaser === null ? '' : ` · ${String(teaser)}-point teaser`}
            {wager.round_robin_id === null || wager.round_robin_id === undefined
              ? ''
              : ' · round robin ticket'}
            {detail ? ` · ${wager.id}` : ''}
          </p>
        </div>
        <Badge variant={wagerTone(wager.status)}>
          {wagerStatusLabel(wager.status)}
        </Badge>
      </header>

      <ul>
        {wager.legs.map((leg) => (
          <LegRow key={leg.id} leg={leg} oddsFormat={oddsFormat} />
        ))}
      </ul>

      <dl className="flex flex-wrap items-baseline gap-x-6 gap-y-2 border-t border-rule px-4 py-3">
        <Figure label="Stake" value={formatMinor(wager.stake_minor)} spoken={spokenMinor(wager.stake_minor)} />
        {/* A PRICE. Never tinted. */}
        <Figure label="Price" value={formatOdds(wager.decimal_odds, oddsFormat)} />

        {running || returned === null ? (
          <Figure
            label="To return"
            value={formatMinor(wager.potential_payout_minor)}
            spoken={spokenMinor(wager.potential_payout_minor)}
            tone={running ? 'money' : undefined}
          />
        ) : (
          <>
            <Figure
              label="Returned"
              value={formatMinor(returned)}
              spoken={spokenMinor(returned)}
              tone={returned > 0 ? 'money' : undefined}
            />
            {net === null ? null : (
              <Figure
                label="Net"
                value={formatMinorSigned(net)}
                spoken={spokenMinor(net)}
                tone={net >= 0 ? 'money' : 'loss'}
              />
            )}
          </>
        )}
      </dl>

      {running ? (
        <div className="border-t border-rule px-4 py-3">
          {detail ? (
            <CashOutPanel wagerId={wager.id} />
          ) : (
            <Button asChild size="sm" variant="outline">
              <Link href={`/bets/${wager.id}`}>Open ticket</Link>
            </Button>
          )}
        </div>
      ) : null}
    </article>
  );
}

function LegRow({
  leg,
  oddsFormat,
}: {
  readonly leg: SchemaWagerLeg;
  readonly oddsFormat: ReturnType<typeof useOddsFormat>;
}) {
  // `grading_line` is the line the leg actually grades at: the teased line where
  // there is one, otherwise the booked line. It is rendered rather than `line`
  // because it is the number that decides whether this leg wins, and a teased
  // leg keeps its REAL booked price beside it — the book never traded at the
  // moved line, and a forged quote there would corrupt line history.
  const gradingLine = leg.grading_line ?? leg.line ?? null;
  const teased = leg.teased_line ?? null;
  const lineText = formatLine(leg.market_type, leg.role, gradingLine);

  // A placed leg carries IDS and no display names — `WagerLeg` has an
  // `event_id`, a `market_id` and a `selection_id`, and no name for any of them,
  // because it is a frozen copy of a price rather than a view of a catalogue.
  //
  // So the label is built from what the leg genuinely knows: its ROLE, its
  // market type and the line it grades at. Nothing is invented to fill the gap,
  // and nothing is fetched to close it either — resolving names would be a
  // request per leg on a page that is mostly legs, for labels the event link
  // already reaches in one click.
  const label = [
    selectionRoleLabel(leg.role),
    lineText === '' ? null : lineText,
  ]
    .filter((part): part is string => part !== null)
    .join(' ');

  return (
    <li className="flex items-start gap-3 border-b border-rule px-4 py-2 last:border-b-0">
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="t-ui truncate text-ink">{label}</span>
        <span className="t-label truncate text-ink-muted">
          {marketTypeLabel(leg.market_type)}
          {teased === null
            ? ''
            : ` · teased from ${formatLine(leg.market_type, leg.role, leg.line ?? null)}`}
        </span>
        <Link
          href={`/events/${leg.event_id}`}
          className="t-mono truncate rounded-price text-ink-muted ui-transition hover:text-ink-2"
        >
          {leg.event_id}
        </Link>
        <span className="t-mono truncate text-ink-faint">{leg.book_slug}</span>
      </div>

      {/* A PRICE, and the price AT PLACEMENT. Never re-resolved, never tinted. */}
      <span className="t-price-sm shrink-0 whitespace-nowrap pt-0.5 text-ink">
        {formatOdds(leg.decimal_odds, oddsFormat)}
      </span>

      <Badge variant={legTone(leg.status)} className="shrink-0">
        {legStatusLabel(leg.status)}
      </Badge>
    </li>
  );
}

function Figure({
  label,
  value,
  spoken,
  tone,
}: {
  readonly label: string;
  readonly value: string;
  // `| undefined` is not noise: `exactOptionalPropertyTypes` is on, so an
  // optional prop that a caller passes as `tone={x ? 'money' : undefined}` — the
  // natural way to write a conditional tone — is a type error without it.
  readonly spoken?: string | undefined;
  readonly tone?: 'money' | 'loss' | undefined;
}) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="t-label text-ink-muted">{label}</dt>
      <dd
        className={cn(
          't-price-sm tabular whitespace-nowrap',
          tone === 'money'
            ? 'text-money'
            : tone === 'loss'
              ? 'text-loss'
              : 'text-ink',
        )}
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
