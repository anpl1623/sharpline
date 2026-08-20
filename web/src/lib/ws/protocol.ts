/**
 * The `sharpline.v1` wire protocol, hand-written against the Go source.
 *
 * These types mirror `internal/wsgw/protocol.go` (the frames) and
 * `internal/pricing/payload.go` (the market document the frames carry). They are
 * hand-written rather than generated because the WebSocket protocol has no
 * OpenAPI document to generate from; the file comments name the Go declaration
 * each type corresponds to so a schema bump has an obvious landing site here.
 *
 * # Every frame carries a monotonic per-connection sequence number
 *
 * `seq` is a uint64 starting at 1 and advancing for EVERY frame the server puts
 * on the connection — hello, ack, snapshot, delta, resync, error and pong alike.
 * It is assigned AT ENQUEUE, not at write, and that is the whole point: when the
 * gateway discards a slow client's pending buffer those frames have already
 * consumed numbers, so the discard shows up on the wire as a VISIBLE GAP
 * (seq 4 followed by seq 41) rather than as a silent hole in the odds board.
 * Detecting that gap is the client's obligation; see `ws/client.ts`.
 *
 * # There is no runtime schema validator here, deliberately
 *
 * No zod, no io-ts. Narrowing is done with the guards below, and a frame that
 * does not parse is DROPPED AND COUNTED rather than thrown into React. A parse
 * error on one frame must not take down a board that is otherwise rendering
 * correctly, and an exception escaping a socket message handler does exactly
 * that.
 */

/** The negotiated subprotocol. `wsgw.Protocol`. */
export const WS_PROTOCOL = 'sharpline.v1';

/**
 * The bearer-credential subprotocol prefix. `wsgw.BearerPrefix`.
 *
 * The credential rides in the RFC 6455 subprotocol offer because that is the
 * ONLY request header a browser's `new WebSocket()` constructor can set — there
 * is no place to put an `Authorization` header. Unlike a query parameter it is
 * not part of the URL, so it never lands in an access log, a `Referer`, browser
 * history, or a link somebody pastes into a chat.
 *
 * A token in the query string is REFUSED by the server as a distinct outcome,
 * not silently ignored.
 */
export const WS_BEARER_PREFIX = 'sharpline.bearer.';

// -----------------------------------------------------------------------------
// Closed sets — wsgw.MessageKind, ClientKind, ErrorCode, ResyncReason, RejectReason
// -----------------------------------------------------------------------------

export type ServerFrameType =
  | 'hello'
  | 'ack'
  | 'snapshot'
  | 'delta'
  | 'resync'
  | 'error'
  | 'pong';

export type ClientFrameType = 'subscribe' | 'unsubscribe' | 'resync' | 'ping';

/** Why ONE channel in a subscribe request was refused. `wsgw.RejectReason`. */
export type RejectReason =
  | 'malformed'
  | 'unknown_kind'
  | 'invalid_id'
  | 'too_long'
  | 'limit_reached'
  | 'duplicate';

/** Why the server told the client its stream has a hole. `wsgw.ResyncReason`. */
export type ResyncReason =
  | 'slow_consumer'
  | 'client_requested'
  | 'presence_lost';

/** The bounded classification on an error frame. `wsgw.ErrorCode`. */
export type WsErrorCode =
  | 'malformed_frame'
  | 'unknown_type'
  | 'frame_too_large'
  | 'invalid_channel'
  | 'channel_limit'
  | 'unauthorized'
  | 'going_away'
  | 'internal';

/** `wsgw.ChannelKind`. */
export type ChannelKind = 'event' | 'market' | 'league';

const SERVER_FRAME_TYPES: readonly string[] = [
  'hello',
  'ack',
  'snapshot',
  'delta',
  'resync',
  'error',
  'pong',
];

// -----------------------------------------------------------------------------
// Channels — wsgw/channel.go
// -----------------------------------------------------------------------------

/**
 * A channel name: `event:{id}`, `market:{id}` or `league:{slug}`.
 *
 * LEAGUE IS KEYED BY SLUG, not by id, deliberately: the board's URL and its
 * subscription are then the same string, and a page can subscribe from its route
 * parameter with no lookup.
 */
export type Channel = string;

export function eventChannel(eventId: string): Channel {
  return `event:${eventId}`;
}

export function marketChannel(marketId: string): Channel {
  return `market:${marketId}`;
}

