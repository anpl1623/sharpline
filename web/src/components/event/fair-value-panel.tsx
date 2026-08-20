'use client';

/**
 * The engineering layer for one market: how the no-vig fair value was computed,
 * and how every book's price scores against it.
 *
 * # This panel reads the STREAM, not REST
 *
 * `pricing.ComputedMarket` is published to the compacted `price.computed` topic
 * and propagated byte-for-byte by the gateway. Nothing in the REST API carries
 * the devig method, the margin decomposition, or a quote's EV, edge and Kelly —
 * so when the socket is not delivering, this panel says so instead of rendering
 * the last numbers it saw as though they were current. Stale EV is worse than
 * no EV: it is actionable and wrong.
 *
 * # Everything here is in the mono register, on purpose
 *
 * DESIGN.md separates the two audiences by TEXTURE rather than by a mode
 * switch: the consumer surface is sans, and every mono glyph on screen means
 * the machine is talking. The market tree above this is sans; this is mono; a
 * reader who does not care never has to be told which is which.
 *
 * # `implied` and `fair_probability` are not the same number
 *
 * `implied` is `1/decimal` with the book's vig still in it. `probability` on a
 * fair selection is the devigged one. They differ by the margin, they are both
 * "probabilities", and confusing them is the single easiest mistake available
 * on this surface — so every column heading below names which one it is.
 */

import type { ReactNode } from 'react';

import { Badge } from '@/components/ui';
import { useLocalTimeZone } from '@/components/event/use-event-live';
import {
  formatOdds,
  renderPercent,
  renderSignedPercent,
} from '@/lib/odds/format';
import type { OddsFormat } from '@/lib/odds/format';
import { selectionRoleLabel } from '@/lib/odds/line';
import { useOddsFormat } from '@/lib/store/preferences';
import { formatAbsolute, formatCompactDuration } from '@/lib/time';
import type { BookAssessment, ComputedMarket } from '@/lib/ws/protocol';
import { useComputedMarket, useStreamDescription } from '@/lib/ws/provider';

export interface FairValuePanelProps {
  readonly marketId: string;
  /** "Moneyline — Team A at Team B". For the table captions. */
  readonly marketLabel: string;
}

function Fact({
  term,
  children,
}: {
  readonly term: string;
  readonly children: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="t-label text-ink-muted">{term}</dt>
      <dd className="t-mono text-ink-2">{children}</dd>
    </div>
  );
}

function BookBadges({ book }: { readonly book: BookAssessment }) {
  return (
    <span className="flex flex-wrap gap-1 pt-1">
      {book.reference ? <Badge variant="info">reference</Badge> : null}
      {/* ADR 0003: a synthetic quote is a statement about a random number
          generator, and every surface that renders one must say so. */}
      {book.kind === 'synthetic' ? (
        <Badge variant="neutral">synthetic</Badge>
      ) : null}
      {book.complete ? null : <Badge variant="neutral">partial</Badge>}
      {book.eligible ? null : <Badge variant="neutral">not eligible</Badge>}
    </span>
  );
}

/**
 * What is shown when the slate holds nothing for this market.
 *
 * The connection state is read HERE and not in `FairValuePanel`, deliberately.
 * `useStreamDescription` is built on `useStreamStatus`, which notifies on every
 * frame — a delta, an ack, a pong. A component that subscribes to it re-renders
 * several times a second, and this panel renders two tables. Reading it in the
 * branch that actually needs it means the populated panel re-renders when its
 * own market document changes and at no other time.
 */
