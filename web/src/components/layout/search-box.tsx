'use client';

/**
 * Event search — a type-ahead combobox over `GET /search`.
 *
 * # What this endpoint does and does not cover
 *
 * The match is a PREFIX match on competitor name, and the status set is
 * deliberately NARROWER than the board's: scheduled and live events only. A
 * suspended, ended, settled, postponed or cancelled event is therefore NOT
 * findable here. That is the API's documented behaviour and it is correct — a
 * search box is for getting to a game you might act on — so this component does
 * not work around it, does not widen the query, and does not apologise for it.
 * A user who wants a finished game reaches it from the board.
 *
 * # ARIA
 *
 * The WAI-ARIA 1.2 combobox-with-listbox-popup pattern, in its
 * `aria-activedescendant` form: DOM focus never leaves the input, and the
 * "current option" is named by id. Arrow keys move it, Enter selects it, Escape
 * closes. The options are `<li role="option">` rather than links precisely
 * because focus must stay in the field — a list of anchors would move focus on
 * every arrow press and destroy the typing experience.
 *
 * The popup stays mounted-but-empty rather than being replaced by a message
 * element when there are no hits: `aria-controls` must point at something that
 * exists, and a listbox may legally contain zero options. The "no events match"
 * line is a sibling of the listbox, not a child of it, because a listbox's
 * children must all be options.
 *
 * # Two instances, one for each width
 *
 * At >= 768px the field is permanently open in the header row. Below that the
 * header has no room for it, so it collapses to an icon that expands the same
 * combobox across the row. Both are rendered and CSS picks one — a media query
 * evaluated in JavaScript would either flash the wrong control on first paint or
 * force the whole header to be client-rendered.
 *
 * # No fabricated suggestions
 *
 * An empty query issues no request and shows no popup. A query with no hits
 * shows one plain line saying so. Nothing here ever invents a "did you mean", a
 * popular-searches list, or a placeholder event.
 */

import { useQuery } from '@tanstack/react-query';
import { Search, X } from 'lucide-react';
import { useRouter } from 'next/navigation';
import { useCallback, useEffect, useId, useRef, useState } from 'react';
import type { KeyboardEvent } from 'react';

import { Input } from '@/components/ui';
import { developerDetail, userFacingMessage } from '@/lib/api/errors';
import { MIN_SEARCH_LENGTH, searchQueryOptions } from '@/lib/api/queries';
import type { SchemaSearchHit } from '@/lib/api/schema';
import { useDisplayTimeZone } from '@/lib/client-value';
import { formatDayAndTime, toDateTimeAttribute } from '@/lib/time';
import { cn } from '@/lib/utils';

/** Long enough that a fast typist issues one request, short enough to feel live. */
const DEBOUNCE_MS = 200;

/** Stable identity so "no results" does not churn referentially. */
const NO_HITS: readonly SchemaSearchHit[] = [];

export function SearchBox() {
  return (
    <>
      {/* >= 768px — a permanent field. */}
      <SearchField className="hidden md:block md:w-52 lg:w-72" />
      {/* < 768px — an icon that expands across the header row. */}
      <CompactSearch className="md:hidden" />
    </>
  );
}

// -----------------------------------------------------------------------------
// The combobox
// -----------------------------------------------------------------------------

interface SearchFieldProps {
  readonly className?: string | undefined;
  /** Move focus here on mount. Used by the compact expansion. */
  readonly focusOnMount?: boolean;
  /** Called on Escape, so a wrapper can collapse itself. */
  readonly onDismiss?: (() => void) | undefined;
}