export function leagueChannel(leagueSlug: string): Channel {
  return `league:${leagueSlug}`;
}

/**
 * Splits a channel into its kind and identifier, or `null` if it is not one.
 *
 * `:` is excluded from the identifier charset upstream (`domain.validID`)
 * precisely so this split is unambiguous, so the FIRST colon is the separator
 * and there cannot be a second.
 */
export function parseChannel(
  channel: string,
): { readonly kind: ChannelKind; readonly id: string } | null {
  const separator = channel.indexOf(':');
  if (separator <= 0) return null;
  const kind = channel.slice(0, separator);
  const id = channel.slice(separator + 1);
  if (id === '') return null;
  if (kind !== 'event' && kind !== 'market' && kind !== 'league') return null;
  return { kind, id };
}

// -----------------------------------------------------------------------------
// The market payload — internal/pricing/payload.go
// -----------------------------------------------------------------------------

/** `pricing.Margin`. All three quantities are distinct and travel together. */
export interface Margin {
  /** n, the number of prices the book quoted. */
  readonly selections: number;
  /** S = sum(1/d). */
  readonly implied_sum: number;
  /** 100*S — the "105% book". The only field that is a percentage. */
  readonly booking_percentage: number;
  /** S - 1, as a fraction. */
  readonly overround: number;
  /** (S-1)/S, as a fraction: the hold on a balanced book. */
  readonly vig: number;
}

/** `normalizer.SportRef`. */
export interface SportRef {
  readonly id: string;
  readonly slug: string;
  readonly name: string;
}

/** `normalizer.LeagueRef`. */
export interface LeagueRef {
  readonly id: string;
  readonly sport_id: string;
  readonly slug: string;
  readonly name: string;
}

/**
 * `normalizer.CompetitorRef`. `id` is optional — providers frequently supply a
 * display name and nothing else.
 */
export interface CompetitorRef {
  readonly id?: string;
  readonly name: string;
}

export type EventKind = 'match' | 'outright';

export type EventStatus =
  | 'scheduled'
  | 'live'
  | 'suspended'
  | 'ended'
  | 'settled'
  | 'postponed'
  | 'cancelled';

/**
 * `normalizer.EventRef`. Score and clock are deliberately absent from the
 * streamed payload; the REST event detail carries them.
 */
export interface EventRef {
  readonly id: string;
  readonly league_id: string;
  readonly kind: EventKind;
  readonly name: string;
  readonly home?: CompetitorRef;
  readonly away?: CompetitorRef;
  /** RFC 3339. */
  readonly scheduled_start: string;
  readonly status: EventStatus;
}

export type MarketType =
  | 'moneyline'
  | 'spread'
  | 'total'
  | 'player_prop'
  | 'futures';

export type MarketStatus =
  | 'open'
  | 'suspended'
  | 'closed'
  | 'settled'
  | 'voided';

/** `normalizer.MarketRef`. */
export interface MarketRef {
  readonly id: string;
  readonly event_id: string;
  readonly type: MarketType;
  /** The provider's own market key ("h2h", "player_pass_tds"). */
  readonly provider_key: string;
  /**
   * The market's CURRENT CONSENSUS line — null on moneyline and futures.
   *
   * It is not any single book's line. A quote's own `line` carries that, and the
   * two legitimately differ: this is the market's line now, that is the line the
   * quote was made at.
   */
  readonly line: number | null;
  /** The individual a player prop is about. Absent otherwise. */
  readonly subject?: string;
  readonly status: MarketStatus;
  /** RFC 3339. */
  readonly updated_at: string;
}

export type BookKind = 'external' | 'synthetic';

export type SelectionRole =
  | 'home'
  | 'draw'
  | 'away'
  | 'over'
  | 'under'
  | 'outright';

/**
 * `pricing.ReferenceRef` — the sharp book this market's fair value was devigged
 * from, and how that book came to be chosen.
 *
 * A consumer that cannot tell a catalogue-designated reference from a configured
 * fallback cannot tell a deliberate trading judgement from a default, and every
 * EV number on this market is relative to this book.
 */
export interface ReferenceRef {
  readonly book_id: string;
  readonly slug: string;
  readonly name: string;
  readonly kind: BookKind;
  readonly source: string;
  /** The reference book's OLDEST quote instant on this market. */
  readonly observed_at: string;
  readonly age_seconds: number;
}

