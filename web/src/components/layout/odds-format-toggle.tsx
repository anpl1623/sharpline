'use client';

/**
 * The odds-format control. American / Decimal / Fractional.
 *
 * American is the default and stays the default. DESIGN.md § "Category
 * conventions deliberately kept" lists it explicitly: innovating on the default
 * odds format buys nothing and costs literacy for the audience that navigates a
 * book without instruction.
 *
 * # Why a real radiogroup and not three buttons that look like one
 *
 * This is a single-choice control over a small mutually-exclusive set, which is
 * precisely a radio group. Implemented as three `role="radio"` buttons inside a
 * `role="radiogroup"`, with the WAI-ARIA roving-tabindex pattern: the group has
 * ONE tab stop (the checked option), and the arrow keys move between options.
 * Three plain buttons would put three stops in the header's tab order and would
 * announce as three unrelated commands rather than as one setting with a current
 * value.
 *
 * Selection follows focus, which is the standard radiogroup behaviour and is safe
 * here because the change is free, instant and reversible — it re-renders prices
 * from the same canonical decimal odds and touches no server state.
 *
 * # The visible label is short; the accessible name is not
 *
 * `AMER` / `DEC` / `FRAC` in the 11px label step, because this sits in a 48px
 * header beside four other controls. Each option's `aria-label` carries the full
 * words ("American odds"), so the abbreviation never reaches a screen reader.
 *
 * # Conversion happens on the client, from decimal
 *
 * The store holds a display preference and nothing else. `decimal_odds` is the
 * canonical value on both REST and the WebSocket, and every rendered price is
 * converted from it at the cell — which is what makes a REST-established row and
 * a stream-updated one render identically. The API's `odds_format` parameter is
 * never sent.
 */

import { useCallback, useRef } from 'react';
import type { KeyboardEvent } from 'react';

import { ODDS_FORMATS, oddsFormatLabel } from '@/lib/odds/format';
import type { OddsFormat } from '@/lib/odds/format';
import { useOddsFormat, useSetOddsFormat } from '@/lib/store/preferences';
import { cn } from '@/lib/utils';

/** Dense header labels. The full words live in each option's accessible name. */
const SHORT_LABEL: Record<OddsFormat, string> = {
  american: 'AMER',
  decimal: 'DEC',
  fractional: 'FRAC',
};

export interface OddsFormatToggleProps {
  readonly className?: string | undefined;
}

export function OddsFormatToggle({ className }: OddsFormatToggleProps) {
  const format = useOddsFormat();
  const setFormat = useSetOddsFormat();

  const buttons = useRef<(HTMLButtonElement | null)[]>([]);

  const focusOption = useCallback(
    (index: number) => {
      const wrapped =
        ((index % ODDS_FORMATS.length) + ODDS_FORMATS.length) %
        ODDS_FORMATS.length;
      const next = ODDS_FORMATS[wrapped];
      if (next === undefined) return;
      setFormat(next);
      buttons.current[wrapped]?.focus();
    },
    [setFormat],
  );

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLButtonElement>, index: number) => {
      switch (event.key) {
        case 'ArrowRight':
        case 'ArrowDown':
          event.preventDefault();
          focusOption(index + 1);
          break;
        case 'ArrowLeft':
        case 'ArrowUp':
          event.preventDefault();
          focusOption(index - 1);
          break;
        case 'Home':
          event.preventDefault();
          focusOption(0);
          break;
        case 'End':
          event.preventDefault();
          focusOption(ODDS_FORMATS.length - 1);
          break;
        default:
          break;
      }
    },
    [focusOption],
  );

  return (
    <div
      role="radiogroup"
      aria-label="Odds format"
      className={cn(
        'flex h-9 shrink-0 items-center gap-0.5 rounded-price border border-rule bg-ground-1 p-0.5',
        className,
      )}
    >
      {ODDS_FORMATS.map((option, index) => {
        const checked = option === format;
        return (
          <button
            key={option}
            ref={(node) => {
              buttons.current[index] = node;
            }}
            type="button"
            role="radio"
            aria-checked={checked}
            aria-label={`${oddsFormatLabel(option)} odds`}
            /* Roving tabindex: the group is one tab stop. */
            tabIndex={checked ? 0 : -1}
            onClick={() => {
              setFormat(option);
            }}
            onKeyDown={(event) => {
              onKeyDown(event, index);
            }}
            className={cn(
              'inline-flex h-8 items-center rounded-price px-1.5 t-label ui-transition',
              checked
                ? 'bg-ground-3 text-ink'
                : 'text-ink-muted hover:text-ink',
            )}
          >
            {SHORT_LABEL[option]}
          </button>
        );
      })}
    </div>
  );
}
