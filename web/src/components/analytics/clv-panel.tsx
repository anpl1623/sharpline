'use client';

/**
 * The signed-in customer's closing line value.
 *
 * # Why this route fetches on the client
 *
 * The same reason `/bets` does: every endpoint under `/account` is scoped to the
 * token's own user, the access token lives IN MEMORY IN THE BROWSER and is never
 * persisted, and a server render has no credential to present. Giving the server
 * one would mean a cookie-bearing API and a CSRF surface to defend.
 *
 * # The one thing this panel must not do
 *
 * COMPUTE ITS OWN MEAN FROM `data`. The rows include line-moved and voided legs;
 * the aggregate excludes both. A client that averaged the rows it was given
 * would produce a number that disagrees with the one the leaderboard ranked this
 * customer on, and the disagreement would be invisible — two plausible
 * percentages, no error anywhere. So every summary figure here comes from
 * `aggregate`, and the only arithmetic in this file is rendering.
 *
 * # A null mean is rendered as "no measurable wagers", never as 0.00%
 *
 * `mean_percent_clv` is null when nothing is countable. A customer with three
 * line-moved legs has no measurable CLV, and "0.00%" would tell them they are
 * exactly break-even — a claim nobody made. `odds.AggregateCLV` reports
 * `ErrCLVNoSamples` for the same reason.
 *
 * # No figure here is money and none is green
 *
 * A CLV percentage is a price-quality score, not an amount. There is no
 * currency value anywhere in this payload — no `*_minor` field exists on it —
 * and the `beat`/`missed` markers are neutral rather than money-coloured,
 * because "this leg beat the close" is not a settled win.
 */

import { usePathname } from 'next/navigation';
import { useQuery } from '@tanstack/react-query';

import { Badge } from '@/components/ui';
import { useLocalTimeZone } from '@/components/event/use-event-live';
import { CLVEmpty, CLVSignedOut, CLVUnavailable } from './clv-empty';
import { accountCLVQueryOptions } from '@/lib/api/queries';
import type { SchemaClvEntry, SchemaClvResponse } from '@/lib/api/schema';
import {
  formatFractionAsPercent,
  formatMarketLine,
  formatProbabilityPoints,
  formatSignedPercentPoints,
  NO_VALUE,
} from '@/lib/analytics/format';
import { formatOdds } from '@/lib/odds/format';
import { marketTypeShortLabel } from '@/lib/odds/line';
import { useAccessToken, useAuth } from '@/lib/store/auth';
import { useOddsFormat } from '@/lib/store/preferences';
import { formatAbsolute } from '@/lib/time';

/** The page size. Large enough that the first page is usually the whole story. */
const PAGE_LIMIT = 50;

