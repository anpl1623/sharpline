/**
 * The board's controls, and the board's WINDOW VOCABULARY.
 *
 * # Why the window helpers live in the toolbar's file
 *
 * `starting_before` is a server-side query parameter — the route computes it
 * before it fetches — and the toolbar is the control that sets it. Splitting the
 * two would put the list of offered windows in one file and their meaning in
 * another, which is how a control ends up offering a choice the fetch cannot
 * honour.
 *
 * So this module carries NO `'use client'` directive and calls no hook. That is
 * load-bearing, not incidental: the two board routes are server components and
 * import `parseBoardWindow` / `startingBeforeFor` / `boardHref` from here, and a
 * client module's exports become client references that a server render cannot
 * call. Everything below is either a pure function or a presentational component
 * whose only interactivity arrives as props.
 *
 * # There is no "all events" window, and saying there was would be a lie
 *
 * `starting_before` is an UPPER BOUND on `scheduled_start` and the API defaults
 * it to +24h when it is omitted. There is no unbounded form of this request, so
 * the widest choice offered here is a real seven-day bound rather than an "All"
 * button that quietly means something narrower than the word.
 *
 * # The pager is keyset, so there are no page numbers
 *
 * `PageInfo` carries `has_more` and an opaque `next_cursor` and no total count,
 * and CLAUDE.md says there never will be one. A "page 3 of 12" control cannot be
 * built on that and should not be faked: `BoardPagination` appends the next
 * keyset page and stops when the API says there is nothing after it.
 */

import Link from 'next/link';
import type { ReactNode } from 'react';

import { cn } from '@/lib/utils';

// -----------------------------------------------------------------------------
// The window vocabulary
// -----------------------------------------------------------------------------

export type BoardWindowId = '3h' | '24h' | '48h' | '7d';

export const BOARD_WINDOW_IDS = ['3h', '24h', '48h', '7d'] as const;

export interface BoardWindowOption {
  readonly id: BoardWindowId;
  /** Hours added to the request instant to produce `starting_before`. */
  readonly hours: number;
  /** The control's own label. Dense: it sits in a filter bar. */
  readonly label: string;
  /** The same window in a sentence, for the empty state and the caption. */
  readonly phrase: string;
}

const BOARD_WINDOW_BY_ID: Readonly<Record<BoardWindowId, BoardWindowOption>> = {
  '3h': { id: '3h', hours: 3, label: '3 hours', phrase: 'the next 3 hours' },
  '24h': { id: '24h', hours: 24, label: '24 hours', phrase: 'the next 24 hours' },
  '48h': { id: '48h', hours: 48, label: '48 hours', phrase: 'the next 48 hours' },
  '7d': { id: '7d', hours: 24 * 7, label: '7 days', phrase: 'the next 7 days' },
};

export const BOARD_WINDOWS: readonly BoardWindowOption[] = BOARD_WINDOW_IDS.map(
  (id) => BOARD_WINDOW_BY_ID[id],
);

/**
 * The API's own default is 24 hours from the time the request is served, so this
 * is the one window whose behaviour is identical whether or not the parameter is
 * sent. That makes it the right default here and the right one to omit from a
 * URL.
 */
export const DEFAULT_BOARD_WINDOW: BoardWindowId = '24h';

export function boardWindowOption(id: BoardWindowId): BoardWindowOption {
  return BOARD_WINDOW_BY_ID[id];
}

function firstParam(raw: string | readonly string[] | undefined): string | null {
  if (raw === undefined) return null;
  if (typeof raw === 'string') return raw;
  return raw[0] ?? null;
}

/** An unrecognised value falls back to the default rather than erroring. */
export function parseBoardWindow(
  raw: string | readonly string[] | undefined,
): BoardWindowId {
  const value = firstParam(raw);
  if (value === null) return DEFAULT_BOARD_WINDOW;
  for (const id of BOARD_WINDOW_IDS) {
    if (id === value) return id;
  }
  return DEFAULT_BOARD_WINDOW;
}

export function parseLiveOnly(
  raw: string | readonly string[] | undefined,
): boolean {
  const value = firstParam(raw);
  return value === '1' || value === 'true';
}

/**
 * `starting_before`, as RFC 3339.
 *
 * Takes the request instant explicitly rather than reading the clock, so the
 * value a route sends and the value it hands to the client for its follow-up
 * pages are provably the same string. A cursor is bound to the filters it was
 * minted under and is rejected with a 400 if presented with different ones, so
 * "recompute it on the client" is a bug that only shows up at the page boundary.
 */
export function startingBeforeFor(id: BoardWindowId, now: Date): string {
  const option = boardWindowOption(id);
  return new Date(now.getTime() + option.hours * 3_600_000).toISOString();
}

/** The next wider window, or null at the widest. Drives the empty state's fix. */
export function widerBoardWindow(id: BoardWindowId): BoardWindowId | null {
  const index = BOARD_WINDOW_IDS.indexOf(id);
  if (index < 0) return null;
  return BOARD_WINDOW_IDS[index + 1] ?? null;
}

export interface BoardHrefParams {
  readonly window: BoardWindowId;
  readonly liveOnly: boolean;
}

/** Defaults are omitted, so the plain board URL stays `/board`. */
export function boardHref(basePath: string, params: BoardHrefParams): string {
  const search = new URLSearchParams();
  if (params.window !== DEFAULT_BOARD_WINDOW) search.set('window', params.window);
  if (params.liveOnly) search.set('live', '1');
  const rendered = search.toString();
  return rendered === '' ? basePath : `${basePath}?${rendered}`;
}

