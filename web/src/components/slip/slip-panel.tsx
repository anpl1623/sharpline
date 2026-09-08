'use client';

/**
 * The bet slip's contents — the same component in the desktop rail and in the
 * mobile sheet.
 *
 * ONE implementation, two containers. The alternative — a compact mobile variant
 * — was rejected on the same grounds DESIGN.md rejects card-per-event for the
 * mobile board: a slip that shows fewer facts on a phone is a slip that asks
 * somebody to commit money with less information, and the small screen is where
 * that matters most. What changes between the two is the container's chrome, not
 * the panel.
 *
 * # The button is off until FOUR separate things are true
 *
 * They are enumerated in `placeBlocker` below rather than folded into one
 * boolean, because a disabled button with no reason is the most frustrating
 * control in any betting interface, and each of these has a different fix:
 *
 *   1. The slip is describable    — a stake, sizes on a round robin, points on a
 *                                   teaser. Known here, from the slip alone.
 *   2. Its price is known         — the quote for THIS slip has arrived. Placing
 *                                   against a quote for a different slip is
 *                                   placing at a number nobody was shown.
 *   3. No leg has moved           — the WebSocket comparison. Blocks until the
 *                                   customer accepts or removes.
 *   4. The book has no objection  — the quote's impediments, ADVISORY, re-decided
 *                                   inside the placement transaction.
 *
 * # A refusal from the server is rendered verbatim
 *
 * `Error.message` is a fixed human-readable string from a closed set in Go, and
 * every refusal this panel can meet — a suspended market, a same-game ticket
 * this deployment will not price, a teaser with no posted ladder, a
 * self-imposed limit — arrives that way. None of them is paraphrased here.
 * Rewriting them client-side would put a second vocabulary in front of the
 * customer that drifts from the one in the logs, and in two of those cases it
 * would mean this frontend asserting something about the server's configuration
 * that only the server knows.
 */

import Link from 'next/link';
import { usePathname } from 'next/navigation';

import { signInHref } from '@/components/auth/auth-card';
import { Badge, Button } from '@/components/ui';
import { userFacingMessage } from '@/lib/api/errors';
import type { ApiError } from '@/lib/api/errors';
import type { SchemaSlipQuote } from '@/lib/api/schema';
import { isUsableIdempotencyKey } from '@/lib/betting/idempotency';
import { formatOdds } from '@/lib/odds/format';
import { useIsAuthenticated } from '@/lib/store/auth';
import { useOddsFormat } from '@/lib/store/preferences';
import {
  legEffectiveLine,
  slipActions,
  useSlip,
  useSlipHydration,
} from '@/lib/store/slip';
import type { SlipLeg } from '@/lib/store/slip';
import { SlipEmpty, SlipUnread } from './slip-empty';
import { SlipLegRow } from './slip-leg-row';
import { SlipReceiptPanel } from './slip-receipt';
import { SlipStakeField } from './slip-stake-field';
import { SlipSummary } from './slip-summary';
import {
  RoundRobinControl,
  SlipKindControl,
  TeaserControl,
} from './slip-ticket-controls';
import { useSlipChannels, useSlipWatches, watchBlocks } from './slip-model';
import type { LegWatch } from './slip-model';
import { usePlaceWager } from './use-place-wager';
import { useSlipQuote } from './use-slip-quote';

export interface SlipPanelProps {
  /** Rendered by the sheet, which supplies its own heading. */
  readonly headless?: boolean | undefined;
}

