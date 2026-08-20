/**
 * The WebSocket client: framing, sequence-gap detection, resync, heartbeat and
 * reconnection.
 *
 * Framework-agnostic on purpose — there is no React in this file. It is a plain
 * class with an event emitter, so it can be driven from a provider, from a test,
 * or from a console.
 *
 * # The sequence gap is the whole reason this class is careful
 *
 * The gateway stamps a frame's sequence number AT ENQUEUE. When it drops a slow
 * client's pending buffer, those frames have already consumed numbers, so the
 * drop appears on the wire as `seq 4` followed by `seq 41`. That gap is the ONLY
 * evidence the client has that its board is missing updates — there is no error,
 * no close, and the prices simply stop moving on whichever markets were in the
 * discarded buffer.
 *
 * So: every inbound frame's `seq` is checked against an expectation, a mismatch
 * emits a `gap`, sends `{"type":"resync"}` with NO channels (meaning "every
 * channel this connection holds", which is what to ask for when the client
 * cannot know which channel lost a frame), and moves the UI into `resyncing`.
 * The expectation is then set to `frame.seq + 1` rather than left behind, so one
 * gap produces one resync instead of a resync per frame for the rest of the
 * connection.
 *
 * The expectation resets to 1 on EVERY new socket. Each connection announces its
 * own `connection_id` and restarts at 1; a reconnect starting over is not an
 * epoch problem, it is a different connection.
 *
 * # The token never goes in the URL
 *
 * It is offered as a subprotocol: `['sharpline.v1', 'sharpline.bearer.' + jwt]`.
 * A browser's `WebSocket` constructor can set exactly one request header and
 * there is no way to send `Authorization`, so the subprotocol is the only
 * mechanism available. A token in the query string lands in access logs, in
 * `Referer` headers and in browser history — and the server refuses it outright
 * as a distinct outcome rather than silently downgrading to anonymous.
 *
 * ANONYMOUS IS LEGAL AND IS THE DEFAULT. Market data is public, so the board
 * works signed out and this client connects with no credential unless given one.
 */

import type {
  Channel,
  ClientFrame,
  ComputedMarket,
  ErrorFrame,
  ResyncFrame,
  ServerFrame,
  SnapshotFrame,
  DeltaFrame,
} from '@/lib/ws/protocol';
import {
  WS_BEARER_PREFIX,
  WS_PROTOCOL,
  isAck,
  isDelta,
  isError,
  isHello,
  isResync,
  isSnapshot,
  parseServerFrame,
} from '@/lib/ws/protocol';

// -----------------------------------------------------------------------------
// Status
// -----------------------------------------------------------------------------

export type StreamStatus =
  | 'idle'
  | 'connecting'
  | 'open'
  | 'resyncing'
  | 'reconnecting'
  | 'closed';

/** Everything the status rail and the mobile status pip render. */
export interface StreamState {
  readonly status: StreamStatus;
  /** `hello.connection_id`. Null until the first hello on this socket. */
  readonly connectionId: string | null;
  readonly sessionId: string | null;
  readonly protocol: string | null;
  /** Whether a credential was presented AND verified. False is normal. */
  readonly authenticated: boolean;
  /** Whether the gateway restored a subscription set from Redis. */
  readonly resumed: boolean;
  readonly heartbeatSeconds: number | null;
  /** The highest sequence number seen on this connection. */
  readonly lastSeq: number;
  /** Epoch ms of the last frame of any kind. */
  readonly lastFrameAt: number | null;
  /** The server's own `ts` on the last frame — the anchor for staleness. */
  readonly lastFrameTs: string | null;
  readonly channels: readonly Channel[];
  readonly channelCount: number;
  /** Sequence gaps observed since construction. */
  readonly gaps: number;
  /** Resyncs requested or received since construction. */
  readonly resyncs: number;
  readonly reconnects: number;
  /** Frames that did not parse and were dropped. */
  readonly malformedFrames: number;
  readonly lastError: { readonly code: string; readonly message: string } | null;
  /**
   * A credential was rejected. Reconnection is STOPPED — a bad token is not a
   * transient failure and retrying it forever produces a login loop nobody can
   * see the cause of.
   */
  readonly unauthorized: boolean;
}

