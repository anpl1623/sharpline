'use client';

/**
 * The pieces a hand-rolled SVG chart needs, and nothing more.
 *
 * There is no charting library in this project and one must not be added
 * (ADR 0009 D5). A chart library brings its own type scale, its own palette,
 * its own easing and its own idea of what a tooltip looks like — all four of
 * which this design system has already decided, differently. The parts a line
 * chart actually needs are a scale, a tick generator and a path builder, and
 * they are ninety lines.
 *
 * # Why the axis labels are HTML and only the geometry is SVG
 *
 * The chart is responsive through its `viewBox`, with no fixed pixel width. A
 * `viewBox` scales EVERYTHING uniformly, text included: a 12px label in a
 * 960-unit box rendered into a 340px column is a 4px label. So the SVG holds
 * the marks — band, line, dots, gridlines — and every character on the chart is
 * HTML positioned around it, where `.t-mono` and `.t-price-sm` mean what they
 * say at every width.
 *
 * The two stay aligned because the plot `viewBox` is EXACTLY the plot area:
 * there is no internal padding, so a value at fraction `f` of the domain is at
 * fraction `f` of the rendered box, and an HTML label positioned at that
 * percentage lands on its gridline at any size.
 */

import type { ReactNode } from 'react';

/**
 * Plot geometry in user units. The aspect ratio (roughly 3.7:1) is what a line
 * chart wants — wide, short, movement read horizontally — and it keeps the
 * chart from eating the page at 1200px.
 */
export const PLOT_WIDTH = 960;
export const PLOT_HEIGHT = 260;

export interface Point {
  readonly x: number;
  readonly y: number;
}

/** A labelled position on an axis, as a 0–1 fraction of the domain. */
export interface AxisTick {
  readonly fraction: number;
  readonly label: string;
  /** A second line, in the mono register. Optional. */
  readonly sub?: string;
}

// -----------------------------------------------------------------------------
// Scales
// -----------------------------------------------------------------------------

/**
 * Where `value` sits between `min` and `max`, as 0–1.
 *
 * Returns 0.5 for a degenerate domain rather than dividing by zero — a series
 * whose every point is identical is a real and common case here, because ingest
 * suppresses no-op updates and a quiet market genuinely does not move.
 */
export function fractionOf(value: number, min: number, max: number): number {
  const span = max - min;
  if (!Number.isFinite(span) || span === 0) return 0.5;
  return (value - min) / span;
}

/** Plot x for a 0–1 fraction. */
export function plotX(fraction: number): number {
  return fraction * PLOT_WIDTH;
}

/** Plot y for a 0–1 fraction. y is inverted: fraction 1 is the TOP. */
export function plotY(fraction: number): number {
  return (1 - fraction) * PLOT_HEIGHT;
}

/**
 * Round-number ticks covering `[min, max]`, at roughly `target` of them.
 *
 * The 1/2/5 progression is the standard one: it is the set of steps that stay
 * mentally divisible at every magnitude, which is the whole job of an axis.
 */
export function niceTicks(
  min: number,
  max: number,
  target = 4,
): readonly number[] {
  if (!Number.isFinite(min) || !Number.isFinite(max) || target < 1) return [];
  if (max <= min) return [min];

  const raw = (max - min) / target;
  const magnitude = 10 ** Math.floor(Math.log10(raw));
  const normalised = raw / magnitude;
  const step =
    magnitude *
    (normalised <= 1 ? 1 : normalised <= 2 ? 2 : normalised <= 5 ? 5 : 10);

  const ticks: number[] = [];
  const first = Math.ceil(min / step) * step;
  for (let value = first; value <= max + step / 1000; value += step) {
    // Kill the float dust a repeated addition accumulates, so a tick reads
    // "0.55" and not "0.5500000000000001".
    ticks.push(Number(value.toFixed(10)));
  }
  return ticks;
}

// -----------------------------------------------------------------------------
// Paths
// -----------------------------------------------------------------------------

/**
 * A polyline through the points. Straight segments only — no smoothing.
 *
 * A spline through sparse quotes draws prices that never existed between two
 * that did, which on a line-movement chart is not a stylistic choice, it is a
 * fabricated tick.
 */
export function buildLinePath(points: readonly Point[]): string {
  if (points.length === 0) return '';
  const parts: string[] = [];
  for (const [index, point] of points.entries()) {
    parts.push(
      `${index === 0 ? 'M' : 'L'}${point.x.toFixed(2)} ${point.y.toFixed(2)}`,
    );
  }
  return parts.join(' ');
}

/**
 * A closed band between an upper and a lower edge sharing x positions. Used for
 * the high–low envelope; degenerate (and therefore invisible) when they are the
 * same series, which is exactly what `resolution: raw` produces.
 */
