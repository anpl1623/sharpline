/**
 * The public leaderboard.
 *
 * A SERVER component with no client half at all. Nothing on it is interactive
 * beyond two links, it holds no state, and it is public — so shipping a
 * kilobyte of JavaScript to switch a sort that the server can serve directly
 * would be spending the reader's bandwidth on nothing.
 *
 * # It does not rank on profit, and the page says so in words
 *
 * CLAUDE.md §6: "a public leaderboard on ROI and CLV, not raw profit". The
 * omission is the design, so it is stated on the page rather than left to be
 * noticed — a reader who finds no profit column will otherwise assume it is an
 * oversight and trust the board less, not more.
 *
 * # Where the money/not-money line falls on this table, and why
 *
 * `staked_minor` and `net_return_minor` ARE money and are rendered as amounts.
 * `roi`, `roi_percent`, `beat_rate` and the two CLV means are NOT — they are
 * ratios, and a ratio in the treatment reserved for a currency amount is the
 * exact category error the phase 8 slip work had to be careful about.
 *
 * The money columns are nonetheless rendered in INK rather than in `money` and
 * `loss`. DESIGN.md permits green on a P&L figure; it does not require it, and
 * the slip's own decision of 2026-08-20 gives the argument for not spending it
 * here: green is a HIGHLIGHT, and a column of twenty-five highlighted figures
 * has highlighted nothing. The sign is carried by an explicit `+`/`−` from
 * `formatMinorSigned`, which is information rather than decoration and works for
 * a colourblind reader.
 */

import Link from 'next/link';

import { Badge } from '@/components/ui';
import type {
  SchemaLeaderboardBasis,
  SchemaLeaderboardPage,
} from '@/lib/api/schema';
import {
  formatFractionAsPercent,
  formatSignedPercentPoints,
} from '@/lib/analytics/format';
import { formatMinor, formatMinorSigned, MONEY_UNIT } from '@/lib/money';
import { cn } from '@/lib/utils';

export interface LeaderboardTableProps {
  readonly page: SchemaLeaderboardPage;
  /** `/leaderboard` — what the basis links point at. */
  readonly basePath: string;
}

const BASES: readonly {
  readonly id: SchemaLeaderboardBasis;
  readonly label: string;
  readonly blurb: string;
}[] = [
  {
    id: 'roi',
    label: 'Return on investment',
    blurb:
      'Net return over everything staked. Stake-normalised, so a customer who ' +
      'staked a fortune and lost cannot outrank one who staked little and won.',
  },
  {
    id: 'clv',
    label: 'Closing line value',
    blurb:
      'How much better than the closing line the prices taken were, measured ' +
      'on devigged prices. Scored against the market rather than the ' +
      'scoreboard, which makes it the better predictor over a short history.',
  },
];