export function SlipPanel({ headless = false }: SlipPanelProps) {
  const hydrated = useSlipHydration();

  const legs = useSlip((state) => state.legs);
  const kind = useSlip((state) => state.kind);
  const stakeMinor = useSlip((state) => state.stakeMinor);
  const roundRobinSizes = useSlip((state) => state.roundRobinSizes);
  const teaserPoints = useSlip((state) => state.teaserPoints);
  const acceptBetterPrice = useSlip((state) => state.acceptBetterPrice);
  const acceptedTicketDecimal = useSlip(
    (state) => state.acceptedTicketDecimal,
  );
  const attemptKey = useSlip((state) => state.attemptKey);
  const notice = useSlip((state) => state.notice);
  const receipt = useSlip((state) => state.receipt);

  const authenticated = useIsAuthenticated();
  const pathname = usePathname();

  // Held for as long as the slip holds the leg, on the MARKET channel — so a
  // slip keeps watching its own prices after the customer navigates away from
  // the board that built it.
  useSlipChannels(legs);
  const watches = useSlipWatches(legs);

  const requestState = {
    legs,
    kind,
    stakeMinor,
    roundRobinSizes,
    teaserPoints,
    acceptBetterPrice,
    acceptedTicketDecimal,
  };

  const { quote, error: quoteError, settling } = useSlipQuote(requestState);
  const placement = usePlaceWager(quote);

  if (!hydrated) return <SlipUnread />;

  if (receipt !== null) {
    return (
      <SlipReceiptPanel
        placement={receipt}
        onDismiss={() => {
          slipActions().dismissReceipt();
        }}
      />
    );
  }

  if (legs.length === 0) {
    return (
      <>
        {headless ? null : <SlipHeader count={0} />}
        <SlipEmpty />
      </>
    );
  }

  const movedCount = watches.filter((watch) =>
    watchBlocks(watch, acceptBetterPrice),
  ).length;
  const blocked = movedCount > 0;
  const blocker = placeBlocker({
    kind,
    stakeMinor,
    roundRobinSizes,
    teaserPoints,
    quote,
    settling,
    blocked,
    attemptKey,
  });

  return (
    <>
      {headless ? null : <SlipHeader count={legs.length} />}

      <MoveAnnouncer movedCount={movedCount} />

      <div className="min-h-0 flex-1 overflow-y-auto">
        <SlipKindControl
          kind={kind}
          legs={legs.map((leg) => ({
            eventId: leg.eventId,
            marketType: leg.marketType,
            line: legEffectiveLine(leg),
          }))}
          onSelect={(next) => {
            placement.reset();
            slipActions().setKind(next);
          }}
        />

        <ul className="border-t border-rule">
          {legs.map((leg, index) => (
            <SlipLegRow
              key={leg.selectionId}
              leg={leg}
              watch={watches[index] ?? emptyWatch(leg)}
              acceptBetterPrice={acceptBetterPrice}
              onRemove={(selectionId) => {
                placement.reset();
                slipActions().remove(selectionId);
              }}
              onAccept={(selectionId, decimal, line) => {
                placement.reset();
                slipActions().acceptLeg(selectionId, decimal, line);
              }}
            />
          ))}
        </ul>

        {kind === 'round_robin' ? (
          <RoundRobinControl
            legCount={legs.length}
            sizes={roundRobinSizes}
            onToggleSize={(size) => {
              placement.reset();
              slipActions().toggleRoundRobinSize(size);
            }}
          />
        ) : null}

        {kind === 'teaser' ? (
          <TeaserControl
            points={teaserPoints}
            onChange={(points) => {
              placement.reset();
              slipActions().setTeaserPoints(points);
            }}
          />
        ) : null}
      </div>

      <div className="shrink-0 border-t border-rule">
        <SlipStakeField
          stakeMinor={stakeMinor}
          perTicket={kind === 'round_robin'}
          cashBalanceMinor={quote?.cash_balance_minor ?? null}
          disabled={placement.isPending}
          onChange={(minor) => {
            placement.reset();
            slipActions().setStakeMinor(minor);
          }}
        />

        {quote === undefined ? null : (
          <SlipSummary quote={quote} settling={settling} />
        )}

        <SameGameNote quote={quote} />
        <Impediments quote={quote} error={quoteError} />

        {notice === null ? null : (
          <p className="px-3 pb-2 t-ui text-ink-2">{notice}</p>
        )}

        <TicketMoveNotice
          error={placement.error}
          acceptedTicketDecimal={acceptedTicketDecimal}
        />

        <PlacementError error={placement.error} />

        <BetterPriceToggle
          value={acceptBetterPrice}
          disabled={placement.isPending}
          onChange={(next) => {
            slipActions().setAcceptBetterPrice(next);
          }}
        />

        <div className="flex flex-col gap-2 px-3 pb-3">
          {authenticated ? (
            <>
              <Button
                type="button"
                variant="primary"
                disabled={blocker !== null || placement.isPending}
                onClick={placement.place}
              >
                {placement.isPending ? 'Placing…' : 'Place bet'}
              </Button>
              {/* NOT a live region. It changes on a stake keystroke and while a
                  quote is in flight, and announcing "Pricing this slip…" on
                  every character typed would be the per-tick chatter DESIGN.md
                  bans, in a different costume. The one thing here that MUST be
                  announced — a price moving — has its own region above, which
                  changes only when the set of moved legs does. */}
              {blocker === null ? null : (
                <p className="t-ui text-ink-muted">{blocker}</p>
              )}
            </>
          ) : (
            <>
              {/* Not a disabled Place button with a tooltip. A signed-out
                  customer is one click from being able to bet, and the control
                  should be that click rather than an explanation of why the
                  other control does not work. */}
              <Button asChild variant="primary">
                <Link href={signInHref(pathname)}>Sign in to place</Link>
              </Button>
              <p className="t-ui text-ink-muted">
                The slip is kept while you sign in.
              </p>
            </>
          )}

          <Button
            type="button"
            variant="ghost"
            size="sm"
            disabled={placement.isPending}
            onClick={() => {
              placement.reset();
              slipActions().clear();
            }}
          >
            Clear slip
          </Button>
        </div>
      </div>
    </>
  );
}