function SearchField({
  className,
  focusOnMount = false,
  onDismiss,
}: SearchFieldProps) {
  const router = useRouter();
  const baseId = useId();
  const listboxId = `${baseId}-listbox`;
  const optionId = (index: number): string => `${baseId}-option-${String(index)}`;

  const inputRef = useRef<HTMLInputElement | null>(null);
  const optionRefs = useRef<(HTMLLIElement | null)[]>([]);

  const [query, setQuery] = useState('');
  const [debounced, setDebounced] = useState('');
  const [open, setOpen] = useState(false);
  const [activeIndex, setActiveIndex] = useState(-1);

  // Local time, resolved after hydration. The server render and the hydration
  // render both use UTC so hydration cannot disagree about a timestamp.
  const timeZone = useDisplayTimeZone();

  useEffect(() => {
    if (!focusOnMount) return;
    inputRef.current?.focus();
  }, [focusOnMount]);

  useEffect(() => {
    const id = setTimeout(() => {
      setDebounced(query.trim());
    }, DEBOUNCE_MS);
    return () => {
      clearTimeout(id);
    };
  }, [query]);

  // A new query invalidates the highlight; nothing is pre-selected, so Enter on
  // a fresh result set does nothing rather than navigating somewhere arbitrary.
  // Adjusted during render rather than in an effect: an effect would paint one
  // frame with the previous query's highlight still on a row that has moved.
  const [highlightedQuery, setHighlightedQuery] = useState(debounced);
  if (highlightedQuery !== debounced) {
    setHighlightedQuery(debounced);
    setActiveIndex(-1);
  }

  const { data, isFetching, isError, error } = useQuery(
    searchQueryOptions(debounced),
  );

  const hits = data?.data ?? NO_HITS;
  const longEnough = query.trim().length >= MIN_SEARCH_LENGTH;
  const expanded = open && longEnough;
  // Only report "no matches" once an answer for THIS query has arrived.
  const settled = !isFetching && debounced === query.trim() && debounced !== '';

  useEffect(() => {
    if (activeIndex < 0) return;
    optionRefs.current[activeIndex]?.scrollIntoView({ block: 'nearest' });
  }, [activeIndex]);

  const select = useCallback(
    (hit: SchemaSearchHit) => {
      setOpen(false);
      setActiveIndex(-1);
      setQuery('');
      setDebounced('');
      router.push(`/events/${encodeURIComponent(hit.id)}`);
    },
    [router],
  );

  const dismiss = useCallback(() => {
    setOpen(false);
    setActiveIndex(-1);
    onDismiss?.();
  }, [onDismiss]);

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLInputElement>) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        dismiss();
        return;
      }
      if (!expanded) {
        if (event.key === 'ArrowDown' && longEnough) setOpen(true);
        return;
      }
      switch (event.key) {
        case 'ArrowDown':
          event.preventDefault();
          setActiveIndex((current) =>
            hits.length === 0 ? -1 : (current + 1) % hits.length,
          );
          break;
        case 'ArrowUp':
          event.preventDefault();
          setActiveIndex((current) =>
            hits.length === 0
              ? -1
              : current <= 0
                ? hits.length - 1
                : current - 1,
          );
          break;
        case 'Home':
          if (hits.length > 0) {
            event.preventDefault();
            setActiveIndex(0);
          }
          break;
        case 'End':
          if (hits.length > 0) {
            event.preventDefault();
            setActiveIndex(hits.length - 1);
          }
          break;
        case 'Enter': {
          const hit = hits[activeIndex];
          if (hit !== undefined) {
            event.preventDefault();
            select(hit);
          }
          break;
        }
        case 'Tab':
          setOpen(false);
          setActiveIndex(-1);
          break;
        default:
          break;
      }
    },
    [dismiss, expanded, hits, activeIndex, longEnough, select],
  );

  const activeDescendant =
    expanded && activeIndex >= 0 && activeIndex < hits.length
      ? optionId(activeIndex)
      : undefined;

  return (
    <div className={cn('relative', className)}>
      <Search
        className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-ink-muted"
        aria-hidden="true"
      />
      <Input
        ref={inputRef}
        type="search"
        role="combobox"
        aria-expanded={expanded}
        aria-controls={listboxId}
        aria-autocomplete="list"
        aria-activedescendant={activeDescendant}
        aria-label="Search events"
        placeholder="Search events"
        autoComplete="off"
        spellCheck={false}
        value={query}
        className="h-9 pl-8"
        onChange={(event) => {
          setQuery(event.target.value);
          setOpen(true);
        }}
        onFocus={() => {
          setOpen(true);
        }}
        onKeyDown={onKeyDown}
      />

      {/* A polite count, announced when a query settles. This is a
       * user-initiated search, so a status here is expected rather than
       * intrusive — unlike the price stream, which is throttled to one
       * announcement every five seconds elsewhere. */}
      <span role="status" aria-live="polite" className="sr-only">
        {expanded && settled
          ? hits.length === 0
            ? 'No events match'
            : `${String(hits.length)} event${hits.length === 1 ? '' : 's'} match`
          : ''}
      </span>

      <div
        hidden={!expanded}
        className={cn(
          'absolute top-full right-0 left-0 z-50 mt-1 max-h-80 overflow-y-auto',
          'rounded-sheet border border-rule-hi bg-ground-2 p-1',
          /* Right-aligned above the breakpoint so a popup wider than the field
           * grows toward the middle of the header rather than off the edge. */
          'md:left-auto md:w-80 lg:w-96',
        )}
      >
        <ul id={listboxId} role="listbox" aria-label="Event search results">
          {hits.map((hit, index) => (
            <li
              key={hit.id}
              id={optionId(index)}
              ref={(node) => {
                optionRefs.current[index] = node;
              }}
              role="option"
              aria-selected={index === activeIndex}
              /* Pointer down would blur the input and close the popup before
               * the click landed. Suppress it and act on the click. */
              onPointerDown={(event) => {
                event.preventDefault();
              }}
              onClick={() => {
                select(hit);
              }}
              onMouseMove={() => {
                setActiveIndex(index);
              }}
              className={cn(
                'flex cursor-default flex-col gap-0.5 rounded-price px-2 py-2',
                index === activeIndex ? 'bg-ground-3' : 'bg-transparent',
              )}
            >
              <span className="t-ui text-ink">{competitors(hit)}</span>
              <span className="flex items-center gap-2 t-mono text-ink-muted">
                <time dateTime={toDateTimeAttribute(hit.scheduled_start)}>
                  {formatDayAndTime(hit.scheduled_start, timeZone)}
                </time>
                {hit.status === 'live' ? (
                  <span className="t-label text-ink-2">Live</span>
                ) : null}
              </span>
            </li>
          ))}
        </ul>

        {isError ? (
          <div className="flex flex-col gap-1 px-2 py-2">
            <p className="t-ui text-loss">{userFacingMessage(error)}</p>
            {developerDetail(error) === null ? null : (
              <p className="t-mono text-ink-muted">{developerDetail(error)}</p>
            )}
          </div>
        ) : null}

        {!isError && settled && hits.length === 0 ? (
          <p className="px-2 py-2 t-ui text-ink-muted">No events match</p>
        ) : null}

        {!isError && hits.length === 0 && !settled ? (
          <p className="px-2 py-2 t-mono text-ink-muted">Searching&hellip;</p>
        ) : null}
      </div>
    </div>
  );
}

