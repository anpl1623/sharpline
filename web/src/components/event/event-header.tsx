'use client';

import Link from 'next/link';

import {
  ALL_LEAGUES_HREF,
  leagueBoardHref,
} from '@/components/layout/league-nav';
import { Badge } from '@/components/ui';
import { useLocalTimeZone } from '@/components/event/use-event-live';
import type {
  SchemaCompetitor,
  SchemaEventStatus,
  SchemaEventSummary,
  SchemaLeague,
  SchemaSport,
} from '@/lib/api/schema';
import {
  formatAbsolute,
  formatCompactDuration,
  formatDayAndTime,
  formatDurationWords,
  secondsBetween,
  toDateTimeAttribute,
} from '@/lib/time';

/**
 * The event's identity: where it sits in the catalogue, who is playing, when it
 * starts, and what state it is in.
 *
 * # Nothing here is invented
 *
 * `clock` and `score` are optional on `EventSummary` and are absent on this
 * feed today. An absent score renders NOTHING — not "0 – 0", which is a
 * fabricated fact that happens to be plausible, and therefore the worst kind.
 * Same for the clock, and for a competitor on an `outright` event, which has no
 * home or away side at all rather than empty ones.
 *
 * # "How long until kickoff" is measured against the payload, not the browser
 *
 * `as_of` is the instant the server assembled this page. Measuring the countdown
 * against it rather than against `Date.now()` means a skewed browser clock
 * cannot make a game that starts in an hour say it started yesterday, and it
 * means the countdown is consistent with every staleness figure elsewhere on the
 * page, which is anchored the same way.
 */

interface EventHeaderProps {
  readonly sport: SchemaSport;
  readonly league: SchemaLeague;
  readonly event: SchemaEventSummary;
  /** The server's assembly instant. The anchor for every relative time here. */
  readonly asOf: string;
}

const STATUS_LABEL: Record<SchemaEventStatus, string> = {
  scheduled: 'Scheduled',
  live: 'Live',
  suspended: 'Suspended',
  ended: 'Ended',
  settled: 'Settled',
  postponed: 'Postponed',
  cancelled: 'Cancelled',
};

/**
 * `info` is DESIGN.md's "neutral system state / suspension" hue, and it is the
 * right one for both `live` and `suspended`: neither is money and neither is an
 * error. `cancelled` is the only status that is genuinely a failure. Nothing
 * here relies on colour — the word is in the badge.
 */
function statusVariant(
  status: SchemaEventStatus,
): 'neutral' | 'info' | 'loss' {
  switch (status) {
    case 'live':
    case 'suspended':
      return 'info';
    case 'cancelled':
      return 'loss';
    case 'scheduled':
    case 'ended':
    case 'settled':
    case 'postponed':
      return 'neutral';
    default:
      return 'neutral';
  }
}

function CompetitorLine({
  role,
  competitor,
}: {
  readonly role: string;
  readonly competitor: SchemaCompetitor;
}) {
  return (
    <div className="flex items-baseline gap-2">
      <span className="t-label w-12 shrink-0 text-ink-muted">{role}</span>
      <span className="t-h3 text-ink">{competitor.name}</span>
    </div>
  );
}

