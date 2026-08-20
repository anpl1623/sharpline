'use client';

/**
 * One leg on the slip, and the accept/reject interstitial when its price moves.
 *
 * # THE MONEY / PRICE COLOUR LINE, stated where it is easiest to get wrong
 *
 * DESIGN.md: "green means money · cyan and amber mean direction · red means
 * something is wrong. No price is ever green or red."
 *
 * Everything on this row is a PRICE. The leg's decimal, the line, the number it
 * moved to — none of them is money and none of them is ever tinted. They render
 * in `ink` and `ink-muted`, exactly as a board cell does.
 *
 * Direction is carried by a tinted BADGE and by the two numerals sitting beside
 * each other, never by colouring the price itself. That is the same division the
 * board makes, where the delta rail is coloured and the numeral is not, and
 * keeping it here means a price looks like a price on both surfaces.
 *
 * The only money on this row is the round robin's per-ticket stake, and it is
 * not tinted either — see `slip-summary.tsx` for where the one green number in
 * the whole panel lives and why it is only one.
 *
 * # The interstitial is not an error
 *
 * A moved price is the market doing the thing this product exists to show. It is
 * rendered as a statement with two controls, not as a failure with a warning
 * colour: the loss hue is reserved for something being wrong, and nothing is
 * wrong here. What it DOES do is block the button, because the number the
 * customer agreed to is no longer the number on offer, and booking it anyway is
 * the defect.
 */

import { X } from 'lucide-react';

import { Badge, Button } from '@/components/ui';
import { formatOdds } from '@/lib/odds/format';
import { formatLine, marketTypeLabel } from '@/lib/odds/line';
import { useOddsFormat } from '@/lib/store/preferences';
import type { OddsFormat } from '@/lib/store/preferences';
import { legEffectiveDecimal, legEffectiveLine } from '@/lib/store/slip';
import type { SlipLeg } from '@/lib/store/slip';
import type { LegWatch } from './slip-model';

export interface SlipLegRowProps {
  readonly leg: SlipLeg;
  /** The live comparison, computed once for the whole slip. */
  readonly watch: LegWatch;
  /** Whether an improvement at an unchanged line is waved through. */
  readonly acceptBetterPrice: boolean;
  readonly onRemove: (selectionId: string) => void;
  readonly onAccept: (
    selectionId: string,
    decimal: number,
    line: number | null,
  ) => void;
}

export function SlipLegRow({
  leg,
  watch,
  acceptBetterPrice,
  onRemove,
  onAccept,
}: SlipLegRowProps) {
  const oddsFormat = useOddsFormat();

  const agreedDecimal = legEffectiveDecimal(leg);
  const agreedLine = legEffectiveLine(leg);
  const agreedPrice = formatOdds(agreedDecimal, oddsFormat);
  const lineText = formatLine(leg.marketType, leg.role, agreedLine);

  const waved = acceptBetterPrice && watch.improved;
  const showsMove = watch.moved && !waved;

  return (
    <li className="flex flex-col gap-2 border-b border-rule px-3 py-2 last:border-b-0">
      <div className="flex items-start gap-2">
        <div className="flex min-w-0 flex-1 flex-col gap-0.5">
          <span className="t-ui truncate text-ink">{leg.selectionName}</span>
          <span className="t-label truncate text-ink-muted">
            {marketTypeLabel(leg.marketType)}
            {lineText === '' ? '' : ` ${lineText}`}
          </span>
          <span className="t-label truncate text-ink-faint">
            {leg.eventName}
          </span>
        </div>

        {/* The agreed price. `t-price-sm` is DESIGN.md's bet-slip price step,
            13px/1.00/550 with tabular figures — the same family as the board,
            one step down, and never mono. */}
        <span className="t-price-sm shrink-0 whitespace-nowrap pt-0.5 text-ink">
          {agreedPrice}
        </span>

        <Button
          type="button"
          variant="ghost"
          size="sm"
          aria-label={`Remove ${leg.selectionName} from the bet slip`}
          className="-mt-1 size-8 shrink-0 px-0"
          onClick={() => {
            onRemove(leg.selectionId);
          }}
        >
          <X aria-hidden="true" />
        </Button>
      </div>

      {/* The book is engineering-layer provenance, so it is mono. A customer
          reading past it sees texture rather than another number. */}
      <span className="t-mono truncate text-ink-muted">{leg.bookSlug}</span>

      {waved ? (
        <PriceImproved
          from={agreedPrice}
          to={formatOdds(watch.currentDecimal ?? agreedDecimal, oddsFormat)}
        />
      ) : null}

      {showsMove ? (
        <PriceMoved
          leg={leg}
          watch={watch}
          oddsFormat={oddsFormat}
          agreedPrice={agreedPrice}
          onAccept={onAccept}
          onRemove={onRemove}
        />
      ) : null}
    </li>
  );
}