/**
 * `away @ home` when both competitors are known, and the event's own name when
 * they are not — an outright has no two sides. Nothing is invented for a missing
 * competitor: the fallback is a real field on the same record.
 */
function competitors(hit: SchemaSearchHit): string {
  const home = hit.home_competitor_name ?? null;
  const away = hit.away_competitor_name ?? null;
  if (home !== null && home !== '' && away !== null && away !== '') {
    return `${away} @ ${home}`;
  }
  return hit.name;
}

// -----------------------------------------------------------------------------
// The compact (< 768px) expansion
// -----------------------------------------------------------------------------

function CompactSearch({ className }: { readonly className?: string }) {
  const [open, setOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: PointerEvent): void => {
      const container = containerRef.current;
      if (container === null) return;
      if (event.target instanceof Node && container.contains(event.target)) {
        return;
      }
      setOpen(false);
    };
    document.addEventListener('pointerdown', onPointerDown);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown);
    };
  }, [open]);

  return (
    <div ref={containerRef} className={cn('relative shrink-0', className)}>
      <button
        type="button"
        aria-label="Search events"
        aria-expanded={open}
        onClick={() => {
          setOpen((current) => !current);
        }}
        className={cn(
          'inline-flex size-9 items-center justify-center rounded-price',
          'border border-rule bg-ground-2 text-ink-2 ui-transition',
          'hover:border-rule-hi hover:text-ink',
        )}
      >
        <Search className="size-4" aria-hidden="true" />
      </button>

      {open ? (
        <div className="absolute top-1/2 right-0 z-40 flex w-[calc(100vw-1.5rem)] max-w-[26rem] -translate-y-1/2 items-center gap-2 rounded-price bg-ground-1">
          <SearchField
            className="min-w-0 flex-1"
            focusOnMount
            onDismiss={() => {
              setOpen(false);
            }}
          />
          <button
            type="button"
            aria-label="Close search"
            onClick={() => {
              setOpen(false);
            }}
            className={cn(
              'inline-flex size-9 shrink-0 items-center justify-center rounded-price',
              'text-ink-muted ui-transition hover:bg-ground-2 hover:text-ink',
            )}
          >
            <X className="size-4" aria-hidden="true" />
          </button>
        </div>
      ) : null}
    </div>
  );
}