// -----------------------------------------------------------------------------
// Announcing a move
// -----------------------------------------------------------------------------

/**
 * The one thing on this surface that is PUSHED to a screen reader.
 *
 * The price-change interstitial has to be announced — a keyboard user whose
 * Place button silently goes dead has no way to discover why — but it must not
 * be announced per tick, and this is how those two requirements are both met:
 * the region's text is a FUNCTION OF HOW MANY LEGS ARE BLOCKING, so a market
 * that moves the same leg forty times in a minute produces one announcement, and
 * a leg that moves back to where it was takes it away again.
 *
 * It is not a second copy of the app-wide market announcer in
 * `components/live/live-announcer.tsx`. That one reports the BOARD moving,
 * batched to one sentence every five seconds, and is deliberately vague ("14
 * markets moved"). This one reports that a decision the customer is in the
 * middle of has been invalidated, which is specific, actionable and rare — and
 * it is silent unless something is actually blocking.
 *
 * The panel is mounted twice below 1000px (rail hidden, sheet live). Only ONE of
 * them is in the accessibility tree at a time: `display: none` removes the rail
 * from it entirely, and the sheet exists only while it is open.
 */
function MoveAnnouncer({ movedCount }: { readonly movedCount: number }) {
  return (
    <div className="sr-only" aria-live="polite" aria-atomic="true">
      {movedCount === 0
        ? null
        : movedCount === 1
          ? 'A price on your slip moved. Take the new price or remove the leg.'
          : `${String(movedCount)} prices on your slip moved. Take the new prices or remove the legs.`}
    </div>
  );
}

// -----------------------------------------------------------------------------
// Header
// -----------------------------------------------------------------------------

function SlipHeader({ count }: { readonly count: number }) {
  return (
    <div className="flex shrink-0 items-center justify-between gap-2 border-b border-rule px-3 py-2">
      <h2 className="t-h3 text-ink">Bet slip</h2>
      <span className="t-label text-ink-muted">
        {count === 1 ? '1 selection' : `${String(count)} selections`}
      </span>
    </div>
  );
}

// -----------------------------------------------------------------------------
// Refusals
// -----------------------------------------------------------------------------

/**
 * Every leg is on one event.
 *
 * Read from the SERVER's `is_same_game` and not computed here, even though the
 * slip plainly knows every leg's event id. The reason is that the field is not
 * really answering "are these the same event" — it is answering "is this the
 * shape that gets priced with a correlation adjustment", and that is the
 * pricer's judgement rather than a string comparison. Recomputing it client-side
 * would be a second definition of a term the server owns.
 *
 * It is worth surfacing because a same-game ticket is NOT the product of its leg
 * prices. A client that multiplied the legs itself would get a different, larger
 * number and would look right — so this note is here partly for the customer and
 * partly as the standing explanation of why this file never multiplies anything.
 */