export function EventHeader({ sport, league, event, asOf }: EventHeaderProps) {
  const timeZone = useLocalTimeZone();

  const untilStart = secondsBetween(asOf, event.scheduled_start);
  const relative =
    untilStart === null
      ? null
      : untilStart >= 0
        ? {
            short: `in ${formatCompactDuration(untilStart)}`,
            words: `starts in ${formatDurationWords(untilStart)}`,
          }
        : {
            short: `${formatCompactDuration(untilStart)} ago`,
            words: `started ${formatDurationWords(untilStart)} ago`,
          };

  const home = event.home_competitor;
  const away = event.away_competitor;
  const score = event.score;
  const clock = event.clock;

  return (
    <header className="flex flex-col gap-6 border-b border-rule pb-6">
      <nav aria-label="Breadcrumb">
        <ol className="flex flex-wrap items-center gap-2 t-ui text-ink-muted">
          <li>
            <Link href={ALL_LEAGUES_HREF} className="ui-transition hover:text-ink">
              Board
            </Link>
          </li>
          <li aria-hidden="true">/</li>
          <li>{sport.name}</li>
          <li aria-hidden="true">/</li>
          <li>
            {/* The same helper the header's league nav uses, so the
                breadcrumb and the nav can never disagree about the route. */}
            <Link
              href={leagueBoardHref(league.slug)}
              className="ui-transition hover:text-ink"
            >
              {league.name}
            </Link>
          </li>
          <li aria-hidden="true">/</li>
          <li aria-current="page" className="text-ink-2">
            {event.name}
          </li>
        </ol>
      </nav>

      <div className="flex flex-col gap-4">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="flex flex-col gap-2">
            <h1 className="t-h1 text-ink">{event.name}</h1>

            {/* An outright has no sides. Rendering an empty pair would be an
                assertion that two competitors exist. */}
            {home !== undefined || away !== undefined ? (
              <div className="flex flex-col gap-1 pt-2">
                {away !== undefined ? (
                  <CompetitorLine role="Away" competitor={away} />
                ) : null}
                {home !== undefined ? (
                  <CompetitorLine role="Home" competitor={home} />
                ) : null}
              </div>
            ) : null}
          </div>

          <div className="flex flex-col items-start gap-2 sm:items-end">
            <Badge variant={statusVariant(event.status)}>
              {STATUS_LABEL[event.status]}
            </Badge>

            <p className="t-body text-ink-2">
              <time dateTime={toDateTimeAttribute(event.scheduled_start)}>
                {formatDayAndTime(event.scheduled_start, timeZone)}
              </time>
              {relative === null ? null : (
                <>
                  {' · '}
                  <span title={relative.words}>
                    <span aria-hidden="true">{relative.short}</span>
                    <span className="sr-only">{relative.words}</span>
                  </span>
                </>
              )}
            </p>
          </div>
        </div>

        {/* Live state, rendered only where the payload actually carries it. */}
        {score !== undefined || clock !== undefined ? (
          <div className="flex flex-wrap items-center gap-4 rounded-card border border-rule bg-ground-1 px-4 py-3">
            {score !== undefined ? (
              <dl className="flex items-baseline gap-4">
                <div className="flex items-baseline gap-2">
                  <dt className="t-label text-ink-muted">
                    {away?.name ?? 'Away'}
                  </dt>
                  <dd className="t-price-lg text-ink">{score.away}</dd>
                </div>
                <div className="flex items-baseline gap-2">
                  <dt className="t-label text-ink-muted">
                    {home?.name ?? 'Home'}
                  </dt>
                  <dd className="t-price-lg text-ink">{score.home}</dd>
                </div>
              </dl>
            ) : null}

            {clock !== undefined ? (
              <p className="t-mono text-ink-muted">
                {[
                  clock.period === null || clock.period === undefined
                    ? null
                    : `period ${String(clock.period)}`,
                  clock.elapsed_seconds === null ||
                  clock.elapsed_seconds === undefined
                    ? null
                    : `elapsed ${formatCompactDuration(clock.elapsed_seconds)}`,
                  clock.running ? 'clock running' : 'clock stopped',
                ]
                  .filter((part): part is string => part !== null)
                  .join(' · ')}
              </p>
            ) : null}
          </div>
        ) : null}

        {/* The engineering register: what this page is anchored to. Every
            staleness figure below is measured against this instant. */}
        <p className="t-mono text-ink-muted">
          {`event ${event.id} · assembled ${formatAbsolute(asOf, timeZone)} · observed ${formatAbsolute(event.observed_at, timeZone)}`}
        </p>
      </div>
    </header>
  );
}