// -----------------------------------------------------------------------------
// Presentation
// -----------------------------------------------------------------------------

/**
 * Board chrome sits at the `sm` control height, not the 44px one.
 *
 * DESIGN.md sets 44–48px for controls outside the board and reserves the 36px
 * step for "dense chrome (header controls, filter bars)". A 44px button stacked
 * directly on a 36px row reads as a different product from the table below it.
 */
const CONTROL = [
  'inline-flex h-9 items-center justify-center whitespace-nowrap',
  'rounded-price border px-3 t-ui ui-transition',
].join(' ');

const CONTROL_IDLE = 'border-rule bg-transparent text-ink-2 hover:border-rule-hi hover:text-ink';
const CONTROL_ACTIVE = 'border-rule-hi bg-ground-2 text-ink';

interface SegmentProps {
  readonly href: string;
  readonly active: boolean;
  readonly children: ReactNode;
}

/**
 * One choice in a group of choices, as a real link.
 *
 * `aria-current` rather than `aria-pressed`: these are navigations, not toggles,
 * and a link that claims to be a button is worse for a screen reader than one
 * that simply says which of the group is the current view. The attribute is
 * spread conditionally because `exactOptionalPropertyTypes` forbids passing an
 * explicit `undefined` where the prop is optional.
 */
function Segment({ href, active, children }: SegmentProps) {
  return (
    <Link
      href={href}
      className={cn(CONTROL, active ? CONTROL_ACTIVE : CONTROL_IDLE)}
      {...(active ? { 'aria-current': 'true' as const } : {})}
    >
      {children}
    </Link>
  );
}

export interface BoardToolbarProps {
  readonly basePath: string;
  readonly window: BoardWindowId;
  readonly liveOnly: boolean;
  /** Rows actually rendered — after the live-only filter. */
  readonly shownCount: number;
  /** Rows returned by the API across every page loaded so far. */
  readonly loadedCount: number;
  readonly hasMore: boolean;
  /** Which book (or which set of books) the prices on screen come from. */
  readonly priceBasis: string;
  /**
   * The provenance sentence for the books in play, or null when the book
   * catalogue could not be read. A synthetic book's quote is a statement about a
   * random number generator and the surface rendering it has to be able to say
   * so.
   */
  readonly provenance: string | null;
}

export function BoardToolbar({
  basePath,
  window: windowId,
  liveOnly,
  shownCount,
  loadedCount,
  hasMore,
  priceBasis,
  provenance,
}: BoardToolbarProps) {
  const filtered = liveOnly && shownCount !== loadedCount;

  return (
    <div className="flex flex-col gap-3 border-b border-rule px-4 py-3">
      <div className="flex flex-wrap items-center gap-3">
        {/* `aria-labelledby` onto the VISIBLE label rather than a duplicate
            `aria-label`, so a screen reader hears the group named once and hears
            the same words a sighted reader sees. */}
        <div
          role="group"
          aria-labelledby="board-window-label"
          className="flex flex-wrap items-center gap-1"
        >
          <span id="board-window-label" className="t-label mr-1 text-ink-muted">
            Starting within
          </span>
          {BOARD_WINDOWS.map((option) => (
            <Segment
              key={option.id}
              href={boardHref(basePath, { window: option.id, liveOnly })}
              active={option.id === windowId}
            >
              {option.label}
            </Segment>
          ))}
        </div>

        <div
          role="group"
          aria-labelledby="board-status-label"
          className="flex flex-wrap items-center gap-1"
        >
          <span id="board-status-label" className="t-label mr-1 text-ink-muted">
            Status
          </span>
          <Segment
            href={boardHref(basePath, { window: windowId, liveOnly: false })}
            active={!liveOnly}
          >
            All
          </Segment>
          <Segment
            href={boardHref(basePath, { window: windowId, liveOnly: true })}
            active={liveOnly}
          >
            Live only
          </Segment>
        </div>
      </div>

      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <p className="t-ui text-ink-2">
          {filtered
            ? `${String(shownCount)} live of ${String(loadedCount)} loaded`
            : `${String(shownCount)} ${shownCount === 1 ? 'event' : 'events'}`}
          {hasMore ? ' · more available' : ''}
        </p>
        <p className="t-ui text-ink-muted">{priceBasis}</p>
        {provenance === null ? null : (
          <p className="t-mono text-ink-muted">{provenance}</p>
        )}
      </div>
    </div>
  );
}

export interface BoardPaginationProps {
  readonly hasMore: boolean;
  readonly loading: boolean;
  readonly error: string | null;
  readonly onLoadMore: () => void;
}

/**
 * The keyset pager. One button, and it disappears when the API says the set is
 * exhausted — there is no page count to render and nothing to number.
 */
export function BoardPagination({
  hasMore,
  loading,
  error,
  onLoadMore,
}: BoardPaginationProps) {
  if (!hasMore && error === null) return null;

  return (
    <div className="flex flex-col items-start gap-2 px-4 py-4">
      {error === null ? null : (
        <p className="t-ui text-loss" role="status">
          {error}
        </p>
      )}
      {hasMore ? (
        <button
          type="button"
          onClick={onLoadMore}
          disabled={loading}
          className={cn(
            CONTROL,
            CONTROL_IDLE,
            'disabled:pointer-events-none disabled:text-ink-faint',
          )}
        >
          {loading ? 'Loading…' : 'Load more events'}
        </button>
      ) : null}
    </div>
  );
}
