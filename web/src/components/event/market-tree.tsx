'use client';

/**
 * Every market on the event, grouped by type in a fixed order.
 *
 * # The order is fixed here; the order INSIDE a market is not ours to choose
 *
 * Market types are ordered moneyline → spread → total → player prop → futures,
 * because that is the order a bettor looks for them in and a stable order is
 * what makes the page navigable by memory rather than by reading.
 *
 * Within a market, selections arrive in the API's display order — home, draw,
 * away, over, under, outright — and are rendered exactly as they arrive. That
 * order is not lexicographic and it is not alphabetical by name; the API sorts
 * it precisely so that every client renders the same tree, and a client that
 * re-sorts breaks the guarantee for everyone.
 *
 * # A group heading only when it earns its place
 *
 * A group holding one market would put "Moneyline" immediately above a panel
 * headed "Moneyline". The heading appears only where a group holds more than
 * one market — which in practice means player props — and the group is named
 * for a screen reader either way through `aria-label`.
 */

import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';

import { MarketPanel } from '@/components/event/market-panel';
import { booksQueryOptions } from '@/lib/api/queries';
import type {
  SchemaBook,
  SchemaMarket,
  SchemaMarketType,
} from '@/lib/api/schema';
import { marketTypeLabel } from '@/lib/odds/line';

/** The order a bettor looks for them in. */
const MARKET_TYPE_ORDER: readonly SchemaMarketType[] = [
  'moneyline',
  'spread',
  'total',
  'player_prop',
  'futures',
];

export interface MarketTreeProps {
  readonly eventId: string;
  readonly eventName: string;
  readonly markets: readonly SchemaMarket[];
  readonly asOf: string;
  /** Market ids the socket is currently carrying on this event's channel. */
  readonly liveMarketIds: readonly string[];
}

interface MarketGroup {
  readonly type: SchemaMarketType | string;
  readonly label: string;
  readonly markets: readonly SchemaMarket[];
}

function groupMarkets(markets: readonly SchemaMarket[]): readonly MarketGroup[] {
  const groups: MarketGroup[] = [];

  for (const type of MARKET_TYPE_ORDER) {
    const matching = markets.filter((market) => market.type === type);
    if (matching.length === 0) continue;
    groups.push({ type, label: marketTypeLabel(type), markets: matching });
  }

  // A market type the generated union does not know about cannot exist as far
  // as the compiler is concerned, but it can exist on the wire the day the API
  // adds one. Appending the leftovers means a new type shows up unstyled rather
  // than silently disappearing, which is the failure that is hard to notice.
  const known = new Set<string>(MARKET_TYPE_ORDER);
  const leftovers = markets.filter((market) => !known.has(market.type));
  if (leftovers.length > 0) {
    groups.push({ type: 'other', label: 'Other markets', markets: leftovers });
  }

  return groups;
}

export function MarketTree({
  eventId,
  eventName,
  markets,
  asOf,
  liveMarketIds,
}: MarketTreeProps) {
  // Catalogue data: cached for minutes, shared by every panel on the page
  // through one query key, and used only to turn a book slug into a name. The
  // event payload carries slugs and no names.
  const catalogue = useQuery(booksQueryOptions());

  const books = useMemo<ReadonlyMap<string, SchemaBook>>(
    () =>
      new Map(
        (catalogue.data?.data ?? []).map(
          (book): [string, SchemaBook] => [book.slug, book],
        ),
      ),
    [catalogue.data],
  );

  const liveSet = useMemo(() => new Set(liveMarketIds), [liveMarketIds]);
  const groups = useMemo(() => groupMarkets(markets), [markets]);

  if (markets.length === 0) {
    return (
      <section
        aria-label="Markets"
        className="rounded-card border border-rule bg-ground-1 p-4"
      >
        <p className="t-body text-ink">No markets are quoted on this event.</p>
        <p className="t-body text-ink-2">
          A market appears here once a book has quoted it inside the freshness
          window. An empty tree is an answer, not a failure.
        </p>
      </section>
    );
  }

  return (
    <div className="flex flex-col gap-8">
      {groups.map((group) => (
        <section
          key={group.type}
          aria-label={group.label}
          className="flex flex-col gap-3"
        >
          {group.markets.length > 1 ? (
            <h2 className="t-label text-ink-muted">
              {`${group.label} · ${String(group.markets.length)}`}
            </h2>
          ) : null}

          {group.markets.map((market) => (
            <MarketPanel
              key={market.id}
              eventId={eventId}
              eventName={eventName}
              market={market}
              asOf={asOf}
              live={liveSet.has(market.id)}
              books={books}
            />
          ))}
        </section>
      ))}
    </div>
  );
}