export function buildBandPath(
  upper: readonly Point[],
  lower: readonly Point[],
): string {
  if (upper.length === 0 || upper.length !== lower.length) return '';
  const forward = buildLinePath(upper);
  const back: string[] = [];
  for (let index = lower.length - 1; index >= 0; index -= 1) {
    const point = lower[index];
    if (point === undefined) continue;
    back.push(`L${point.x.toFixed(2)} ${point.y.toFixed(2)}`);
  }
  return `${forward} ${back.join(' ')} Z`;
}

// -----------------------------------------------------------------------------
// The frame
// -----------------------------------------------------------------------------

export interface ChartFrameProps {
  /** Ticks up the left edge, bottom (0) to top (1). */
  readonly yTicks: readonly AxisTick[];
  /** Three labels along the bottom: start, middle, end of the window. */
  readonly xLabels: readonly [string, string, string];
  /** Names the whole image for a screen reader. */
  readonly ariaLabel: string;
  /** SVG content, in plot user units. */
  readonly children: ReactNode;
}

/**
 * The axes, the gridlines and the plot box.
 *
 * `role="img"` with a full `aria-label` on the SVG makes the marks a single
 * described object rather than a pile of unlabelled shapes; the data itself is
 * reachable through `ChartDataTable`, which is the part that actually has to be
 * readable without sight.
 */
export function ChartFrame({
  yTicks,
  xLabels,
  ariaLabel,
  children,
}: ChartFrameProps) {
  return (
    <div className="grid grid-cols-[3.5rem_1fr] gap-x-2">
      <div className="relative">
        {yTicks.map((tick) => (
          <div
            key={`${tick.label}-${String(tick.fraction)}`}
            className="absolute right-0 -translate-y-1/2 text-right"
            style={{ top: `${String((1 - tick.fraction) * 100)}%` }}
          >
            <span className="t-price-sm block text-ink-muted">
              {tick.label}
            </span>
            {tick.sub === undefined ? null : (
              <span className="t-mono block text-ink-muted">{tick.sub}</span>
            )}
          </div>
        ))}
      </div>

      <svg
        viewBox={`0 0 ${String(PLOT_WIDTH)} ${String(PLOT_HEIGHT)}`}
        className="block h-auto w-full"
        role="img"
        aria-label={ariaLabel}
        preserveAspectRatio="xMidYMid meet"
      >
        {/* Gridlines. `rule` is the hairline token; at this scale one user unit
            renders under a pixel on a wide viewport, so 1.5 keeps it visible
            without becoming a line the eye reads as data. */}
        {yTicks.map((tick) => (
          <line
            key={`grid-${String(tick.fraction)}`}
            x1={0}
            x2={PLOT_WIDTH}
            y1={plotY(tick.fraction)}
            y2={plotY(tick.fraction)}
            stroke="var(--color-rule)"
            strokeWidth={1.5}
          />
        ))}
        {[0.25, 0.5, 0.75].map((fraction) => (
          <line
            key={`vgrid-${String(fraction)}`}
            x1={plotX(fraction)}
            x2={plotX(fraction)}
            y1={0}
            y2={PLOT_HEIGHT}
            stroke="var(--color-rule)"
            strokeWidth={1.5}
          />
        ))}
        {children}
      </svg>

      <div />
      <div className="flex justify-between pt-2">
        {xLabels.map((label, index) => (
          <span
            key={`${label}-${String(index)}`}
            className="t-mono text-ink-muted"
          >
            {label}
          </span>
        ))}
      </div>
    </div>
  );
}

// -----------------------------------------------------------------------------
// The accessible data
// -----------------------------------------------------------------------------

export interface ChartDataTableProps {
  readonly caption: string;
  readonly columns: readonly string[];
  readonly rows: readonly (readonly string[])[];
}

/**
 * The chart's points as a real table, visually hidden.
 *
 * This is not a fallback. A line chart is a picture of numbers and the numbers
 * have to be reachable without sight; an `aria-label` summarising the endpoints
 * says what the shape is, and this says what the shape is made of.
 */
export function ChartDataTable({
  caption,
  columns,
  rows,
}: ChartDataTableProps) {
  return (
    <table className="sr-only">
      <caption>{caption}</caption>
      <thead>
        <tr>
          {columns.map((column) => (
            <th key={column} scope="col">
              {column}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {rows.map((row, rowIndex) => (
          <tr key={`row-${String(rowIndex)}`}>
            {row.map((cell, cellIndex) => (
              <td key={`cell-${String(rowIndex)}-${String(cellIndex)}`}>
                {cell}
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  );
}