const INITIAL_STATE: StreamState = {
  status: 'idle',
  connectionId: null,
  sessionId: null,
  protocol: null,
  authenticated: false,
  resumed: false,
  heartbeatSeconds: null,
  lastSeq: 0,
  lastFrameAt: null,
  lastFrameTs: null,
  channels: [],
  channelCount: 0,
  gaps: 0,
  resyncs: 0,
  reconnects: 0,
  malformedFrames: 0,
  lastError: null,
  unauthorized: false,
};

/** The state a server render and a not-yet-mounted client both report. */
export const IDLE_STREAM_STATE: StreamState = INITIAL_STATE;

// -----------------------------------------------------------------------------
// The DESIGN.md status pip
// -----------------------------------------------------------------------------

/**
 * The five connection tones DESIGN.md defines, and their accessible names.
 *
 * The pip carries connection state by fill and NEVER BY FILL ALONE: the name
 * states the same fact in words, so the colour is redundant both for a screen
 * reader and for a colourblind reader. `label` is the accessible name verbatim
 * from DESIGN.md's table — do not paraphrase it in a component.
 */
export type StreamTone =
  | 'streaming'
  | 'resyncing'
  | 'reconnecting'
  | 'disconnected'
  | 'idle';

export interface StreamDescription {
  readonly tone: StreamTone;
  readonly label: string;
}

export function describeStream(state: StreamState): StreamDescription {
  switch (state.status) {
    case 'open':
      return { tone: 'streaming', label: 'Live — streaming' };
    case 'resyncing':
      return { tone: 'resyncing', label: 'Resyncing' };
    case 'reconnecting':
      return { tone: 'reconnecting', label: 'Reconnecting' };
    case 'connecting':
      // The FIRST attempt is not a reconnection. DESIGN.md has no separate
      // "connecting" state, and "not connected" is the true statement while the
      // handshake is still in flight; a subsequent attempt is a reconnection and
      // says so.
      return state.reconnects > 0
        ? { tone: 'reconnecting', label: 'Reconnecting' }
        : { tone: 'idle', label: 'Not connected' };
    case 'closed':
      return { tone: 'disconnected', label: 'Disconnected' };
    case 'idle':
      return { tone: 'idle', label: 'Not connected' };
    default:
      return { tone: 'idle', label: 'Not connected' };
  }
}

// -----------------------------------------------------------------------------
// Events
// -----------------------------------------------------------------------------

export interface SequenceGap {
  readonly expected: number;
  readonly got: number;
}

interface EventMap {
  /** The whole state object changed. Referentially new; safe for a store. */
  state: StreamState;
  /** Every parsed frame, in order. */
  frame: ServerFrame;
  snapshot: SnapshotFrame;
  delta: DeltaFrame;
  gap: SequenceGap;
  resync: ResyncFrame;
  error: ErrorFrame;
  /** A new socket was opened; the slate should be treated as cold. */
  reset: { readonly connectionId: string | null };
}

type Listener<T> = (payload: T) => void;

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

export interface WsClientOptions {
  /** Overrides `NEXT_PUBLIC_WS_URL`. Rarely needed outside a test. */
  readonly url?: string | undefined;
  /** The access token, or null for an anonymous connection (the default). */
  readonly accessToken?: string | null;
  readonly baseBackoffMs?: number;
  readonly backoffFactor?: number;
  readonly maxBackoffMs?: number;
  /** +/- this fraction of the computed delay. */
  readonly jitterRatio?: number;
  /** Used until `hello.heartbeat_seconds` arrives. */
  readonly fallbackHeartbeatSeconds?: number;
  /** Close and reconnect after this multiple of the heartbeat with no frame. */
  readonly livenessFactor?: number;
  /** Injection seam for tests. */
  readonly socketFactory?: (url: string, protocols: string[]) => WebSocket;
}

