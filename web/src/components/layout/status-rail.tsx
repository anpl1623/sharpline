'use client';

/**
 * THE ENGINEERING LAYER — a permanent 24px mono strip, not a debug drawer.
 *
 * DESIGN.md's Decisions Log records this as a decision with a stated price:
 * "Engineering layer as a permanent 24px mono status rail, not a debug drawer.
 * Costs 24px of permanent vertical space." It is paid because the product serves
 * two audiences from one surface — a bettor and an engineer — and a drawer that
 * has to be opened is a second product. Every mono glyph on screen means *the
 * machine is talking*; keeping the register intact is what lets a reader tell
 * the two layers apart in peripheral vision, with no mode switch.
 *
 * Below 768px the rail collapses to the connection pip (`status-pip.tsx`), which
 * re-renders this exact readout, in this exact order, inside a sheet. Nothing is
 * lost on mobile, only folded — so `StatusReadout` is exported and both surfaces
 * consume it.
 *
 * # It degrades honestly
 *
 * When the socket is down the rail says so in words ("Disconnected"), keeps
 * showing the last sequence number it actually saw, and renders an em dash for
 * every value it does not have. It never hides, never blanks, and never invents
 * a plausible number to fill a slot. A rail that goes quiet when the system
 * breaks is worse than no rail.
 *
 * # Where each number comes from
 *
 * Everything except staleness is read straight off the socket, so the rail is
 * correct on every page including ones with no board on them:
 *
 *   link / conn / seq / chan   `useStreamStatus()` — the client's own state
 *   mkts                       `useSlateStats()` — markets held in the slate
 *   rate / last                counted here, off the raw frame stream
 *   src                        `ComputedMarket.provider` from the newest frame
 *   stale                      see below
 *
 * ODDS STALENESS is the headline SLO (CLAUDE.md §9), so it gets the most care.
 * It is ALWAYS `observed_at` (the provider's own instant) subtracted from a
 * SERVER anchor — `BoardPage.as_of` on REST, the frame's `ts` on the stream —
 * and never from `Date.now()`. A browser with a skewed clock must not be able to
 * make a fresh board look stale, or a stale one look fresh.
 *
 * ONE source: a rolling median over the markets carried by recent stream
 * frames, each measured against its own frame's `ts`. The sample is biased
 * toward markets that recently ticked, which is the correct bias — a market
 * nobody is quoting is not what "how fresh is a typical price" is asking about —
 * and it is available on every page rather than only on the board.
 *
 * A second source was considered and rejected: a mounted board publishing the
 * median across its visible rows. The only staleness a board component can
 * compute is REST `observed_at` against an `as_of` frozen at page assembly, so
 * it would sit unchanged while ingestion stalled, and being the page's own
 * number it would have outranked this one — masking the single failure this
 * field exists to surface.
 *
 * Before the first frame, the field is an em dash, which is the honest answer.
 */

import { useEffect, useRef, useState } from 'react';

import { DISCLAIMER_SHORT } from '@/components/layout/disclaimer';
import { formatCompactDuration, stalenessSeconds } from '@/lib/time';
import { cn } from '@/lib/utils';
import type { ComputedMarket } from '@/lib/ws/protocol';
import {
  useSlateStats,
  useStreamClient,
  useStreamDescription,
  useStreamStatus,
} from '@/lib/ws/provider';

/** Rendered wherever the rail has no value. Never a zero, never a guess. */
const UNKNOWN = '—';

/** How often the rail recomputes derived numbers. One second. */
const TICK_MS = 1_000;

/** How many staleness samples the stream-derived median keeps. */
const STALENESS_WINDOW = 256;

/** The rolling window the frame rate is measured over. See useRailTelemetry. */
const RATE_WINDOW_MS = 10_000;

/** Characters of `connection_id` shown. Enough to correlate with a server log. */
const CONNECTION_ID_CHARS = 8;

function median(samples: readonly number[]): number | null {
  if (samples.length === 0) return null;
  const sorted = [...samples].sort((a, b) => a - b);
  const middle = Math.floor(sorted.length / 2);
  if (sorted.length % 2 === 1) return sorted[middle] ?? null;
  const low = sorted[middle - 1];
  const high = sorted[middle];
  if (low === undefined || high === undefined) return null;
  return (low + high) / 2;
}