export function LeaderboardTable({ page, basePath }: LeaderboardTableProps) {
  const active = BASES.find((basis) => basis.id === page.basis) ?? BASES[0];

  return (
    <div className="flex flex-col gap-4">
      <nav aria-label="Ranking basis" className="flex flex-wrap gap-1">
        {BASES.map((basis) => {
          const isActive = basis.id === page.basis;
          return (
            <Link
              key={basis.id}
              href={`${basePath}?basis=${basis.id}`}
              aria-current={isActive ? 'page' : undefined}
              className={cn(
                'inline-flex items-center border-b-2 px-2 py-2 whitespace-nowrap t-ui ui-transition',
                isActive
                  ? 'border-ink text-ink'
                  : 'border-transparent text-ink-muted hover:text-ink',
              )}
            >
              {basis.label}
            </Link>
          );
        })}
      </nav>

      <div className="flex flex-col gap-2 rounded-card border border-rule bg-ground-1 p-4">
        <p className="t-body text-ink-2">{active?.blurb}</p>
        <p className="t-body text-ink-muted">
          This board deliberately does not rank on profit. Raw profit rewards
          stake size and variance — the top of a profit board is whoever staked
          the most and got lucky — so the two measures here are a ratio and a
          price-quality score, and both are on every row whichever one is sorted.
        </p>
        {/* The sample floors, beside the rows rather than in a footnote. A
            ranking without its minimum sample cannot be audited: a reader has no
            way to know the top row is not one lucky maximum-stake bet. */}
        <p className="t-mono text-ink-muted">
          {`minimum ${String(page.minimum_samples.settled_wagers)} settled wagers and ${String(page.minimum_samples.clv_samples)} countable CLV legs to be ranked`}
        </p>
      </div>

      {page.data.length === 0 ? (
        <LeaderboardEmpty page={page} />
      ) : (
        <div className="board-scroll">
          <table className="w-full border-collapse">
            <caption className="sr-only">
              {`Customers ranked by ${active?.label ?? page.basis}. Amounts are play money; ROI and CLV are ratios, not amounts.`}
            </caption>
            <thead>
              <tr className="border-b border-rule">
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  #
                </th>
                <th scope="col" className="t-label px-2 py-2 text-left text-ink-muted">
                  Player
                </th>
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  ROI
                </th>
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  Mean CLV
                </th>
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  Beat close
                </th>
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  Settled
                </th>
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  {`Staked (${MONEY_UNIT})`}
                </th>
                <th scope="col" className="t-label px-2 py-2 text-right text-ink-muted">
                  {`Net (${MONEY_UNIT})`}
                </th>
              </tr>
            </thead>
            <tbody>
              {page.data.map((row) => (
                <tr key={row.user} className="border-b border-rule">
                  <td className="t-mono px-2 py-2 text-right text-ink-muted tabular">
                    {row.rank}
                  </td>
                  <th scope="row" className="px-2 py-2 text-left">
                    {/* A derived pseudonym, not a name: this system stores no
                        display name, so a real identity here would be a
                        published email address. */}
                    <span className="t-mono text-ink">{row.user}</span>
                  </th>
                  <td className="t-price-sm px-2 py-2 text-right text-ink tabular">
                    {formatSignedPercentPoints(row.roi_percent)}
                  </td>
                  <td className="t-price-sm px-2 py-2 text-right text-ink tabular">
                    {formatSignedPercentPoints(row.mean_percent_clv)}
                  </td>
                  <td className="px-2 py-2 text-right align-top">
                    <span className="t-mono block text-ink-2 tabular">
                      {formatFractionAsPercent(row.beat_rate)}
                    </span>
                    <span className="t-mono block text-ink-muted tabular">
                      {`${String(row.beat_count)}/${String(row.clv_samples)}`}
                    </span>
                  </td>
                  <td className="t-mono px-2 py-2 text-right text-ink-2 tabular">
                    {row.settled_wagers}
                  </td>
                  {/* Money, and rendered as ink: see the file comment on why the
                      green is not spent on a column. */}
                  <td className="t-mono px-2 py-2 text-right text-ink-2 tabular">
                    {formatMinor(row.staked_minor)}
                  </td>
                  <td className="t-mono px-2 py-2 text-right text-ink tabular">
                    {formatMinorSigned(row.net_return_minor)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

/**
 * Nobody has met the sample floor yet.
 *
 * This is a CORRECT state and is the state of every fresh deployment: it takes
 * settled wagers and graded legs to be ranked, and neither exists until somebody
 * has bet and something has finished. There is no placeholder row, no greyed
 * example, and no "you could be here" — a fabricated leaderboard entry is
 * indistinguishable from a real one in a screenshot.
 */
function LeaderboardEmpty({ page }: { readonly page: SchemaLeaderboardPage }) {
  return (
    <div className="flex max-w-prose flex-col items-start gap-3 rounded-card border border-rule bg-ground-1 p-6">
      <h2 className="t-h3 text-ink">Nobody is ranked yet</h2>
      <p className="t-body text-ink-2">
        A customer appears here once they have{' '}
        {page.minimum_samples.settled_wagers} settled wagers and{' '}
        {page.minimum_samples.clv_samples} closing-line-value legs inside this
        window. Both floors exist so that one lucky maximum-stake bet cannot
        reach the top of the board.
      </p>
      <p className="t-body text-ink-muted">
        Nothing is seeded here. An empty board means the thresholds have not been
        met, not that the ranking is broken.
      </p>
      <Badge variant="neutral">play money · simulation</Badge>
    </div>
  );
}
