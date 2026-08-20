'use client';

/**
 * The line-movement chart. Hand-rolled SVG; no charting library, ever
 * (ADR 0009 D5).
 *
 * # The y axis is implied probability, not the odds format
 *
 * A chart plotted in the display format changes SHAPE when the reader toggles
 * American / decimal / fractional, and American is worse than merely different:
 * it is discontinuous across evens, so a market drifting through 2.00 draws a
 * jump from +100 to -100 that no price ever made. Implied probability is
 * continuous, monotone in price, and linear in the quantity a line actually
 * moves in. The AXIS LABELS are rendered in the reader's format so the numbers
 * are the ones they recognise; only the geometry is probability.
 *
 * That probability is `1/decimal` WITH THE BOOK'S VIG STILL IN IT. It is not a
 * fair probability and the axis caption says so. The devigged number lives in
 * the fair value panel, in the mono register, beside the method that produced
 * it.
 *
 * # OHLC, and why one shape renders both cases
 *
 * The API returns open/high/low/close per bucket because "the mean of a line is
 * a price nobody traded at". The close is the line; the high–low is a faint
 * band behind it. On `resolution: raw` every field but `samples` holds the same
 * stored quote, so the band collapses onto the line and disappears — the same
 * code draws both, with no branch.
 *
 * # Sparse is correct
 *
 * Ingest suppresses no-op updates, so an hour of a quiet market can hold four
 * points. They are drawn as four dots joined by three straight segments. There
 * is no smoothing: a spline through sparse quotes draws prices that never
 * existed. An empty series says so in words rather than drawing a flat line at
 * zero, which would be a chart of a market nobody ever quoted at 1.00.
 */

import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import {
  ChartDataTable,
  ChartFrame,
  buildBandPath,
  buildLinePath,
  fractionOf,
  niceTicks,
  plotX,
  plotY,
} from '@/components/chart/chart-primitives';
import type { AxisTick, Point } from '@/components/chart/chart-primitives';
import {
  DEFAULT_WINDOW_KEY,
  HistoryControls,
  historyWindow,
} from '@/components/event/history-controls';
import type {
  HistoryBookOption,
  HistoryWindowKey,
} from '@/components/event/history-controls';
import { useDisplayTimeZone, useMountInstant } from '@/lib/client-value';
import { Button } from '@/components/ui';
import {
  developerDetail,
  isApiError,
  userFacingMessage,
} from '@/lib/api/errors';
import {
  booksQueryOptions,
  selectionHistoryQueryOptions,
} from '@/lib/api/queries';
import type {
  SchemaBook,
  SchemaHistoryResolution,
} from '@/lib/api/schema';
import {
  formatOdds,
  impliedProbability,
  isPriceableDecimal,
  renderPercent,
} from '@/lib/odds/format';
import { useOddsFormat } from '@/lib/store/preferences';
import { formatTimeOfDay, parseInstant } from '@/lib/time';

const RESOLUTIONS: readonly SchemaHistoryResolution[] = [
  'raw',
  '10s',
  '1m',
  '5m',
  '15m',
  '1h',
  '6h',
  '1d',
];

/** Above this many points, a dot per point is a solid bar, not a mark. */
const MAX_DOTTED_POINTS = 60;

export interface LineMovementChartProps {
  readonly selectionId: string;
  /** From the REST payload. A selection name is never derived from a role. */
  readonly selectionName: string;
  /** "Moneyline", "Total — Over 54.5". For the accessible name and caption. */
  readonly marketLabel: string;
  /** The books that quote this market, from the event payload. */
  readonly bookSlugs: readonly string[];
}

interface Plotted {
  readonly iso: string;
  readonly at: number;
  readonly x: number;
  readonly open: number;
  readonly high: number;
  readonly low: number;
  readonly close: number;
  readonly samples: number;
  /** 1/close — implied, WITH vig. */
  readonly pClose: number;
  /** 1/low — the highest implied probability in the bucket. */
  readonly pHigh: number;
  /** 1/high — the lowest. */
  readonly pLow: number;
}

