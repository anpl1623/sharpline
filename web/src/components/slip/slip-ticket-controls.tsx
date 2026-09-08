'use client';

/**
 * The controls that decide WHAT KIND of ticket the slip is: the kind selector,
 * the round robin's combination sizes, and a teaser's points.
 *
 * # A disabled kind says why
 *
 * `kindAvailability` returns a reason with every refusal and the reason is
 * rendered, because a customer holding four legs who finds "Straight" greyed out
 * is owed the sentence "a straight is one selection". The reasons are ARITY and
 * MARKET-TYPE facts — things this client knows for certain from the slip alone.
 * Nothing about balance, limits, suspension or price movement is decided here;
 * all of that is server state, and a control disabled on a guess at server state
 * is a control that is wrong the moment the guess ages.
 *
 * # The round robin states what it costs before it is chosen
 *
 * Its ticket count is a binomial coefficient, so "by 2s on four selections" is
 * six tickets and six stakes. The count sits on the control itself rather than
 * only in the summary, because the moment somebody needs it is the moment they
 * are choosing, not the moment they have already multiplied their exposure by
 * six.
 *
 * # The teaser has no ladder here, and that is deliberate
 *
 * `odds/parlay.go` is explicit that a teased price cannot be derived from the
 * posted one without an empirical model of how the sport's margins are
 * distributed, and that "inventing one here would be fabricated data of exactly
 * the kind the project forbids". A frontend that offered "6 · 6.5 · 7" as though
 * those were prices this book posts would be inventing the same number one layer
 * further from anyone who would notice. So the control is a plain stepper over
 * the half-point grid lines are quoted on, it starts EMPTY, and whether the
 * resulting ticket can be priced at all is the server's answer — rendered
 * verbatim, in the panel's impediment list.
 */

import { useId } from 'react';
import { Minus, Plus } from 'lucide-react';

import { Button } from '@/components/ui';
import type { SchemaWagerKind } from '@/lib/api/schema';
import {
  MAX_TEASER_POINTS,
  MIN_TEASER_POINTS,
  TEASER_POINT_STEP,
  binomial,
  combinationCount,
  kindAvailability,
  roundRobinSizeOptions,
  wagerKindLabel,
} from '@/lib/betting/ticket';
import type { TicketShapeLeg } from '@/lib/betting/ticket';
import { formatLineNumber } from '@/lib/odds/line';
import { cn } from '@/lib/utils';

// -----------------------------------------------------------------------------
// Kind
// -----------------------------------------------------------------------------

export interface SlipKindControlProps {
  readonly kind: SchemaWagerKind;
  readonly legs: readonly TicketShapeLeg[];
  readonly onSelect: (kind: SchemaWagerKind) => void;
}