function StreamAbsent() {
  const stream = useStreamDescription();

  if (stream.tone === 'streaming') {
    return (
      <div className="flex flex-col gap-3 rounded-card border border-rule bg-ground-1 p-4">
        <p className="t-body text-ink">
          Connected. Waiting for the first computed record on this market.
        </p>
        <p className="t-body text-ink-2">
          A market appears on the stream when the pricer publishes it, which
          happens when its price actually changes. A market that has not moved
          since this connection opened has nothing on the wire yet.
        </p>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3 rounded-card border border-rule bg-ground-1 p-4">
      <p className="t-body text-ink">
        {`No fair value: the stream is ${stream.label.toLowerCase()}.`}
      </p>
      <p className="t-body text-ink-2">
        Devig method, margin, expected value and Kelly are computed on the live
        stream and exist nowhere else. Showing the last ones received would put
        an actionable number on screen with nothing to say it is out of date, so
        nothing is shown.
      </p>
    </div>
  );
}

export function FairValuePanel({ marketId, marketLabel }: FairValuePanelProps) {
  const format = useOddsFormat();
  const timeZone = useLocalTimeZone();
  const market = useComputedMarket(marketId);

  if (market === undefined) return <StreamAbsent />;

  return (
    <FairValueBody
      market={market}
      marketLabel={marketLabel}
      format={format}
      timeZone={timeZone}
    />
  );
}

function FairValueBody({
  market,
  marketLabel,
  format,
  timeZone,
}: {
  readonly market: ComputedMarket;
  readonly marketLabel: string;
  readonly format: OddsFormat;
  readonly timeZone: string;
}) {
  const { fair, reference, books } = market;
  const margin = fair.margin;
  const arbitrageCount = market.arbitrage?.length ?? 0;
  const middleCount = market.middles?.length ?? 0;

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="neutral">{`devig ${fair.method}`}</Badge>
        {fair.fallback ? (
          <Badge variant="info">{`fell back from ${fair.requested_method}`}</Badge>
        ) : null}
        {/* The ONE saturated fill in the interface. Absent on almost every
            market, and absent is correct — a feed with a constant arbitrage on
            it is a feed with a bug. */}
        {arbitrageCount > 0 ? (
          <Badge variant="arb">
            {`arbitrage ×${String(arbitrageCount)}`}
          </Badge>
        ) : null}
        {middleCount > 0 ? (
          <Badge variant="neutral">{`middles ×${String(middleCount)}`}</Badge>
        ) : null}
        <Badge variant="neutral">{`market ${market.market.status}`}</Badge>
      </div>

      <dl className="grid grid-cols-2 gap-4 md:grid-cols-4">
        <Fact term="Method">
          {fair.method}
          {fair.parameter === 0 ? '' : ` (p=${String(fair.parameter)})`}
        </Fact>
        <Fact term="Attribution">{fair.attribution}</Fact>
        <Fact term="Iterations">{String(fair.iterations)}</Fact>
        {/* The largest absolute probability difference between the devig
            methods that priced this market — the fair value's error bar, in
            percentage points. A spread over two methods and one over four are
            different claims, so the count travels with it. */}
        <Fact term="Method spread (max |Δp|)">
          {`${renderPercent(fair.disagreement, 3)} over ${String(fair.methods_compared)} methods`}
        </Fact>

        <Fact term="Reference book">
          {`${reference.name} (${reference.slug})`}
        </Fact>
        <Fact term="Reference age">
          {formatCompactDuration(reference.age_seconds)}
        </Fact>
        <Fact term="Reference kind">{reference.kind}</Fact>
        <Fact term="Reference source">{reference.source}</Fact>

        <Fact term="Booking %">
          {`${margin.booking_percentage.toFixed(2)}%`}
        </Fact>
        <Fact term="Overround">{renderPercent(margin.overround, 2)}</Fact>
        <Fact term="Vig (hold)">{renderPercent(margin.vig, 2)}</Fact>
        <Fact term="Implied sum">
          {`${margin.implied_sum.toFixed(4)} over ${String(margin.selections)}`}
        </Fact>
      </dl>

      <section className="flex flex-col gap-2">
        <h4 className="t-label text-ink-muted">Fair value by selection</h4>
        <div className="board-scroll">
          <table className="w-full border-collapse">
            <caption className="sr-only">
              {`No-vig fair value for ${marketLabel}, devigged with the ${fair.method} method against ${reference.name}.`}
            </caption>
            <thead>
              <tr className="border-b border-rule">
                <th scope="col" className="t-label px-2 py-2 text-left text-ink-muted">
                  Selection
                </th>
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  Fair probability (no vig)
                </th>
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  Fair price
                </th>
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  Reference price
                </th>
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  Reference implied (with vig)
                </th>
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  Excess
                </th>
              </tr>
            </thead>
            <tbody>
              {fair.selections.map((selection) => (
                <tr key={selection.selection_id} className="border-b border-rule">
                  <th scope="row" className="px-2 py-2 text-left">
                    <span className="t-ui block text-ink">{selection.name}</span>
                    <span className="t-mono block text-ink-muted">
                      {selectionRoleLabel(selection.role)}
                    </span>
                  </th>
                  <td className="t-mono px-2 py-2 text-right text-ink">
                    {renderPercent(selection.probability, 2)}
                  </td>
                  <td className="t-price-sm px-2 py-2 text-right text-ink">
                    {formatOdds(selection.decimal, format)}
                  </td>
                  <td className="t-price-sm px-2 py-2 text-right text-ink-2">
                    {formatOdds(selection.reference_decimal, format)}
                  </td>
                  <td className="t-mono px-2 py-2 text-right text-ink-2">
                    {renderPercent(selection.reference_implied, 2)}
                  </td>
                  <td className="t-mono px-2 py-2 text-right text-ink-2">
                    {renderSignedPercent(selection.excess, 2)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className="flex flex-col gap-2">
        <h4 className="t-label text-ink-muted">Expected value by book</h4>
        <div className="board-scroll">
          <table className="w-full border-collapse">
            <caption className="sr-only">
              {`Every book's price on ${marketLabel} scored against the no-vig fair value: expected value and Kelly stake fraction.`}
            </caption>
            <thead>
              <tr className="border-b border-rule">
                <th scope="col" className="t-label px-2 py-2 text-left text-ink-muted">
                  Book
                </th>
                {fair.selections.map((selection) => (
                  <th
                    key={selection.selection_id}
                    scope="col"
                    className="t-label px-2 py-2 text-right text-ink-2"
                  >
                    {selection.name}
                  </th>
                ))}
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  Overround
                </th>
              </tr>
            </thead>
            <tbody>
              {books.map((book) => (
                <tr key={book.book_id} className="border-b border-rule">
                  <th scope="row" className="px-2 py-2 text-left align-top">
                    <span className="t-ui block text-ink">{book.name}</span>
                    <span className="t-mono block text-ink-muted">
                      {formatCompactDuration(book.age_seconds)}
                    </span>
                    <BookBadges book={book} />
                  </th>

                  {fair.selections.map((selection) => {
                    const quote = book.quotes.find(
                      (entry) => entry.selection_id === selection.selection_id,
                    );
                    if (quote === undefined) {
                      return (
                        <td
                          key={selection.selection_id}
                          className="px-2 py-2 text-right align-top"
                        >
                          <span className="t-mono text-ink-muted">
                            not quoted
                          </span>
                        </td>
                      );
                    }
                    const positive = quote.expected_value > 0;
                    return (
                      <td
                        key={selection.selection_id}
                        className="px-2 py-2 text-right align-top"
                      >
                        <span className="flex flex-col items-end gap-1">
                          <span className="t-price-sm text-ink">
                            {formatOdds(quote.decimal, format)}
                          </span>
                          <span className="t-mono text-ink-2">
                            {`EV ${renderSignedPercent(quote.expected_value, 2)}`}
                          </span>
                          {positive ? (
                            <>
                              {/* DESIGN.md § Signals: +EV is a TINTED badge —
                                  8% fill, 40% border. The saturated money fill
                                  is spent on arbitrage and nothing else. */}
                              <Badge variant="money">+EV</Badge>
                              <span className="t-mono text-ink-muted">
                                {`kelly ${renderPercent(quote.kelly, 2)} · frac ${renderPercent(quote.fractional_kelly, 2)}`}
                              </span>
                            </>
                          ) : null}
                        </span>
                      </td>
                    );
                  })}

                  <td className="t-mono px-2 py-2 text-right align-top text-ink-2">
                    {book.complete
                      ? renderPercent(book.margin.overround, 2)
                      : 'partial'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <p className="t-mono text-ink-muted">
        {`provider ${market.provider} · schema v${String(market.schema_version)} · fingerprint ${market.source_fingerprint.slice(0, 12)} · observed ${formatAbsolute(market.observed_at, timeZone)} · ingested ${formatAbsolute(market.ingested_at, timeZone)}`}
      </p>
      <p className="t-body text-ink-2">
        Every expected value above is measured against {reference.name}&rsquo;s
        devigged price. A book&rsquo;s own implied probability is 1/price and
        still contains that book&rsquo;s margin; it is never a fair probability.
      </p>
    </div>
  );
}