/**
 * `pricing.FairSelection` — one selection's no-vig fair value.
 *
 * `name` here is THE ONLY PLACE A SELECTION'S DISPLAY NAME APPEARS on the
 * streamed payload. The book quotes below carry a `selection_id` and no name, so
 * a stream-only surface joins through this list.
 */
export interface FairSelection {
  readonly selection_id: string;
  readonly role: SelectionRole;
  readonly name: string;
  /** The DEVIGGED probability. This one may be called fair. */
  readonly probability: number;
  /** The devigged fair price, 1/probability. */
  readonly decimal: number;
  readonly reference_decimal: number;
  /** The reference book's implied probability, WITH its vig still in it. */
  readonly reference_implied: number;
  readonly excess: number;
  readonly relative_margin: number;
  readonly attributed_excess: number;
  readonly attributed_share: number;
}

/** `pricing.FairValue`. */
export interface FairValue {
  readonly method: string;
  readonly requested_method: string;
  readonly fallback: boolean;
  readonly attribution: string;
  readonly parameter: number;
  readonly iterations: number;
  readonly margin: Margin;
  readonly selections: readonly FairSelection[];
  readonly disagreement: number;
  readonly methods_compared: number;
}

/**
 * `pricing.QuoteAssessment` — one book's price on one selection, scored against
 * the fair value.
 */
export interface QuoteAssessment {
  readonly selection_id: string;
  readonly status: string;
  /** THE CANONICAL PRICE. Everything on screen is converted from this. */
  readonly decimal: number;
  /**
   * 1/decimal. The book's implied probability, WITH THE VIG STILL IN IT. It is
   * NOT a fair probability and must never be labelled as one; the de-vigged
   * number is `FairSelection.probability`.
   */
  readonly implied: number;
  /** The line THIS QUOTE was made at, from this selection's own perspective. */
  readonly line: number | null;
  readonly observed_at: string;
  readonly age_seconds: number;
  readonly fair_probability: number;
  readonly fair_decimal: number;
  readonly expected_value: number;
  readonly expected_value_percent: number;
  readonly edge: number;
  readonly edge_percent: number;
  readonly kelly: number;
  readonly fractional_kelly: number;
  readonly attributed_excess: number;
  readonly attributed_share: number;
}

/** `pricing.BookAssessment` — one book's whole position on this market. */
export interface BookAssessment {
  readonly book_id: string;
  readonly slug: string;
  readonly name: string;
  readonly kind: BookKind;
  /** Whether this is the book the fair value was devigged from. */
  readonly reference: boolean;
  /** Whether the book quoted every selection. */
  readonly complete: boolean;
  readonly eligible: boolean;
  /**
   * The book's margin. Meaningful only when `complete` is true: an overround
   * computed over a partial market is not a margin, it is a smaller number that
   * looks like a better price.
   */
  readonly margin: Margin;
  readonly oldest_observed_at: string;
  readonly newest_observed_at: string;
  readonly age_seconds: number;
  readonly quotes: readonly QuoteAssessment[];
}

/**
 * `pricing.ComputedMarket` — one market's no-vig fair value and every book's
 * price scored against it.
 *
 * The gateway propagates this document BYTE FOR BYTE from the compacted
 * `price.computed` topic. It is never re-shaped into a view type on the way
 * through, so what arrives here is exactly what the pricer published.
 */
export interface ComputedMarket {
  /** `pricing.SchemaVersion`. 1 today. */
  readonly schema_version: number;
  /**
   * The adapter slug the prices came from — "synthetic" or a real provider.
   *
   * A synthetic book's quote is a statement about a random number generator, and
   * ADR 0003 requires every surface that renders one to be able to say so. This
   * field, and `books[].kind`, are how a surface knows.
   */
  readonly provider: string;
  /**
   * The normalizer's hash of the market state this was computed from. Two
   * records with the same fingerprint describe the same market state, so it is
   * what distinguishes a recomputation from a movement.
   */
  readonly source_fingerprint: string;
  readonly source_schema_version: number;

  readonly sport: SportRef;
  readonly league: LeagueRef;
  readonly event: EventRef;
  readonly market: MarketRef;

  readonly reference: ReferenceRef;
  readonly fair: FairValue;
  /** Every quoting book, scored, ordered by book identifier. */
  readonly books: readonly BookAssessment[];

