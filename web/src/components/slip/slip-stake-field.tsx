'use client';

/**
 * The stake field. A string on screen, an integer number of minor units
 * everywhere else.
 *
 * # The field owns its own TEXT, and the store owns the AMOUNT
 *
 * A controlled input whose value is `formatMinorForInput(stakeMinor)` cannot be
 * typed into: entering "12.50" passes through "12." — which parses to 1200 and
 * would be re-rendered as "12.00", moving the caret and eating the keystroke.
 * So the text is local state, the parsed amount is pushed to the store, and the
 * store's value is read back only when it changes for a reason OTHER than typing
 * here (a clear, a preset, a restored slip).
 *
 * # A rejected keystroke leaves the text alone
 *
 * `parseMinor` refuses a third decimal digit rather than truncating it, because
 * truncation silently changes the amount somebody believes they typed. The
 * handler below drops the event on a refusal, which is how every money field in
 * every application behaves: the character does not appear.
 *
 * # No arithmetic happens here at all
 *
 * Not the parse (see `@/lib/money` — it never multiplies a fraction) and not the
 * presets, which are constants in minor units. `stake * 100` does not appear in
 * this file and must not.
 */

import { useId, useState } from 'react';

import { Button, Input, Label } from '@/components/ui';
import {
  MINOR_UNITS_PER_MAJOR,
  MONEY_UNIT,
  formatMinor,
  formatMinorForInput,
  parseMinor,
} from '@/lib/money';
import { cn } from '@/lib/utils';

/**
 * The quick-stake buttons, in MINOR UNITS.
 *
 * Round numbers in the play-money unit, not amounts derived from anything —
 * a preset is a control's value, not data, and nothing here is presented as a
 * recommendation. There is no "max" button: a control that stakes the entire
 * balance in one click is a nudge, and this product carries responsible-gaming
 * limits precisely because that kind of nudge is the thing being modelled
 * rather than the thing being built.
 */
const PRESETS_MINOR: readonly number[] = [
  5 * MINOR_UNITS_PER_MAJOR,
  10 * MINOR_UNITS_PER_MAJOR,
  25 * MINOR_UNITS_PER_MAJOR,
  100 * MINOR_UNITS_PER_MAJOR,
];

export interface SlipStakeFieldProps {
  readonly stakeMinor: number;
  readonly onChange: (minor: number) => void;
  /** Set on a round robin, where the stake is per ticket rather than total. */
  readonly perTicket: boolean;
  /** The spendable balance, when a quote has reported one. */
  readonly cashBalanceMinor: number | null;
  readonly disabled: boolean;
}

export function SlipStakeField({
  stakeMinor,
  onChange,
  perTicket,
  cashBalanceMinor,
  disabled,
}: SlipStakeFieldProps) {
  // Generated rather than fixed, because the slip is MOUNTED TWICE below
  // 1000px: the rail is hidden with `display: none` (which is hydration-safe
  // where a media-query hook is not) while the sheet holds a live copy. Two
  // elements sharing one id would leave `htmlFor` pointing at whichever the
  // browser found first, so the label would drive the wrong field.
  const fieldId = useId();
  const unitId = useId();
  const [text, setText] = useState(() => formatMinorForInput(stakeMinor));

  // Adopts an amount the store changed behind this field's back — a preset, a
  // cleared slip, a restored one. The guard is what stops it from fighting the
  // caret: while typing, the store already holds what the text parses to, so
  // this is a no-op on exactly the renders where a write would be destructive.
  //
  // Adjusted DURING RENDER rather than in an effect, which is React's own
  // prescription for deriving state from a changed prop
  // (https://react.dev/learn/you-might-not-need-an-effect). An effect would
  // paint the stale text first and correct it on a second pass, so a preset
  // would visibly flicker through the old amount; setting state during render
  // makes React discard this render and retry with the new value before
  // committing anything to the DOM. The `!==` guard is what bounds the retry.
  const [lastPushed, setLastPushed] = useState(stakeMinor);
  if (stakeMinor !== lastPushed) {
    setLastPushed(stakeMinor);
    setText(formatMinorForInput(stakeMinor));
  }

  const commit = (next: string): void => {
    const parsed = parseMinor(next);
    if (!parsed.accepted) return;
    setText(next);
    const minor = parsed.minor ?? 0;
    setLastPushed(minor);
    onChange(minor);
  };

  const insufficient =
    cashBalanceMinor !== null && stakeMinor > cashBalanceMinor;

  return (
    <div className="flex flex-col gap-2 px-3 py-3">
      <div className="flex items-baseline justify-between gap-2">
        <Label htmlFor={fieldId} className="t-label text-ink-muted">
          {perTicket ? 'Stake per ticket' : 'Stake'}
        </Label>
        {cashBalanceMinor === null ? null : (
          <span
            className={cn(
              't-price-sm tabular',
              // `loss` here is not a price being tinted red — it is the
              // BALANCE, which is money, and the fact being reported is that
              // something is wrong: the stake is more than there is.
              insufficient ? 'text-loss' : 'text-ink-muted',
            )}
          >
            <span className="t-label mr-1 text-ink-muted">Balance</span>
            {formatMinor(cashBalanceMinor)}
          </span>
        )}
      </div>

      <Input
        id={fieldId}
        // `inputMode` rather than `type="number"`: a number input on iOS offers a
        // keypad with no decimal separator in several locales, and its spinner
        // arrows are a control nobody wants on a stake. `decimal` gets the right
        // keypad and leaves the value a plain string this field can validate.
        inputMode="decimal"
        autoComplete="off"
        placeholder="0.00"
        aria-describedby={unitId}
        aria-invalid={insufficient}
        disabled={disabled}
        value={text}
        onChange={(event) => {
          commit(event.target.value);
        }}
        className="t-price tabular"
      />
      <span id={unitId} className="sr-only">
        {`Amount in ${MONEY_UNIT} units. Play money — no real money moves.`}
      </span>

      <div className="flex flex-wrap gap-1.5">
        {PRESETS_MINOR.map((preset) => (
          <Button
            key={preset}
            type="button"
            size="sm"
            variant="outline"
            disabled={disabled}
            onClick={() => {
              commit(formatMinorForInput(preset));
            }}
          >
            {formatMinor(preset)}
          </Button>
        ))}
      </div>
    </div>
  );
}