function SameGameNote({ quote }: { readonly quote: SchemaSlipQuote | undefined }) {
  if (quote === undefined || quote.is_same_game !== true) return null;

  return (
    <p className="px-3 pb-2 t-ui text-ink-2">
      Every leg is on one event, so this ticket is priced with a correlation
      adjustment rather than as independent legs.
    </p>
  );
}

/**
 * The quote's impediments, and a quote that could not be produced at all.
 *
 * Both render the SERVER's own message. `SlipImpediment.message` is documented
 * as "a fixed human-readable string from a closed set in Go, exactly as
 * `Error.message` is", and a `409` on the quote path means the slip cannot be
 * priced — a market suspended, an event no longer accepting wagers, or no book
 * quoting a leg — with no partial quote worth returning.
 *
 * `limit_exceeded` is deliberately absent from the impediment vocabulary and
 * will never appear here: evaluating a self-imposed limit is a period-scoped sum
 * over the ledger taken under the placement lock, and a second evaluation on a
 * read path would be a second answer to a responsible-gaming control. It arrives
 * at the button instead, as a `422`, and `PlacementError` renders it.
 */
function Impediments({
  quote,
  error,
}: {
  readonly quote: SchemaSlipQuote | undefined;
  readonly error: unknown;
}) {
  const impediments = quote?.impediments ?? [];

  if (error === null || error === undefined) {
    if (impediments.length === 0) return null;
    return (
      <ul className="flex flex-col gap-1 px-3 pb-2">
        {impediments.map((impediment, index) => (
          <li
            key={`${impediment.code}-${impediment.selection_id ?? String(index)}`}
            className="t-ui text-ink-2"
          >
            {impediment.message}
          </li>
        ))}
      </ul>
    );
  }

  return (
    <p className="px-3 pb-2 t-ui text-ink-2" role="status">
      {userFacingMessage(error)}
    </p>
  );
}

/**
 * A ticket-level price move, which is a different fact from a leg's.
 *
 * A parlay's ticket price is not any leg's price — and with correlated legs it
 * is not even their product — so it can move while every leg on screen is
 * unchanged. Accepting it is its own act with its own control, exactly as a
 * leg's is.
 */
function TicketMoveNotice({
  error,
  acceptedTicketDecimal,
}: {
  readonly error: ApiError | null;
  readonly acceptedTicketDecimal: number | null;
}) {
  const oddsFormat = useOddsFormat();

  if (error === null || !error.isPriceMoved) return null;
  const move = error.priceMoves.find((entry) => entry.scope === 'ticket');
  if (move === undefined) return null;

  const current = move.current_decimal ?? null;
  if (current === null) return null;
  if (acceptedTicketDecimal === current) return null;

  const seen = move.seen_decimal ?? null;

  return (
    <div className="mx-3 mb-2 flex flex-col gap-2 rounded-price border border-rule-hi bg-ground-2 p-2">
      <Badge variant="neutral">Ticket price moved</Badge>
      <p className="t-ui text-ink-2">
        {/* PRICES. Not tinted, on the receipt, on the slip or here. */}
        {seen === null ? null : (
          <>
            <span className="t-price-sm text-ink-muted">
              {formatOdds(seen, oddsFormat)}
            </span>
            {' → '}
          </>
        )}
        <span className="t-price-sm text-ink">
          {formatOdds(current, oddsFormat)}
        </span>
      </p>
      <div>
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={() => {
            slipActions().acceptTicket(current);
          }}
        >
          Take {formatOdds(current, oddsFormat)}
        </Button>
      </div>
    </div>
  );
}

/**
 * A refusal from `POST /wagers`, rendered as the server phrased it.
 *
 * A `409 price_moved` is NOT shown here — it is not a fault, it is the market
 * doing its job, and it is already on screen as the leg and ticket
 * interstitials. Everything else is: `403 self_excluded` and
 * `403 account_not_active`, `422 insufficient_funds`, `422 limit_exceeded` with
 * the limit's kind and period in `invalid_params`, `422 unprocessable` for a
 * ticket shape this deployment does not price.
 *
 * `loss` is the right hue for these and the price rule is not being bent: none
 * of them is a price. They are the "something is wrong" case the token exists
 * for.
 */