  /**
   * Under-round line groups found across the books on this record.
   *
   * ABSENT ON ALMOST EVERY MARKET, and that is the correct and expected state —
   * a feed with a constant arbitrage on it is a feed with a bug. Typed as
   * `unknown` because rendering it is phase 9 work; the shape is
   * `pricing.ArbitrageRef` in internal/pricing/signals.go.
   */
  readonly arbitrage?: readonly unknown[];
  /** `pricing.MiddleRef`. Phase 9. A middle can lose; it is never merged with arbitrage. */
  readonly middles?: readonly unknown[];

  /**
   * The provider's own observation instant. THE STALENESS SUBTRAHEND, and not
   * interchangeable with `ingested_at`.
   */
  readonly observed_at: string;
  /** When this system received the payload. */
  readonly ingested_at: string;
}

// -----------------------------------------------------------------------------
// Server frames — wsgw/protocol.go
// -----------------------------------------------------------------------------

interface FrameBase {
  /** Monotonic, per connection, starting at 1. See the file comment. */
  readonly seq: number;
  readonly type: ServerFrameType;
  /** RFC 3339. A DIAGNOSTIC — never the staleness subtrahend. */
  readonly ts: string;
  /** Echoes the client's correlation id when this frame answers a request. */
  readonly id?: string;
}

/** `wsgw.HelloFrame`. The first frame on every connection. */
export interface HelloFrame extends FrameBase {
  readonly type: 'hello';
  /**
   * Identifies THIS connection. A reconnect gets a new one and restarts the
   * sequence at 1, which is why a reconnect is not an epoch problem.
   */
  readonly connection_id: string;
  readonly protocol: string;
  /** `Options.PingInterval` in whole seconds. Size the liveness timer from it. */
  readonly heartbeat_seconds: number;
  readonly session_id?: string;
  /**
   * A subscription set was restored from Redis. This is the field that makes
   * affinity-free routing observable: reconnect, land on another replica, see
   * `resumed: true`, and the channels come back.
   */
  readonly resumed: boolean;
  /**
   * A credential was presented AND verified. FALSE IS THE NORMAL CASE: market
   * data is public and an anonymous connection is legal and is the default.
   */
  readonly authenticated: boolean;
  /** The restored channels. `[]` when nothing was restored, never null. */
  readonly channels: readonly Channel[];
}

/** `wsgw.RejectedChannel`. */
export interface RejectedChannel {
  readonly channel: string;
  readonly reason: RejectReason;
}

/**
 * `wsgw.AckFrame`. Answers a subscribe or an unsubscribe.
 *
 * A PARTIAL SUCCESS IS REPORTED AS ONE. `subscribed` is what the connection now
 * holds; `rejected` is what was refused, with a reason each.
 */
export interface AckFrame extends FrameBase {
  readonly type: 'ack';
  readonly subscribed: readonly Channel[];
  readonly rejected: readonly RejectedChannel[];
}

/**
 * `wsgw.SnapshotFrame`. A channel's current markets.
 *
 * An EMPTY `markets` array is a CORRECT snapshot of a channel with no markets —
 * a league with no scheduled events, an event whose markets have all been
 * tombstoned. Nothing fabricates a placeholder to make it look populated.
 */
export interface SnapshotFrame extends FrameBase {
  readonly type: 'snapshot';
  readonly channel: Channel;
  readonly markets: readonly ComputedMarket[];
  /**
   * This frame is the whole snapshot. Always true today, because the snapshot is
   * taken atomically under the gateway's state lock and is never chunked. Check
   * it anyway — the field exists so chunking can arrive without a version bump.
   */
  readonly complete: boolean;
}

/**
 * `wsgw.DeltaFrame`. One market's change on one channel.
 *
 * Exactly one of `market` and `removed` is populated. A TOMBSTONE (`removed`) on
 * a compacted topic means the market is gone for ever: a client that ignores it
 * leaves the market on the board permanently, because no further record for that
 * key is coming.
 */
export interface DeltaFrame extends FrameBase {
  readonly type: 'delta';
  readonly channel: Channel;
  readonly market?: ComputedMarket;
  readonly removed?: string;
}