/**
 * A 422 names the resolution that WOULD fit, in the `resolution` parameter's
 * reason ("too fine for this window; try 5m"). Reading it back out is what
 * turns a refusal into an offer.
 */
function suggestedResolution(error: unknown): SchemaHistoryResolution | null {
  if (!isApiError(error)) return null;
  const reason = error.reasonFor('resolution');
  if (reason === null) return null;
  for (const resolution of RESOLUTIONS) {
    if (reason.endsWith(`try ${resolution}`)) return resolution;
  }
  return null;
}

export function LineMovementChart({
  selectionId,
  selectionName,
  marketLabel,
  bookSlugs,
}: LineMovementChartProps) {
  const format = useOddsFormat();
  const timeZone = useDisplayTimeZone();

  const [windowKey, setWindowKey] = useState<HistoryWindowKey>(DEFAULT_WINDOW_KEY);
  const [chosenBook, setChosenBook] = useState<string | null>(null);
  const [override, setOverride] = useState<SchemaHistoryResolution | null>(null);
  /**
   * The wall-clock instant `from` is measured back from.
   *
   * Read through `useMountInstant` rather than during render, for two reasons:
   * a value read during render differs between the server pass and hydration,
   * and a value recomputed every render would change the query key every render
   * and refetch forever. It advances only when the reader asks — there is no
   * poll here, exactly as there is none anywhere else beside the socket.
   */
  const mountedAt = useMountInstant();
  const [refreshedAt, setRefreshedAt] = useState<number | null>(null);
  const anchorMs = refreshedAt ?? mountedAt;

  const catalogue = useQuery(booksQueryOptions());

  const books = useMemo<readonly HistoryBookOption[]>(() => {
    const known = new Map(
      (catalogue.data?.data ?? []).map(
        (book): [string, SchemaBook] => [book.slug, book],
      ),
    );
    return bookSlugs.map((slug) => {
      const book = known.get(slug);
      return {
        slug,
        name: book?.name ?? slug,
        kind: book?.kind ?? null,
        isReference: book?.is_reference ?? false,
      };
    });
  }, [bookSlugs, catalogue.data]);

  // The reference book by default: every EV figure in this system is relative
  // to it, so it is the series a reader is most often after. Derived rather
  // than stored, so the catalogue landing after first paint upgrades the
  // default without an effect and without overriding a real choice.
  const bookSlug =
    chosenBook ??
    books.find((book) => book.isReference)?.slug ??
    books[0]?.slug ??
    '';

  const windowSpec = historyWindow(windowKey);
  const resolution = override ?? windowSpec.resolution;
  const from =
    anchorMs === null
      ? ''
      : new Date(anchorMs - windowSpec.seconds * 1000).toISOString();

  // `selectionHistoryQueryOptions` already disables itself while `book` or
  // `from` is empty, which is exactly the "not ready yet" condition here.
  const history = useQuery(
    selectionHistoryQueryOptions(selectionId, {
      book: bookSlug,
      from,
      resolution,
    }),
  );

  const bookName =
    books.find((book) => book.slug === bookSlug)?.name ?? bookSlug;

  const plotted = useMemo<readonly Plotted[]>(() => {
    const series = history.data;
    if (series === undefined) return [];
    const fromMs = parseInstant(series.from);
    const toMs = parseInstant(series.to);
    if (fromMs === null || toMs === null || toMs <= fromMs) return [];

    const out: Plotted[] = [];
    for (const point of series.points) {
      const at = parseInstant(point.at);
      if (at === null) continue;
      if (
        !isPriceableDecimal(point.close) ||
        !isPriceableDecimal(point.high) ||
        !isPriceableDecimal(point.low) ||
        !isPriceableDecimal(point.open)
      ) {
        continue;
      }
      const pClose = impliedProbability(point.close);
      const pHigh = impliedProbability(point.low);
      const pLow = impliedProbability(point.high);
      if (pClose === null || pHigh === null || pLow === null) continue;

      out.push({
        iso: point.at,
        at,
        x: plotX(fractionOf(at, fromMs, toMs)),
        open: point.open,
        high: point.high,
        low: point.low,
        close: point.close,
        samples: point.samples,
        pClose,
        pHigh,
        pLow,
      });
    }
    return out;
  }, [history.data]);

  const domain = useMemo(() => {
    if (plotted.length === 0) return { min: 0, max: 1 };
    let min = Number.POSITIVE_INFINITY;
    let max = Number.NEGATIVE_INFINITY;
    for (const point of plotted) {
      min = Math.min(min, point.pLow);
      max = Math.max(max, point.pHigh);
    }
    if (!Number.isFinite(min) || !Number.isFinite(max)) {
      return { min: 0, max: 1 };
    }
    const span = max - min;
    // A market that has not moved is a real and frequent state. Give it a
    // window rather than a zero-height domain, so a flat line sits in the
    // middle of the plot instead of on its floor.
    const pad = span === 0 ? Math.max(0.01, min * 0.05) : span * 0.12;
    return { min: Math.max(0, min - pad), max: Math.min(1, max + pad) };
  }, [plotted]);

  const yTicks = useMemo<readonly AxisTick[]>(
    () =>
      niceTicks(domain.min, domain.max, 4)
        .filter((value) => value > 0 && value < 1)
        .map((probability) => ({
          fraction: fractionOf(probability, domain.min, domain.max),
          label: formatOdds(1 / probability, format),
          sub: renderPercent(probability, 1),
        })),
    [domain, format],
  );

  const closeLine = useMemo<readonly Point[]>(
    () =>
      plotted.map((point) => ({
        x: point.x,
        y: plotY(fractionOf(point.pClose, domain.min, domain.max)),
      })),
    [plotted, domain],
  );

  const bandUpper = useMemo<readonly Point[]>(
    () =>
      plotted.map((point) => ({
        x: point.x,
        y: plotY(fractionOf(point.pHigh, domain.min, domain.max)),
      })),
    [plotted, domain],
  );

  const bandLower = useMemo<readonly Point[]>(
    () =>
      plotted.map((point) => ({
        x: point.x,
        y: plotY(fractionOf(point.pLow, domain.min, domain.max)),
      })),
    [plotted, domain],
  );

  const dots = useMemo<readonly Point[]>(() => {
    if (closeLine.length <= MAX_DOTTED_POINTS) return closeLine;
    const out: Point[] = [];
    const head = closeLine[0];
    const tail = closeLine[closeLine.length - 1];
    if (head !== undefined) out.push(head);
    if (tail !== undefined) out.push(tail);
    return out;
  }, [closeLine]);

  const first = plotted[0];
  const last = plotted[plotted.length - 1];
  const previous = plotted.length >= 2 ? plotted[plotted.length - 2] : undefined;

  // Direction of the LAST move only. Colouring every segment of a 360 point
  // series produces a rainbow that competes with the delta rails elsewhere on
  // the page; colouring the newest one answers "which way did it just go"
  // without spending the whole palette. Direction is also stated in words and
  // by an arrow above the chart, so colour is never the only carrier.
  const lastDirection: 'in' | 'out' | null =
    last === undefined || previous === undefined
      ? null
      : last.pClose > previous.pClose
        ? 'in'
        : last.pClose < previous.pClose
          ? 'out'
          : null;

  const lastSegment = useMemo<readonly [Point, Point] | null>(() => {
    if (closeLine.length < 2) return null;
    const a = closeLine[closeLine.length - 2];
    const b = closeLine[closeLine.length - 1];
    if (a === undefined || b === undefined) return null;
    return [a, b];
  }, [closeLine]);

  const netDirection: 'in' | 'out' | null =
    first === undefined || last === undefined
      ? null
      : last.pClose > first.pClose
        ? 'in'
        : last.pClose < first.pClose
          ? 'out'
          : null;

  const windowFromIso = history.data?.from ?? from;
  const windowToIso = history.data?.to ?? '';
  const midIso = useMemo(() => {
    const a = parseInstant(windowFromIso);
    const b = parseInstant(windowToIso);
    if (a === null || b === null) return '';
    return new Date((a + b) / 2).toISOString();
  }, [windowFromIso, windowToIso]);

  const ariaLabel =
    first === undefined || last === undefined
      ? `Line movement for ${selectionName} on ${marketLabel} at ${bookName}, ${windowSpec.words}: no quotes.`
      : `Line movement for ${selectionName} on ${marketLabel} at ${bookName}, ${windowSpec.words}. ` +
        `Opened at ${formatOdds(first.open, format)}, implied probability ${renderPercent(first.pClose, 1)}. ` +
        `Closed at ${formatOdds(last.close, format)}, implied probability ${renderPercent(last.pClose, 1)}. ` +
        `${String(plotted.length)} points at ${resolution} resolution. ` +
        (netDirection === null
          ? 'Implied probability is unchanged over the window.'
          : netDirection === 'in'
            ? 'Implied probability rose over the window; the price shortened.'
            : 'Implied probability fell over the window; the price lengthened.');

  const error = history.error;
  const suggestion = suggestedResolution(error);
  const windowReason = isApiError(error) ? error.reasonFor('from') : null;

  const netToneClass =
    netDirection === 'in'
      ? 'text-delta-in'
      : netDirection === 'out'
        ? 'text-delta-out'
        : 'text-ink-muted';

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <HistoryControls
          windowKey={windowKey}
          onWindowChange={(key) => {
            setWindowKey(key);
            // A resolution the API suggested for the OLD window is not a
            // suggestion for the new one.
            setOverride(null);
            setRefreshedAt(Date.now());
          }}
          books={books}
          bookSlug={bookSlug}
          onBookChange={(slug) => {
            setChosenBook(slug);
          }}
          resolution={resolution}
          disabled={books.length === 0}
        />
        <Button
          type="button"
          size="sm"
          variant="ghost"
          onClick={() => {
            setRefreshedAt(Date.now());
          }}
        >
          Advance window
        </Button>
      </div>

      {first !== undefined && last !== undefined ? (
        <p className="t-ui text-ink-2">
          <span className={netToneClass}>
            <span aria-hidden="true">
              {netDirection === 'in' ? '↑' : netDirection === 'out' ? '↓' : '→'}
            </span>{' '}
            {formatOdds(first.open, format)} → {formatOdds(last.close, format)}
          </span>
          <span className="text-ink-muted">
            {' · implied probability '}
            {renderPercent(first.pClose, 1)} → {renderPercent(last.pClose, 1)}
            {netDirection === 'in'
              ? ' (rose — price shortened)'
              : netDirection === 'out'
                ? ' (fell — price lengthened)'
                : ' (unchanged)'}
          </span>
        </p>
      ) : null}

      <div aria-busy={history.isFetching}>
        {bookSlug === '' ? (
          <p className="t-body text-ink-2">
            No book has quoted this selection inside the freshness window, so
            there is no series to draw.
          </p>
        ) : history.isError ? (
          <div className="rounded-card border border-rule bg-ground-1 p-4">
            {isApiError(error) && error.status === 422 ? (
              <div className="flex flex-col gap-3">
                <p className="t-body text-ink">
                  {`This window is too wide to return at ${resolution} resolution. `}
                  The API refuses the request rather than thinning the series,
                  because a truncated series is a chart that lies about when the
                  line moved.
                </p>
                {suggestion === null ? (
                  <p className="t-body text-ink-2">
                    {windowReason ?? 'Choose a narrower window.'}
                  </p>
                ) : (
                  <div className="flex flex-wrap items-center gap-3">
                    <p className="t-body text-ink-2">
                      {`${resolution} does not fit this window; ${suggestion} does.`}
                    </p>
                    <Button
                      type="button"
                      size="sm"
                      onClick={() => {
                        setOverride(suggestion);
                      }}
                    >
                      {`Redraw at ${suggestion}`}
                    </Button>
                  </div>
                )}
              </div>
            ) : (
              <div className="flex flex-col gap-3">
                <p className="t-body text-ink">{userFacingMessage(error)}</p>
                {developerDetail(error) === null ? null : (
                  <details>
                    <summary className="t-ui text-ink-muted">Details</summary>
                    <p className="t-mono text-ink-muted">
                      {developerDetail(error)}
                    </p>
                  </details>
                )}
              </div>
            )}
          </div>
        ) : history.isPending ? (
          <p className="t-body text-ink-muted">Loading line movement…</p>
        ) : plotted.length === 0 ? (
          <div className="flex flex-col gap-2 rounded-card border border-rule bg-ground-1 p-4">
            <p className="t-body text-ink">
              {`No quotes for ${selectionName} at ${bookName} in the ${windowSpec.words}.`}
            </p>
            <p className="t-body text-ink-2">
              Ingest suppresses updates that did not change the price, so a
              quiet market genuinely has no points to draw. This is an answer,
              not a gap.
            </p>
          </div>
        ) : (
          <>
            <ChartFrame
              yTicks={yTicks}
              xLabels={[
                formatTimeOfDay(windowFromIso, timeZone),
                formatTimeOfDay(midIso, timeZone),
                formatTimeOfDay(windowToIso, timeZone),
              ]}
              ariaLabel={ariaLabel}
            >
              {/* High–low envelope. Degenerate, and therefore invisible, on a
                  raw series where every field holds the same quote. */}
              <path
                d={buildBandPath(bandUpper, bandLower)}
                fill="var(--color-ink-2)"
                fillOpacity={0.12}
                stroke="none"
              />
              <path
                d={buildLinePath(closeLine)}
                fill="none"
                stroke="var(--color-ink-2)"
                strokeWidth={2.5}
                strokeLinejoin="round"
                strokeLinecap="round"
              />
              {lastSegment === null || lastDirection === null ? null : (
                <line
                  x1={lastSegment[0].x}
                  y1={lastSegment[0].y}
                  x2={lastSegment[1].x}
                  y2={lastSegment[1].y}
                  stroke={
                    lastDirection === 'in'
                      ? 'var(--color-delta-in)'
                      : 'var(--color-delta-out)'
                  }
                  strokeWidth={3}
                  strokeLinecap="round"
                />
              )}
              {dots.map((point, index) => (
                <circle
                  key={`dot-${String(index)}-${point.x.toFixed(2)}`}
                  cx={point.x}
                  cy={point.y}
                  r={3}
                  fill="var(--color-ink-2)"
                />
              ))}
            </ChartFrame>

            <p className="t-mono pt-2 text-ink-muted">
              {`y: implied probability — 1/decimal, with ${bookName}'s vig still in it, not a fair probability · ${String(plotted.length)} points · ${resolution}`}
            </p>

            <ChartDataTable
              caption={`Line movement for ${selectionName} on ${marketLabel} at ${bookName}, ${windowSpec.words}, ${resolution} resolution.`}
              columns={[
                'Time',
                'Open',
                'High',
                'Low',
                'Close',
                'Implied probability at close',
                'Samples',
              ]}
              rows={plotted.map((point) => [
                formatTimeOfDay(point.iso, timeZone),
                formatOdds(point.open, format),
                formatOdds(point.high, format),
                formatOdds(point.low, format),
                formatOdds(point.close, format),
                renderPercent(point.pClose, 1),
                String(point.samples),
              ])}
            />
          </>
        )}
      </div>
    </div>
  );
}