/**
 * The move the customer opted into.
 *
 * Shown rather than silently applied. `accept_better_price` is a standing
 * consent, not a reason to hide what it consented to, and a customer who set it
 * once weeks ago should still be able to see the book taking it.
 */
function PriceImproved({
  from,
  to,
}: {
  readonly from: string;
  readonly to: string;
}) {
  return (
    <p className="t-label text-ink-muted">
      <span className="sr-only">Price improved. </span>
      {from} → {to} · booked at the better price
    </p>
  );
}

interface PriceMovedProps {
  readonly leg: SlipLeg;
  readonly watch: LegWatch;
  readonly oddsFormat: OddsFormat;
  readonly agreedPrice: string;
  readonly onAccept: (
    selectionId: string,
    decimal: number,
    line: number | null,
  ) => void;
  readonly onRemove: (selectionId: string) => void;
}

/**
 * The accept/reject interstitial. DESIGN.md keeps this convention deliberately
 * ("persistent bet slip with the price-change accept/reject interstitial").
 *
 * Two numbers and two controls, and no third option: a customer whose price
 * moved either takes the new one or takes the leg off. There is no "place
 * anyway" and no timer that accepts on their behalf.
 *
 * A LINE move is called out separately from a price move even when both
 * happened, because they are different questions. `movement` compares two
 * prices; a line compares two BETS — a spread of -4 loses games that -3.5 wins —
 * so consent to the price is not consent to the handicap, and the accept control
 * names both.
 */
function PriceMoved({
  leg,
  watch,
  oddsFormat,
  agreedPrice,
  onAccept,
  onRemove,
}: PriceMovedProps) {
  const current = watch.currentDecimal;
  const currentPrice =
    current === null ? null : formatOdds(current, oddsFormat);
  const currentLineText = formatLine(
    leg.marketType,
    leg.role,
    watch.currentLine,
  );
  const agreedLineText = formatLine(
    leg.marketType,
    leg.role,
    legEffectiveLine(leg),
  );

  // `in` is the token for "implied probability ROSE", which is a SHORTENED
  // price. The badge names the customer-facing fact and the hue names the
  // direction; they agree because the vocabulary is the same one the board uses.
  const shortened = watch.movement === 'shortened';
  const directionVariant = shortened ? 'delta-in' : 'delta-out';

  return (
    <div className="flex flex-col gap-2 rounded-price border border-rule-hi bg-ground-2 p-2">
      <div className="flex flex-wrap items-center gap-1.5">
        {watch.movement === 'unchanged' ? null : (
          <Badge variant={directionVariant}>
            {shortened ? 'Shortened' : 'Lengthened'}
          </Badge>
        )}
        {watch.lineMoved ? <Badge variant="neutral">Line moved</Badge> : null}
      </div>

      <p className="t-ui text-ink-2">
        {watch.lineMoved ? (
          <>
            <span className="t-price-sm text-ink-muted">
              {agreedLineText === '' ? agreedPrice : `${agreedLineText} ${agreedPrice}`}
            </span>
            {' → '}
            <span className="t-price-sm text-ink">
              {currentLineText === '' ? (currentPrice ?? '—') : `${currentLineText} ${currentPrice ?? '—'}`}
            </span>
          </>
        ) : (
          <>
            <span className="t-price-sm text-ink-muted">{agreedPrice}</span>
            {' → '}
            <span className="t-price-sm text-ink">{currentPrice ?? '—'}</span>
          </>
        )}
      </p>

      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={current === null}
          onClick={() => {
            if (current === null) return;
            onAccept(leg.selectionId, current, watch.currentLine);
          }}
        >
          {/* Names what is being taken, not just "Accept": a row of identical
              Accept buttons on a four-leg slip is four decisions that look like
              one. */}
          Take {currentPrice ?? 'the new price'}
        </Button>
        <Button
          type="button"
          size="sm"
          variant="ghost"
          onClick={() => {
            onRemove(leg.selectionId);
          }}
        >
          Remove leg
        </Button>
      </div>
    </div>
  );
}