// -----------------------------------------------------------------------------
// Telemetry read off the raw frame stream
// -----------------------------------------------------------------------------

interface RailTelemetry {
  /** Epoch ms, sampled on the tick. Null until mounted — SSR has no clock. */
  readonly now: number | null;
  readonly framesPerSecond: number | null;
  readonly provider: string | null;
  readonly streamStalenessSeconds: number | null;
}

const IDLE_TELEMETRY: RailTelemetry = {
  now: null,
  framesPerSecond: null,
  provider: null,
  streamStalenessSeconds: null,
};

function useRailTelemetry(): RailTelemetry {
  const client = useStreamClient();
  const [telemetry, setTelemetry] = useState<RailTelemetry>(IDLE_TELEMETRY);

  // Frame ARRIVAL INSTANTS over a rolling window, not a counter reset every tick.
  //
  // This feed is bursty by construction: ingest suppresses no-op polls, so a
  // league's markets arrive as a handful of deltas together and then nothing for
  // several seconds. Counted over a 1s window and reset, the rate reads "0/s" on
  // almost every tick with an occasional spike — which on a visibly moving board
  // reads as "the stream is dead" and is the opposite of what the field is for.
  // A 10s rolling window reports the rate a human actually perceives.
  const frameTimesRef = useRef<number[]>([]);
  const providerRef = useRef<string | null>(null);
  const samplesRef = useRef<number[]>([]);

  useEffect(() => {
    if (client === null) return;

    const record = (market: ComputedMarket | undefined, frameTs: string): void => {
      if (market === undefined) return;
      providerRef.current = market.provider;
      // Anchored to the FRAME's own `ts` — the gateway's clock at send time —
      // and never to the browser's, so every sample is a server-measured age.
      // `ts` is the MINUEND here. The protocol is explicit that it is never the
      // subtrahend: that is always `observed_at`, the provider's own instant.
      const seconds = stalenessSeconds(market.observed_at, frameTs);
      if (seconds === null) return;
      const samples = samplesRef.current;
      samples.push(seconds);
      if (samples.length > STALENESS_WINDOW) {
        samples.splice(0, samples.length - STALENESS_WINDOW);
      }
    };

    const offFrame = client.on('frame', () => {
      frameTimesRef.current.push(Date.now());
    });
    const offSnapshot = client.on('snapshot', (frame) => {
      for (const market of frame.markets) record(market, frame.ts);
    });
    const offDelta = client.on('delta', (frame) => {
      record(frame.market, frame.ts);
    });
    // A new socket means a cold slate: the old samples describe a stream that
    // no longer exists, and the frame counter's window is meaningless.
    const offReset = client.on('reset', () => {
      frameTimesRef.current = [];
      samplesRef.current = [];
    });

    return () => {
      offFrame();
      offSnapshot();
      offDelta();
      offReset();
    };
  }, [client]);

  useEffect(() => {
    const mountedAt = Date.now();

    const id = setInterval(() => {
      const now = Date.now();

      // Drop everything that fell out of the window, then divide by however much
      // of the window has actually elapsed — so the first ten seconds after a
      // connection report a real rate rather than a tenth of one.
      const cutoff = now - RATE_WINDOW_MS;
      const times = frameTimesRef.current.filter((at) => at >= cutoff);
      frameTimesRef.current = times;

      const observedMs = Math.min(RATE_WINDOW_MS, Math.max(1, now - mountedAt));
      const rate = times.length / (observedMs / 1000);

      setTelemetry({
        now,
        framesPerSecond: rate,
        provider: providerRef.current,
        streamStalenessSeconds: median(samplesRef.current),
      });
    }, TICK_MS);

    return () => {
      clearInterval(id);
    };
  }, []);

  return telemetry;
}

// -----------------------------------------------------------------------------
// Fields
// -----------------------------------------------------------------------------

type FieldTone = 'default' | 'info' | 'loss';