export function SlipKindControl({
  kind,
  legs,
  onSelect,
}: SlipKindControlProps) {
  const availability = kindAvailability(legs);
  const blocked = availability.filter(
    (entry) => !entry.available && entry.reason !== null,
  );

  return (
    <div className="flex flex-col gap-1.5 px-3 py-2">
      <div
        role="group"
        aria-label="Ticket kind"
        className="flex flex-wrap items-center gap-1"
      >
        {availability.map((entry) => {
          const active = entry.kind === kind;
          return (
            <Button
              key={entry.kind}
              type="button"
              size="sm"
              variant={active ? 'default' : 'ghost'}
              aria-pressed={active}
              disabled={!entry.available}
              onClick={() => {
                onSelect(entry.kind);
              }}
            >
              {wagerKindLabel(entry.kind)}
            </Button>
          );
        })}
      </div>

      {blocked.length === 0 ? null : (
        <ul className="flex flex-col gap-0.5">
          {blocked.map((entry) => (
            <li key={entry.kind} className="t-label text-ink-faint">
              {/* `ink-faint` is 3.1:1 and DESIGN.md bars it from BODY text.
                  This is a disabled control's own explanation sitting beside it,
                  which is the decorative/disabled case the token is for — and
                  the same fact reaches assistive technology through the button's
                  `disabled` state rather than through this contrast. */}
              {wagerKindLabel(entry.kind)}: {entry.reason}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// -----------------------------------------------------------------------------
// Round robin
// -----------------------------------------------------------------------------

export interface RoundRobinControlProps {
  readonly legCount: number;
  readonly sizes: readonly number[];
  readonly onToggleSize: (size: number) => void;
}

export function RoundRobinControl({
  legCount,
  sizes,
  onToggleSize,
}: RoundRobinControlProps) {
  const options = roundRobinSizeOptions(legCount);
  const tickets = combinationCount(legCount, sizes);

  return (
    <div className="flex flex-col gap-1.5 border-t border-rule px-3 py-2">
      <span className="t-label text-ink-muted">Combination sizes</span>

      <div
        role="group"
        aria-label="Round robin combination sizes"
        className="flex flex-wrap items-center gap-1"
      >
        {options.map((size) => {
          const active = sizes.includes(size);
          return (
            <Button
              key={size}
              type="button"
              size="sm"
              variant={active ? 'default' : 'ghost'}
              aria-pressed={active}
              onClick={() => {
                onToggleSize(size);
              }}
            >
              <span aria-hidden="true">By {size}s</span>
              <span className="sr-only">
                {`Combinations of ${String(size)} — ${String(binomial(legCount, size))} tickets`}
              </span>
            </Button>
          );
        })}
      </div>

      <p className="t-ui text-ink-2">
        {sizes.length === 0
          ? 'Choose at least one combination size.'
          : tickets === 1
            ? '1 ticket, and the stake below is on it.'
            : `${String(tickets)} separate tickets, and the stake below is on EACH of them.`}
      </p>
    </div>
  );
}

// -----------------------------------------------------------------------------
// Teaser
// -----------------------------------------------------------------------------

export interface TeaserControlProps {
  readonly points: number | null;
  readonly onChange: (points: number | null) => void;
}

export function TeaserControl({ points, onChange }: TeaserControlProps) {
  // See the note in `slip-stake-field.tsx`: the slip is mounted twice below
  // 1000px, so every id this file emits has to be per-instance.
  const labelId = useId();
  const step = (delta: number): void => {
    const next = (points ?? 0) + delta;
    if (next < MIN_TEASER_POINTS) {
      onChange(null);
      return;
    }
    if (next > MAX_TEASER_POINTS) return;
    onChange(next);
  };

  return (
    <div className="flex flex-col gap-1.5 border-t border-rule px-3 py-2">
      <span className="t-label text-ink-muted" id={labelId}>
        Points every line moves
      </span>

      <div
        className="flex items-center gap-2"
        role="group"
        aria-labelledby={labelId}
      >
        <Button
          type="button"
          size="sm"
          variant="outline"
          aria-label={`Decrease teaser points by ${formatLineNumber(TEASER_POINT_STEP)}`}
          disabled={points === null}
          onClick={() => {
            step(-TEASER_POINT_STEP);
          }}
        >
          <Minus aria-hidden="true" />
        </Button>

        {/* Not an editable field. A teaser's points are a value from a coarse
            grid, and a free-text box invites a third decimal that the grid does
            not have. The number is the readout, the buttons are the control. */}
        <output
          className={cn(
            't-price tabular min-w-[3.5rem] rounded-price border border-rule',
            'bg-ground-3 px-2 py-2 text-center',
            points === null ? 'text-ink-faint' : 'text-ink',
          )}
        >
          {points === null ? '—' : formatLineNumber(points)}
        </output>

        <Button
          type="button"
          size="sm"
          variant="outline"
          aria-label={`Increase teaser points by ${formatLineNumber(TEASER_POINT_STEP)}`}
          disabled={points !== null && points >= MAX_TEASER_POINTS}
          onClick={() => {
            step(TEASER_POINT_STEP);
          }}
        >
          <Plus aria-hidden="true" />
        </Button>
      </div>

      <p className="t-ui text-ink-2">
        {points === null
          ? 'Set the points before this ticket can be priced.'
          : 'Every leg’s line moves by this much, in that leg’s favour.'}
      </p>
    </div>
  );
}