function PlacementError({ error }: { readonly error: ApiError | null }) {
  if (error === null) return null;
  if (error.isPriceMoved) return null;

  return (
    <div
      className="mx-3 mb-2 flex flex-col gap-1 rounded-price border border-loss/40 bg-loss/8 p-2"
      role="alert"
    >
      <p className="t-ui text-ink">{error.message}</p>
      {error.invalidParams.length === 0 ? null : (
        <ul className="flex flex-col gap-0.5">
          {error.invalidParams.map((param) => (
            <li key={param.name} className="t-mono text-ink-muted">
              {param.name}: {param.reason}
            </li>
          ))}
        </ul>
      )}
      {error.requestId === null ? null : (
        <p className="t-mono text-ink-faint">request {error.requestId}</p>
      )}
    </div>
  );
}

// -----------------------------------------------------------------------------
// Standing consent
// -----------------------------------------------------------------------------

/**
 * "Book a better price without asking."
 *
 * A checkbox rather than a default, which is a real divergence from every book
 * in the category. The API's own note gives the reason and it is worth keeping
 * in front of a reader: "accept when the new price is longer" and "accept when
 * the new price is shorter" are one comparison operator apart, and the
 * difference between them is invisible in review and invisible in every test
 * where the line does not move. So the concession is explicit and it is on the
 * slip, where a reader can see the customer asked for it.
 *
 * It NEVER covers a shorter price and NEVER covers a line move.
 */
function BetterPriceToggle({
  value,
  disabled,
  onChange,
}: {
  readonly value: boolean;
  readonly disabled: boolean;
  readonly onChange: (next: boolean) => void;
}) {
  return (
    <label className="flex cursor-pointer items-start gap-2 px-3 pb-2">
      <input
        type="checkbox"
        checked={value}
        disabled={disabled}
        onChange={(event) => {
          onChange(event.target.checked);
        }}
        className="mt-0.5 size-4 shrink-0 accent-[var(--color-money)]"
      />
      <span className="t-ui text-ink-2">
        Take a longer price without asking
        <span className="block t-label text-ink-muted">
          Never a shorter price, and never across a line move.
        </span>
      </span>
    </label>
  );
}

// -----------------------------------------------------------------------------
// The blocker
// -----------------------------------------------------------------------------

interface BlockerInput {
  readonly kind: string;
  readonly stakeMinor: number;
  readonly roundRobinSizes: readonly number[];
  readonly teaserPoints: number | null;
  readonly quote: SchemaSlipQuote | undefined;
  readonly settling: boolean;
  readonly blocked: boolean;
  readonly attemptKey: string;
}

/**
 * The one reason the button is off, or null when it is on.
 *
 * Ordered by what the customer should fix FIRST, not by which check is cheapest:
 * a slip with no stake and a moved leg is told about the stake, because that is
 * the step they are in the middle of. Returning one sentence rather than a list
 * is deliberate — a control that reports four problems at once reads as broken.
 */
function placeBlocker(input: BlockerInput): string | null {
  if (input.stakeMinor <= 0) return 'Enter a stake.';

  if (input.kind === 'round_robin' && input.roundRobinSizes.length === 0) {
    return 'Choose at least one combination size.';
  }
  if (input.kind === 'teaser' && input.teaserPoints === null) {
    return 'Set the teaser points.';
  }

  if (input.blocked) {
    return 'A price moved. Take the new one or remove the leg.';
  }

  if (input.settling || input.quote === undefined) {
    return 'Pricing this slip…';
  }

  if (!input.quote.placeable) {
    // The impediment list above already says WHICH, in the server's words. This
    // only explains the disabled button, and repeating the reason here would put
    // the same sentence on screen twice.
    return 'This slip cannot be placed right now.';
  }

  if (!isUsableIdempotencyKey(input.attemptKey)) {
    return 'Preparing this slip…';
  }

  return null;
}

/** A leg the watch array has not caught up with. Reports "unchanged". */
function emptyWatch(leg: SlipLeg): LegWatch {
  return {
    selectionId: leg.selectionId,
    currentDecimal: null,
    currentLine: null,
    movement: 'unchanged',
    lineMoved: false,
    improved: false,
    moved: false,
  };
}