interface Field {
  readonly label: string;
  readonly value: string;
  readonly tone?: FieldTone;
  /** Spelled-out form for a screen reader, when the compact form is cryptic. */
  readonly title?: string;
}

function abbreviateId(id: string | null): string {
  if (id === null || id === '') return UNKNOWN;
  return id.length <= CONNECTION_ID_CHARS ? id : id.slice(0, CONNECTION_ID_CHARS);
}

function formatRate(rate: number | null): string {
  if (rate === null || !Number.isFinite(rate)) return UNKNOWN;
  // A genuine zero over the whole window means the stream has gone quiet, which
  // is worth saying plainly rather than as "0.00/s".
  if (rate === 0) return '0/s';
  if (rate < 1) return `${rate.toFixed(2)}/s`;
  return `${rate.toFixed(1)}/s`;
}

function formatStalenessValue(seconds: number | null): string {
  if (seconds === null) return UNKNOWN;
  // Sub-second staleness is the normal case on a healthy pipeline and
  // `formatCompactDuration` truncates it to "0s", which reads as broken rather
  // than as fast. Below ten seconds the rail shows one decimal.
  if (seconds < 10) return `${seconds.toFixed(1)}s`;
  return formatCompactDuration(seconds);
}

const TONE_CLASS: Record<FieldTone, string> = {
  default: 'text-ink-2',
  info: 'text-info',
  loss: 'text-loss',
};

// -----------------------------------------------------------------------------
// The readout
// -----------------------------------------------------------------------------

export type StatusReadoutLayout = 'rail' | 'stacked';

export interface StatusReadoutProps {
  /**
   * `rail` is the desktop 24px strip: one horizontal line, scrolling rather than
   * wrapping. `stacked` is the mobile sheet: the same fields, the same order,
   * as a two-column list with room to breathe.
   */
  readonly layout?: StatusReadoutLayout;
  readonly className?: string | undefined;
}

