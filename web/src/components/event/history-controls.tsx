'use client';

import {
  Button,
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui';
import type {
  SchemaBookKind,
  SchemaHistoryResolution,
} from '@/lib/api/schema';

/**
 * The line-movement chart's query controls: how far back, and whose prices.
 *
 * # Why a book selector is mandatory rather than a refinement
 *
 * `GET /selections/{id}/history` REQUIRES `book`. There is no all-books variant
 * and there should not be: a single series mixing two books' quotes is not a
 * line-movement chart, it is two of them overlaid with no way to tell which
 * point came from where. The control exists because the API's shape says one
 * book at a time, not because somebody wanted a filter.
 *
 * # Why the window is a preset and not a date range
 *
 * `prices` is a Timescale hypertable partitioned on `observed_at`, and `from`
 * is required so a read can exclude chunks. Each preset carries the resolution
 * that fits inside the API's `max_points` for that width — the API REFUSES an
 * over-wide request rather than truncating it, so choosing the pair together is
 * the client's job, and getting it wrong is a 422 the chart has to explain.
 */

export type HistoryWindowKey = '15m' | '1h' | '6h' | '24h';

export interface HistoryWindow {
  readonly key: HistoryWindowKey;
  readonly label: string;
  /** How far back `from` is placed. */
  readonly seconds: number;
  /**
   * The resolution that fits this width.
   *
   * `raw` on the shortest window on purpose: at 15 minutes the stored quotes
   * ARE the chart, and bucketing them would smooth away the individual moves
   * that a 15 minute window is being opened to look at. The API allows raw for
   * windows up to 6h; the wider presets bucket.
   */
  readonly resolution: SchemaHistoryResolution;
  /** Spoken form, for an accessible name. */
  readonly words: string;
}

export const HISTORY_WINDOWS: readonly HistoryWindow[] = [
  { key: '15m', label: '15m', seconds: 15 * 60, resolution: 'raw', words: 'last 15 minutes' },
  { key: '1h', label: '1h', seconds: 60 * 60, resolution: '1m', words: 'last hour' },
  { key: '6h', label: '6h', seconds: 6 * 60 * 60, resolution: '1m', words: 'last 6 hours' },
  { key: '24h', label: '24h', seconds: 24 * 60 * 60, resolution: '15m', words: 'last 24 hours' },
];

/** The default: wide enough to show a session, fine enough to show a move. */
export const DEFAULT_WINDOW_KEY: HistoryWindowKey = '6h';

/** Total lookup — an unknown key falls back to the default rather than throwing. */
export function historyWindow(key: HistoryWindowKey): HistoryWindow {
  const found = HISTORY_WINDOWS.find((window) => window.key === key);
  if (found !== undefined) return found;
  // HISTORY_WINDOWS is a non-empty literal, but `noUncheckedIndexedAccess`
  // does not know that and a `!` here would be the one place this file lies.
  const fallback = HISTORY_WINDOWS[2];
  return (
    fallback ?? {
      key: '6h',
      label: '6h',
      seconds: 6 * 60 * 60,
      resolution: '1m',
      words: 'last 6 hours',
    }
  );
}

export interface HistoryBookOption {
  readonly slug: string;
  /** The catalogue name, or the slug when the catalogue has not loaded. */
  readonly name: string;
  /**
   * Null when the catalogue read has not landed. Absent evidence is rendered as
   * absence, never as a default — claiming a book is external or synthetic
   * because a fetch is in flight is exactly the kind of invented fact this
   * project does not ship.
   */
  readonly kind: SchemaBookKind | null;
  readonly isReference: boolean;
}

export interface HistoryControlsProps {
  readonly windowKey: HistoryWindowKey;
  readonly onWindowChange: (key: HistoryWindowKey) => void;
  /** The books that actually quote this market. Never the whole catalogue. */
  readonly books: readonly HistoryBookOption[];
  readonly bookSlug: string;
  readonly onBookChange: (slug: string) => void;
  /** The resolution in force — which a 422 may have changed. Read-only here. */
  readonly resolution: SchemaHistoryResolution;
  readonly disabled?: boolean;
}

function bookItemLabel(book: HistoryBookOption): string {
  const notes: string[] = [];
  if (book.isReference) notes.push('reference');
  // ADR 0003: a synthetic book's quote is a statement about a random number
  // generator, and every surface that renders one must be able to say so.
  if (book.kind === 'synthetic') notes.push('synthetic');
  return notes.length === 0
    ? book.name
    : `${book.name} — ${notes.join(', ')}`;
}

export function HistoryControls({
  windowKey,
  onWindowChange,
  books,
  bookSlug,
  onBookChange,
  resolution,
  disabled = false,
}: HistoryControlsProps) {
  return (
    <div className="flex flex-wrap items-center gap-4">
      <div
        role="group"
        aria-label="Chart window"
        className="flex items-center gap-1"
      >
        {HISTORY_WINDOWS.map((window) => {
          const active = window.key === windowKey;
          return (
            <Button
              key={window.key}
              type="button"
              size="sm"
              variant={active ? 'default' : 'ghost'}
              aria-pressed={active}
              disabled={disabled}
              onClick={() => {
                onWindowChange(window.key);
              }}
            >
              <span aria-hidden="true">{window.label}</span>
              <span className="sr-only">{window.words}</span>
            </Button>
          );
        })}
      </div>

      <Select
        value={bookSlug}
        onValueChange={onBookChange}
        disabled={disabled || books.length === 0}
      >
        <SelectTrigger className="h-9" aria-label="Book">
          <SelectValue placeholder="Book" />
        </SelectTrigger>
        <SelectContent>
          {books.map((book) => (
            <SelectItem key={book.slug} value={book.slug}>
              {bookItemLabel(book)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>

      <p className="t-mono text-ink-muted">
        {`resolution ${resolution}`}
      </p>
    </div>
  );
}