/**
 * `wsgw.ResyncFrame`. The client's stream has a hole.
 *
 * Sent UNPROMPTED when the gateway discards a slow client's buffer, and in
 * answer to a client-requested resync. `from_seq`/`to_seq` bracket the sequence
 * numbers the client did not receive, inclusive, so the hole is diagnosable from
 * a browser console rather than only from the server's logs.
 */
export interface ResyncFrame extends FrameBase {
  readonly type: 'resync';
  readonly reason: ResyncReason;
  readonly dropped: number;
  readonly from_seq: number;
  readonly to_seq: number;
}

/** `wsgw.ErrorFrame`. A bounded, coded failure. */
export interface ErrorFrame extends FrameBase {
  readonly type: 'error';
  readonly code: WsErrorCode;
  /** Fixed human-readable text for a console. Never the client's own input. */
  readonly message: string;
}

/** `wsgw.PongFrame`. Answers a client ping — and still consumes a sequence number. */
export interface PongFrame extends FrameBase {
  readonly type: 'pong';
}

export type ServerFrame =
  | HelloFrame
  | AckFrame
  | SnapshotFrame
  | DeltaFrame
  | ResyncFrame
  | ErrorFrame
  | PongFrame;

// -----------------------------------------------------------------------------
// Client frames — wsgw.ClientFrame
// -----------------------------------------------------------------------------

/**
 * A client-to-server frame.
 *
 * `id` is an optional correlation token, at most 64 printable ASCII bytes,
 * echoed on the answering frame.
 *
 * A `resync` with an EMPTY OR ABSENT `channels` list means "every channel this
 * connection holds" — which is exactly what to send after detecting a sequence
 * gap, when the client does not know WHICH channel lost a frame.
 */
export interface ClientFrame {
  readonly type: ClientFrameType;
  readonly id?: string;
  readonly channels?: readonly Channel[];
}

/** The correlation-id charset and length the server accepts. */
export const MAX_REQUEST_ID_LENGTH = 64;

// -----------------------------------------------------------------------------
// Guards
// -----------------------------------------------------------------------------

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

export function isHello(frame: ServerFrame): frame is HelloFrame {
  return frame.type === 'hello';
}

export function isAck(frame: ServerFrame): frame is AckFrame {
  return frame.type === 'ack';
}

export function isSnapshot(frame: ServerFrame): frame is SnapshotFrame {
  return frame.type === 'snapshot';
}

export function isDelta(frame: ServerFrame): frame is DeltaFrame {
  return frame.type === 'delta';
}

export function isResync(frame: ServerFrame): frame is ResyncFrame {
  return frame.type === 'resync';
}

export function isError(frame: ServerFrame): frame is ErrorFrame {
  return frame.type === 'error';
}

export function isPong(frame: ServerFrame): frame is PongFrame {
  return frame.type === 'pong';
}

/**
 * Decodes one text frame, or returns `null`.
 *
 * Checks only the envelope — a numeric `seq`, a known `type`, a string `ts` —
 * and trusts the body beyond that. The gateway propagates the market document
 * byte for byte from a topic whose records it has already validated, so a
 * second full validation here would be a second declaration of the same facts,
 * which is the drift failure the Go side spends several comments arguing
 * against. What this DOES defend against is a truncated frame, a proxy error
 * page arriving on the socket, and a future frame kind this build does not know:
 * all three return null and are counted, and none of them reaches React.
 */
export function parseServerFrame(raw: string): ServerFrame | null {
  let decoded: unknown;
  try {
    decoded = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!isRecord(decoded)) return null;

  const { seq, type, ts } = decoded;
  if (typeof seq !== 'number' || !Number.isFinite(seq) || seq < 1) return null;
  if (typeof type !== 'string' || !SERVER_FRAME_TYPES.includes(type)) {
    return null;
  }
  if (typeof ts !== 'string') return null;

  return decoded as unknown as ServerFrame;
}

/** Every channel a market is delivered on. Mirrors `wsgw.ChannelsFor`. */
export function channelsFor(market: ComputedMarket): readonly Channel[] {
  return [
    marketChannel(market.market.id),
    eventChannel(market.event.id),
    leagueChannel(market.league.slug),
  ];
}

/**
 * The market's key in the live slate. `market.id` is also `SchemaMarket.id` on
 * REST, which is what lets a REST-rendered board be updated in place by a
 * streamed delta.
 */
export function marketIdOf(market: ComputedMarket): string {
  return market.market.id;
}