export function StatusReadout({
  layout = 'rail',
  className,
}: StatusReadoutProps) {
  const state = useStreamStatus();
  const description = useStreamDescription();
  const stats = useSlateStats();
  const telemetry = useRailTelemetry();

  const stacked = layout === 'stacked';

  const linkTone: FieldTone =
    description.tone === 'disconnected'
      ? 'loss'
      : description.tone === 'resyncing' || description.tone === 'reconnecting'
        ? 'info'
        : 'default';

  const lastFrameAge =
    telemetry.now === null || state.lastFrameAt === null
      ? UNKNOWN
      : formatCompactDuration((telemetry.now - state.lastFrameAt) / 1000);

  // The headline SLO number, and it comes from the STREAM and only the stream.
  //
  // A page-published median was considered and removed: the only staleness a
  // board component can compute is REST `observed_at` against a `as_of` frozen
  // at page assembly, so it would sit unchanged while ingestion stalled — and
  // being the page's own number it would outrank this one, masking the single
  // failure the staleness SLO exists to surface. A frozen number that wins is
  // worse than no number, so there is one source here rather than two.
  const staleness = telemetry.streamStalenessSeconds;
  const stalenessSample =
    telemetry.streamStalenessSeconds === null ? 'no samples yet' : 'recent stream frames';

  const fields: Field[] = [
    { label: 'link', value: description.label, tone: linkTone },
    {
      label: 'conn',
      value: abbreviateId(state.connectionId),
      title: 'Connection id',
    },
    {
      label: 'seq',
      value: state.lastSeq > 0 ? String(state.lastSeq) : UNKNOWN,
      title: 'Highest sequence number seen on this connection',
    },
    {
      label: 'chan',
      value: String(state.channelCount),
      title: 'Channels held',
    },
    { label: 'mkts', value: String(stats.markets), title: 'Markets in the slate' },
    { label: 'rate', value: formatRate(telemetry.framesPerSecond), title: 'Frames per second' },
    { label: 'last', value: lastFrameAge, title: 'Age of the newest frame' },
    {
      label: 'stale',
      value: formatStalenessValue(staleness),
      title: `Median odds staleness (${stalenessSample})`,
    },
    {
      label: 'src',
      value: telemetry.provider ?? UNKNOWN,
      title: 'Provider that produced the newest quote',
    },
  ];

  // Diagnostics are quiet until they are not. On the rail they appear only when
  // non-zero, so a healthy connection reads as a short line; in the sheet they
  // are always present, because a reader who opened it is asking.
  const diagnostics: Field[] = [];
  if (stacked || state.gaps > 0) {
    diagnostics.push({
      label: 'gaps',
      value: String(state.gaps),
      title: 'Sequence gaps observed',
      tone: state.gaps > 0 ? 'info' : 'default',
    });
  }
  if (stacked || state.resyncs > 0) {
    diagnostics.push({
      label: 'resync',
      value: String(state.resyncs),
      title: 'Resyncs requested or received',
      tone: state.resyncs > 0 ? 'info' : 'default',
    });
  }
  if (stacked || state.reconnects > 0) {
    diagnostics.push({
      label: 'reconn',
      value: String(state.reconnects),
      title: 'Reconnections',
    });
  }
  if (state.lastError !== null) {
    diagnostics.push({
      label: 'err',
      value: state.lastError.code,
      tone: 'loss',
      title: state.lastError.message,
    });
  }
  if (state.unauthorized) {
    diagnostics.push({
      label: 'auth',
      value: 'rejected',
      tone: 'loss',
      title: 'A credential was rejected; reconnection has stopped',
    });
  }

  const all = [...fields, ...diagnostics];

  if (stacked) {
    return (
      <div className={cn('flex flex-col gap-4', className)}>
        <dl className="grid grid-cols-[5.5rem_minmax(0,1fr)] gap-x-4 gap-y-2">
          {all.map((field) => (
            <FieldPair key={field.label} field={field} stacked />
          ))}
        </dl>
        <p className="t-mono text-ink-muted">{DISCLAIMER_SHORT}</p>
      </div>
    );
  }

  return (
    <div
      className={cn(
        'flex h-full w-full items-center gap-3 overflow-x-auto px-3',
        '[scrollbar-width:none] [&::-webkit-scrollbar]:hidden',
        className,
      )}
    >
      <dl className="flex shrink-0 items-center gap-3">
        {all.map((field) => (
          <FieldPair key={field.label} field={field} />
        ))}
      </dl>
      <p className="ml-auto shrink-0 pl-3 t-mono whitespace-nowrap text-ink-muted">
        {DISCLAIMER_SHORT}
      </p>
    </div>
  );
}

function FieldPair({
  field,
  stacked = false,
}: {
  readonly field: Field;
  readonly stacked?: boolean;
}) {
  const value = (
    <dd
      className={cn(
        't-mono whitespace-nowrap',
        TONE_CLASS[field.tone ?? 'default'],
      )}
    >
      {field.value}
    </dd>
  );

  const label = (
    <dt className="t-mono font-medium whitespace-nowrap text-ink-muted">
      {field.label}
    </dt>
  );

  if (stacked) {
    return (
      <>
        {label}
        {value}
      </>
    );
  }

  return (
    <div className="flex shrink-0 items-center gap-1.5" title={field.title}>
      {label}
      {value}
    </div>
  );
}

// -----------------------------------------------------------------------------
// The rail
// -----------------------------------------------------------------------------

export interface StatusRailProps {
  readonly className?: string | undefined;
}

/**
 * The persistent strip. 24px, mono, `ground-1` over a `rule` hairline, stuck to
 * the bottom of the viewport.
 *
 * Hidden below 768px, where DESIGN.md folds it into the connection pip: the rail
 * is 6% of an 812px viewport and it is the first thing to cut. It is a `<footer>`
 * because it is exactly that — the application's contentinfo landmark, carrying
 * the machine's state and the simulation notice.
 */
export function StatusRail({ className }: StatusRailProps) {
  return (
    <footer
      aria-label="Stream status"
      className={cn(
        'sticky bottom-0 z-30 hidden h-6 shrink-0 border-t border-rule bg-ground-1',
        'md:block',
        className,
      )}
    >
      <StatusReadout />
    </footer>
  );
}