const DEFAULT_BASE_BACKOFF_MS = 500;
const DEFAULT_BACKOFF_FACTOR = 1.8;
const DEFAULT_MAX_BACKOFF_MS = 15_000;
const DEFAULT_JITTER_RATIO = 0.25;
const DEFAULT_HEARTBEAT_SECONDS = 20;
const DEFAULT_LIVENESS_FACTOR = 2.5;

/** Close code used when this client gives up on a silent socket. */
const CLOSE_LIVENESS = 4000;
/** Close code used for a deliberate teardown. */
const CLOSE_NORMAL = 1000;

// -----------------------------------------------------------------------------
// URL resolution
// -----------------------------------------------------------------------------

/**
 * Resolves the gateway URL against `window.location`, mapping https to wss and
 * http to ws.
 *
 * `NEXT_PUBLIC_WS_URL` is a PROXY PATH (`/ws`), not an origin — Caddy maps it
 * onto `stream:8081`. A container hostname here would be unresolvable from a
 * browser, so an absolute URL is accepted only when it is already ws/wss or
 * http/https and is assumed to be a deliberate override.
 */
export function resolveWsUrl(configured?: string): string {
  const raw = configured ?? process.env.NEXT_PUBLIC_WS_URL ?? '/ws';

  if (/^wss?:\/\//i.test(raw)) return raw;
  if (/^https:\/\//i.test(raw)) return `wss://${raw.slice('https://'.length)}`;
  if (/^http:\/\//i.test(raw)) return `ws://${raw.slice('http://'.length)}`;

  const location = window.location;
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const path = raw.startsWith('/') ? raw : `/${raw}`;
  return `${scheme}//${location.host}${path}`;
}

// -----------------------------------------------------------------------------
// The client
// -----------------------------------------------------------------------------

export class WsClient {
  private readonly options: WsClientOptions;
  private readonly listeners = new Map<
    keyof EventMap,
    Set<Listener<never>>
  >();

  private socket: WebSocket | null = null;
  private state: StreamState = INITIAL_STATE;

  /**
   * The channel set this CLIENT holds, independent of any socket. It is re-sent
   * on every connect, so a reconnect restores the board with no page-level code
   * involved.
   */
  private readonly held = new Set<Channel>();

  /** The next sequence number expected on the CURRENT socket. */
  private expectedSeq = 1;

  private accessToken: string | null;
  private shouldRun = false;
  private attempt = 0;
  private requestCounter = 0;

  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private pingTimer: ReturnType<typeof setInterval> | null = null;
  private livenessTimer: ReturnType<typeof setInterval> | null = null;

  constructor(options: WsClientOptions = {}) {
    this.options = options;
    this.accessToken = options.accessToken ?? null;
  }

  // ---------------------------------------------------------------------------
  // Public surface
  // ---------------------------------------------------------------------------

  getState(): StreamState {
    return this.state;
  }

  /**
   * Subscribes a listener. Returns the unsubscribe function, so a React effect
   * can return it directly.
   */
  on<K extends keyof EventMap>(
    event: K,
    listener: Listener<EventMap[K]>,
  ): () => void {
    let set = this.listeners.get(event);
    if (set === undefined) {
      set = new Set<Listener<never>>();
      this.listeners.set(event, set);
    }
    const erased = listener as unknown as Listener<never>;
    const target = set;
    target.add(erased);
    return () => {
      target.delete(erased);
    };
  }

  /**
   * Opens the socket. Safe to call repeatedly and safe to call on the server,
   * where it is a no-op — there is no WebSocket in a Node render and a client
   * that threw there would take down the page instead of degrading to the
   * REST-rendered board.
   */
  connect(): void {
    if (typeof window === 'undefined') return;
    if (this.state.unauthorized) return;
    this.shouldRun = true;
    if (this.socket !== null) return;
    this.openSocket();
  }

  /** Tears down: no reconnect, no timers, no listeners left on the socket. */
  close(): void {
    this.shouldRun = false;
    this.clearReconnect();
    this.stopTimers();
    const socket = this.socket;
    this.socket = null;
    if (socket !== null) {
      this.detach(socket);
      if (
        socket.readyState === WebSocket.OPEN ||
        socket.readyState === WebSocket.CONNECTING
      ) {
        socket.close(CLOSE_NORMAL, 'client closed');
      }
    }
    this.patch({ status: 'closed', connectionId: null });
  }

  /**
   * Adds channels to the held set and subscribes if the socket is open. Channels
   * already held are not re-sent — the server answers a duplicate with a
   * `duplicate` rejection, which is noise on the ack.
   */
  subscribe(channels: readonly Channel[]): void {
    const added: Channel[] = [];
    for (const channel of channels) {
      if (channel === '' || this.held.has(channel)) continue;
      this.held.add(channel);
      added.push(channel);
    }
    if (added.length === 0) return;
    this.publishChannels();
    this.send({ type: 'subscribe', id: this.nextRequestId(), channels: added });
  }

  /** Removes channels from the held set and unsubscribes if the socket is open. */
  unsubscribe(channels: readonly Channel[]): void {
    const removed: Channel[] = [];
    for (const channel of channels) {
      if (!this.held.delete(channel)) continue;
      removed.push(channel);
    }
    if (removed.length === 0) return;
    this.publishChannels();
    this.send({
      type: 'unsubscribe',
      id: this.nextRequestId(),
      channels: removed,
    });
  }

  /** The channels this client holds, independent of socket state. */
  channels(): readonly Channel[] {
    return [...this.held];
  }

  /**
   * Asks the gateway to re-send snapshots.
   *
   * With no argument this sends a resync with NO channels, which the server
   * reads as "every channel this connection holds". That is the correct request
   * after a sequence gap, when the client cannot know which channel lost a
   * frame.
   */
  resync(channels?: readonly Channel[]): void {
    this.patch({
      status: this.state.status === 'open' ? 'resyncing' : this.state.status,
      resyncs: this.state.resyncs + 1,
    });
    if (channels === undefined || channels.length === 0) {
      this.send({ type: 'resync', id: this.nextRequestId() });
      return;
    }
    this.send({ type: 'resync', id: this.nextRequestId(), channels });
  }

  /**
   * Replaces the credential.
   *
   * Changing it reconnects, because the credential is presented in the
   * handshake and there is no in-band way to re-present it. That cost is why the
   * provider does not attach a rotating access token by default: the phase-7
   * stream surface is entirely public, and reconnecting on every token rotation
   * would buy nothing and cost a snapshot storm.
   */
  setAccessToken(token: string | null): void {
    if (token === this.accessToken) return;
    this.accessToken = token;
    this.patch({ unauthorized: false });
    if (!this.shouldRun) return;
    this.cycleSocket();
  }

  // ---------------------------------------------------------------------------
  // Socket lifecycle
  // ---------------------------------------------------------------------------

  private openSocket(): void {
    let url: string;
    try {
      url = resolveWsUrl(this.options.url);
    } catch {
      this.scheduleReconnect();
      return;
    }

    // THE CREDENTIAL IS OFFERED AS A SUBPROTOCOL, NEVER AS A QUERY PARAMETER.
    // See the file comment. There is no debug path that builds a URL with it.
    const protocols =
      this.accessToken === null || this.accessToken === ''
        ? [WS_PROTOCOL]
        : [WS_PROTOCOL, `${WS_BEARER_PREFIX}${this.accessToken}`];

    // The expectation resets for every socket: each connection has its own
    // sequence space and restarts at 1.
    this.expectedSeq = 1;

    this.patch({
      status: this.attempt === 0 ? 'connecting' : 'reconnecting',
      connectionId: null,
      sessionId: null,
      protocol: null,
      authenticated: false,
      resumed: false,
      lastSeq: 0,
    });

    let socket: WebSocket;
    try {
      socket = this.options.socketFactory
        ? this.options.socketFactory(url, protocols)
        : new WebSocket(url, protocols);
    } catch {
      this.scheduleReconnect();
      return;
    }

    this.socket = socket;
    socket.addEventListener('open', this.onOpen);
    socket.addEventListener('message', this.onMessage);
    socket.addEventListener('close', this.onClose);
    socket.addEventListener('error', this.onSocketError);
  }

  private detach(socket: WebSocket): void {
    socket.removeEventListener('open', this.onOpen);
    socket.removeEventListener('message', this.onMessage);
    socket.removeEventListener('close', this.onClose);
    socket.removeEventListener('error', this.onSocketError);
  }

  /** Closes the current socket and reconnects immediately. */
  private cycleSocket(): void {
    const socket = this.socket;
    this.socket = null;
    this.stopTimers();
    if (socket !== null) {
      this.detach(socket);
      if (
        socket.readyState === WebSocket.OPEN ||
        socket.readyState === WebSocket.CONNECTING
      ) {
        socket.close(CLOSE_NORMAL, 'client cycled');
      }
    }
    this.attempt = 0;
    this.openSocket();
  }

  private readonly onOpen = (): void => {
    this.attempt = 0;
    this.emit('reset', { connectionId: null });
    // Re-assert the held channel set on every connection. The server may also
    // have restored it from Redis (`hello.resumed`), but asking is idempotent
    // and is what makes a reconnect invisible to page-level code.
    if (this.held.size > 0) {
      this.send({
        type: 'subscribe',
        id: this.nextRequestId(),
        channels: [...this.held],
      });
    }
  };

  private readonly onSocketError = (): void => {
    // A WebSocket `error` event carries no detail by design (it would be a
    // cross-origin information leak). The close event that follows is where the
    // reconnection decision is made, so nothing is done here beyond not
    // throwing.
  };

  private readonly onClose = (): void => {
    const socket = this.socket;
    if (socket !== null) this.detach(socket);
    this.socket = null;
    this.stopTimers();

    if (!this.shouldRun || this.state.unauthorized) {
      this.patch({ status: 'closed', connectionId: null });
      return;
    }
    this.patch({ status: 'reconnecting', connectionId: null });
    this.scheduleReconnect();
  };

  private readonly onMessage = (event: MessageEvent): void => {
    if (typeof event.data !== 'string') {
      // The protocol is JSON TEXT frames. A binary frame is not something this
      // build understands, and guessing at it is worse than dropping it.
      this.patch({ malformedFrames: this.state.malformedFrames + 1 });
      return;
    }

    const frame = parseServerFrame(event.data);
    if (frame === null) {
      this.patch({ malformedFrames: this.state.malformedFrames + 1 });
      return;
    }

    this.noteFrame(frame);
    this.emit('frame', frame);
    this.route(frame);
  };

  // ---------------------------------------------------------------------------
  // Frame handling
  // ---------------------------------------------------------------------------

  /**
   * Records the frame's arrival and checks the sequence.
   *
   * A gap sets the expectation to `frame.seq + 1` rather than leaving it behind,
   * so ONE gap produces ONE resync. Leaving it behind would make every
   * subsequent frame look like a gap and turn a single dropped buffer into a
   * resync storm against a server that is already under pressure.
   */
  private noteFrame(frame: ServerFrame): void {
    const gapped = frame.seq !== this.expectedSeq;
    const expected = this.expectedSeq;
    this.expectedSeq = frame.seq + 1;

    this.patch({
      lastSeq: frame.seq,
      lastFrameAt: Date.now(),
      lastFrameTs: frame.ts,
      ...(gapped ? { gaps: this.state.gaps + 1 } : {}),
    });

    if (!gapped) return;

    this.emit('gap', { expected, got: frame.seq });
    // No channels: "every channel this connection holds". The client does not
    // know which channel lost a frame, and guessing would leave the wrong one
    // stale.
    this.resync();
  }

  private route(frame: ServerFrame): void {
    if (isHello(frame)) {
      this.patch({
        status: 'open',
        connectionId: frame.connection_id,
        sessionId: frame.session_id ?? null,
        protocol: frame.protocol,
        authenticated: frame.authenticated,
        resumed: frame.resumed,
        heartbeatSeconds: frame.heartbeat_seconds,
      });
      this.startTimers(frame.heartbeat_seconds);
      return;
    }

    if (isSnapshot(frame)) {
      // A snapshot is the answer to a subscribe or a resync. Receiving one means
      // the hole is filled, so the UI leaves the resyncing state.
      if (this.state.status === 'resyncing') this.patch({ status: 'open' });
      this.emit('snapshot', frame);
      return;
    }

    if (isDelta(frame)) {
      this.emit('delta', frame);
      return;
    }

    if (isResync(frame)) {
      // SERVER-INITIATED. `slow_consumer` means our buffer was discarded;
      // `presence_lost` means the fleet's subscription state was wiped. Either
      // way what we hold is now untrustworthy, so re-assert the channel set to
      // get fresh snapshots.
      this.patch({ status: 'resyncing', resyncs: this.state.resyncs + 1 });
      this.emit('resync', frame);
      if (this.held.size > 0) {
        this.send({
          type: 'subscribe',
          id: this.nextRequestId(),
          channels: [...this.held],
        });
      } else {
        this.patch({ status: 'open' });
      }
      return;
    }

    if (isAck(frame)) {
      if (this.state.status === 'resyncing' && this.held.size === 0) {
        this.patch({ status: 'open' });
      }
      return;
    }

    if (isError(frame)) {
      this.patch({
        lastError: { code: frame.code, message: frame.message },
      });
      this.emit('error', frame);
      if (frame.code === 'unauthorized') {
        // A bad token is NOT a transient failure. Retrying it forever produces a
        // reconnect loop whose cause is invisible, so reconnection stops here
        // and the state says why.
        this.patch({ unauthorized: true, status: 'closed' });
        this.shouldRun = false;
        this.clearReconnect();
        this.stopTimers();
        const socket = this.socket;
        this.socket = null;
        if (socket !== null) {
          this.detach(socket);
          socket.close(CLOSE_NORMAL, 'unauthorized');
        }
      }
      return;
    }

    // pong: nothing to do. It still consumed a sequence number and still reset
    // the liveness clock, both of which happened in noteFrame.
  }

  // ---------------------------------------------------------------------------
  // Heartbeat
  // ---------------------------------------------------------------------------

  /**
   * Starts the ping interval and the liveness watchdog.
   *
   * The interval comes from `hello.heartbeat_seconds` rather than from a
   * constant here, so the period cannot drift away from the server's. The
   * watchdog closes the socket when NO FRAME OF ANY KIND has arrived within
   * 2.5x that interval — any frame counts, because the gateway is continuously
   * sending deltas on a live board and a silent socket is the failure, not a
   * missing pong specifically.
   */
  private startTimers(heartbeatSeconds: number): void {
    this.stopTimers();
    const seconds =
      heartbeatSeconds > 0
        ? heartbeatSeconds
        : (this.options.fallbackHeartbeatSeconds ?? DEFAULT_HEARTBEAT_SECONDS);
    const intervalMs = seconds * 1000;
    const livenessMs =
      intervalMs * (this.options.livenessFactor ?? DEFAULT_LIVENESS_FACTOR);

    this.pingTimer = setInterval(() => {
      this.send({ type: 'ping' });
    }, intervalMs);

    this.livenessTimer = setInterval(() => {
      const last = this.state.lastFrameAt;
      if (last === null) return;
      if (Date.now() - last < livenessMs) return;
      const socket = this.socket;
      if (socket === null) return;
      // Close rather than wait. A half-open TCP connection can sit there for
      // minutes with the board frozen and every indicator claiming health.
      socket.close(CLOSE_LIVENESS, 'no frames within liveness window');
    }, intervalMs);
  }

  private stopTimers(): void {
    if (this.pingTimer !== null) {
      clearInterval(this.pingTimer);
      this.pingTimer = null;
    }
    if (this.livenessTimer !== null) {
      clearInterval(this.livenessTimer);
      this.livenessTimer = null;
    }
  }

  // ---------------------------------------------------------------------------
  // Reconnection
  // ---------------------------------------------------------------------------

  /**
   * Jittered exponential backoff: 500ms base, x1.8, capped at 15s, +/-25%.
   *
   * The jitter is not decoration. Without it, a gateway restart brings every
   * client back in the same millisecond, which is a thundering herd against a
   * process that is still reading the compacted topic to rebuild its slate.
   */
  private scheduleReconnect(): void {
    if (!this.shouldRun || this.state.unauthorized) return;
    this.clearReconnect();

    const base = this.options.baseBackoffMs ?? DEFAULT_BASE_BACKOFF_MS;
    const factor = this.options.backoffFactor ?? DEFAULT_BACKOFF_FACTOR;
    const cap = this.options.maxBackoffMs ?? DEFAULT_MAX_BACKOFF_MS;
    const jitter = this.options.jitterRatio ?? DEFAULT_JITTER_RATIO;

    const raw = Math.min(cap, base * Math.pow(factor, this.attempt));
    const spread = raw * jitter;
    const delay = Math.max(0, raw + (Math.random() * 2 - 1) * spread);

    this.attempt += 1;
    this.patch({
      status: 'reconnecting',
      reconnects: this.state.reconnects + 1,
    });

    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      if (!this.shouldRun) return;
      this.openSocket();
    }, delay);
  }

  private clearReconnect(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  // ---------------------------------------------------------------------------
  // Plumbing
  // ---------------------------------------------------------------------------

  /**
   * Sends a client frame if the socket is open.
   *
   * A frame sent while closed is DROPPED, not queued. The only frames worth
   * replaying are subscriptions, and those are re-asserted from the held set on
   * every connect; queueing a stale ping or a stale resync would send it against
   * a different connection's sequence space, where it means nothing.
   */
  private send(frame: ClientFrame): void {
    const socket = this.socket;
    if (socket === null || socket.readyState !== WebSocket.OPEN) return;
    try {
      socket.send(JSON.stringify(frame));
    } catch {
      // A send on a socket that closed between the readyState check and here.
      // The close handler will reconnect; nothing to add.
    }
  }

  private nextRequestId(): string {
    this.requestCounter += 1;
    // Printable ASCII, well under the server's 64-byte bound.
    return `c${String(this.requestCounter)}`;
  }

  private publishChannels(): void {
    const channels = [...this.held];
    this.patch({ channels, channelCount: channels.length });
  }

  /** Replaces the state object and notifies. Never mutates in place. */
  private patch(partial: Partial<StreamState>): void {
    this.state = { ...this.state, ...partial };
    this.emit('state', this.state);
  }

  private emit<K extends keyof EventMap>(event: K, payload: EventMap[K]): void {
    const set = this.listeners.get(event);
    if (set === undefined) return;
    for (const listener of [...set]) {
      const typed = listener as unknown as Listener<EventMap[K]>;
      try {
        typed(payload);
      } catch {
        // A listener that throws must not break the socket loop. Dropping the
        // frame for that one listener is strictly better than losing every
        // subsequent frame for every listener.
      }
    }
  }
}

/** Convenience re-export so a consumer needs one import for the market type. */
export type { ComputedMarket };