export function CLVPanel() {
  const pathname = usePathname();
  const accessToken = useAccessToken();
  const format = useOddsFormat();
  const timeZone = useLocalTimeZone();

  // The same three-state resolution the wager history uses: the server renders
  // signed out because it has no storage, and the client only learns who is
  // signed in after the store rehydrates and redeems its refresh token. Showing
  // the signed-out panel during either step would be a visible lie that flips to
  // somebody's history a moment later.
  const hydrated = useAuth((state) => state.hydrated);
  const status = useAuth((state) => state.status);
  const hasStoredSession = useAuth(
    (state) => state.refreshToken !== null && state.refreshToken !== '',
  );
  const signedIn = accessToken !== null && accessToken !== '';
  const resolving =
    !hydrated ||
    status === 'authenticating' ||
    status === 'refreshing' ||
    (!signedIn && hasStoredSession);

  const clv = useQuery(
    accountCLVQueryOptions(accessToken, { limit: PAGE_LIMIT }),
  );

  if (resolving) {
    return (
      <p className="t-ui text-ink-muted" role="status">
        Checking your session…
      </p>
    );
  }

  if (!signedIn) return <CLVSignedOut pathname={pathname} />;

  if (clv.isError) {
    return (
      <CLVUnavailable
        error={clv.error}
        onRetry={() => {
          void clv.refetch();
        }}
      />
    );
  }

  if (clv.isPending) {
    return (
      <p className="t-ui text-ink-muted" role="status">
        Loading your closing line value…
      </p>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <CLVSummary response={clv.data} timeZone={timeZone} />

      {clv.data.data.length === 0 ? (
        <CLVEmpty />
      ) : (
        <section className="flex flex-col gap-2">
          <h2 className="t-label text-ink-muted">Graded legs</h2>
          <div className="board-scroll">
            <table className="w-full border-collapse">
              <caption className="sr-only">
                Every graded leg scored against the market&rsquo;s closing price.
                Line-moved and voided legs are shown and are excluded from the
                summary above.
              </caption>
              <thead>
                <tr className="border-b border-rule">
                  <th scope="col" className="t-label px-2 py-2 text-left text-ink-muted">
                    Market
                  </th>
                  <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                    Fair price taken
                  </th>
                  <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                    Fair price at close
                  </th>
                  <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                    CLV
                  </th>
                  <th scope="col" className="t-label px-2 py-2 text-left text-ink-muted">
                    Counted
                  </th>
                  <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                    Graded
                  </th>
                </tr>
              </thead>
              <tbody>
                {clv.data.data.map((entry) => (
                  <CLVRow
                    key={entry.leg_id}
                    entry={entry}
                    format={format}
                    timeZone={timeZone}
                  />
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      {clv.data.by_league.length === 0 ? null : (
        <section className="flex flex-col gap-2">
          <h2 className="t-label text-ink-muted">By league</h2>
          <p className="t-body text-ink-muted">
            The same summary cut by league, most evidence first — what you are
            actually good at. Only leagues with a countable leg appear.
          </p>
          <div className="board-scroll">
            <table className="w-full border-collapse">
              <caption className="sr-only">
                Mean closing line value per league, over countable legs only.
              </caption>
              <thead>
                <tr className="border-b border-rule">
                  <th scope="col" className="t-label px-2 py-2 text-left text-ink-muted">
                    League
                  </th>
                  <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                    Legs
                  </th>
                  <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                    Beat close
                  </th>
                  <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                    Mean CLV
                  </th>
                </tr>
              </thead>
              <tbody>
                {clv.data.by_league.map((league) => (
                  <tr key={league.league_id} className="border-b border-rule">
                    <th scope="row" className="t-mono px-2 py-2 text-left break-all text-ink">
                      {league.league_id}
                    </th>
                    <td className="t-mono px-2 py-2 text-right text-ink-2 tabular">
                      {league.counted}
                    </td>
                    <td className="t-mono px-2 py-2 text-right text-ink-2 tabular">
                      {formatFractionAsPercent(league.beat_rate)}
                    </td>
                    <td className="t-price-sm px-2 py-2 text-right text-ink tabular">
                      {formatSignedPercentPoints(league.mean_percent_clv)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </div>
  );
}

function CLVSummary({
  response,
  timeZone,
}: {
  readonly response: SchemaClvResponse;
  readonly timeZone: string;
}) {
  const { aggregate, window } = response;
  const measurable = aggregate.counted > 0;

  return (
    <section className="flex flex-col gap-3 rounded-card border border-rule bg-ground-1 p-4">
      <h2 className="t-label text-ink-muted">
        {`Summary · ${formatAbsolute(window.from, timeZone)} to ${formatAbsolute(window.to, timeZone)}`}
      </h2>

      <dl className="grid grid-cols-2 gap-4 md:grid-cols-4">
        <Fact term="Mean CLV">
          {/* Null, not zero. "No measurable wagers" and "measured, and it came
              out at zero" are different facts, and a 0.00% here would report the
              first as the second. */}
          {aggregate.mean_percent_clv == null
            ? NO_VALUE
            : formatSignedPercentPoints(aggregate.mean_percent_clv)}
        </Fact>
        <Fact term="Mean CLV (probability)">
          {aggregate.mean_probability_clv == null
            ? NO_VALUE
            : formatProbabilityPoints(aggregate.mean_probability_clv)}
        </Fact>
        <Fact term="Beat the close">
          {aggregate.beat_rate == null
            ? NO_VALUE
            : `${formatFractionAsPercent(aggregate.beat_rate)} (${String(aggregate.beat_count)}/${String(aggregate.counted)})`}
        </Fact>
        <Fact term="Legs counted">
          {`${String(aggregate.counted)} of ${String(aggregate.samples)}`}
        </Fact>
      </dl>

      {measurable ? null : (
        <p className="t-body text-ink-2">
          No measurable closing line value in this window. That is not a score of
          zero — it means nothing you have had graded here could be compared
          against a close.
        </p>
      )}

      {/* The exclusions, always visible. A mean over a filtered set is not
          auditable without knowing what was filtered out of it. */}
      <p className="t-mono text-ink-muted">
        {`excluded: ${String(aggregate.void_excluded)} void, ${String(aggregate.line_moved_excluded)} line moved`}
      </p>
      <p className="t-body text-ink-muted">
        A voided leg has no closing line to be compared against. A leg whose line
        moved — taken at −3, closed at −3.5 — is a different question, and
        converting between the two needs a model of game margins rather than
        arithmetic. Both are shown below and neither is counted. A push IS
        counted: it is a settlement outcome, not a data problem.
      </p>
    </section>
  );
}

function CLVRow({
  entry,
  format,
  timeZone,
}: {
  readonly entry: SchemaClvEntry;
  readonly format: ReturnType<typeof useOddsFormat>;
  readonly timeZone: string;
}) {
  const takenLine = formatMarketLine(entry.market_type, entry.taken_line);
  const closingLine = formatMarketLine(entry.market_type, entry.closing_line);
  const counted = !entry.voided && !entry.line_moved;

  return (
    <tr className="border-b border-rule">
      <th scope="row" className="px-2 py-2 text-left align-top">
        <span className="t-ui block text-ink">
          {marketTypeShortLabel(entry.market_type)}
          {takenLine === '' ? '' : ` ${takenLine}`}
        </span>
        <span className="t-mono block break-all text-ink-muted">
          {entry.selection_id}
        </span>
        <span className="t-mono block text-ink-muted">
          {`${entry.taken_book_id} → close ${entry.closing_book_id} · devig ${entry.devig_method}`}
        </span>
      </th>

      <td className="t-price-sm px-2 py-2 text-right align-top text-ink">
        {formatOdds(entry.taken_price, format)}
      </td>

      <td className="px-2 py-2 text-right align-top">
        <span className="t-price-sm block text-ink-2">
          {formatOdds(entry.closing_price, format)}
        </span>
        {/* Both lines, when they differ. This is the "you took −3, it closed
            −3.5" the exclusion exists to be able to show. */}
        {entry.line_moved && closingLine !== '' ? (
          <span className="t-mono block text-ink-muted">{`line ${closingLine}`}</span>
        ) : null}
      </td>

      <td className="px-2 py-2 text-right align-top">
        {/* Ink, never money: a CLV percentage is a price-quality score. */}
        <span className="t-price-sm block text-ink tabular">
          {formatSignedPercentPoints(entry.percent_clv)}
        </span>
        <span className="t-mono block text-ink-muted tabular">
          {formatProbabilityPoints(entry.probability_clv)}
        </span>
      </td>

      <td className="px-2 py-2 text-left align-top">
        <span className="flex flex-wrap gap-1">
          {counted ? (
            <Badge variant="neutral">
              {entry.beat_close ? 'beat close' : 'missed close'}
            </Badge>
          ) : null}
          {entry.line_moved ? <Badge variant="info">line moved</Badge> : null}
          {entry.voided ? <Badge variant="info">void</Badge> : null}
          <Badge variant="neutral">{entry.leg_status}</Badge>
        </span>
      </td>

      <td className="t-mono px-2 py-2 text-right align-top text-ink-muted">
        {formatAbsolute(entry.graded_at, timeZone)}
      </td>
    </tr>
  );
}

function Fact({
  term,
  children,
}: {
  readonly term: string;
  readonly children: React.ReactNode;
}) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="t-label text-ink-muted">{term}</dt>
      <dd className="t-price-sm text-ink tabular">{children}</dd>
    </div>
  );
}
