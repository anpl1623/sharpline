// ---------------------------------------------------------------------------
// Forcing a sequence gap, deterministically, without touching the server.
// ---------------------------------------------------------------------------
// CLAUDE.md §5 and internal/wsgw D3 both make the same promise: "Every message
// carries a monotonic sequence number; a gap triggers client resync." D3 goes
// further and explains that the number is stamped at ENQUEUE precisely so that a
// discarded buffer shows up on the wire as a gap the client can SEE — "a resync
// that cannot be triggered is not a resync, and this is the one line of code
// that decides which of those two this service has."
//
// That promise is worth an assertion, and a conditional one ("if a gap happened,
// a resync must have followed") asserts nothing on a healthy connection. The
// server only creates a real gap when it drops a slow consumer's buffer, which a
// browser on a loopback network will never be.
//
// So the gap is injected on the CLIENT side, one layer below the application:
// `window.WebSocket` is subclassed and exactly one `delta` frame is swallowed
// before it reaches any listener. Everything else is untouched.
//
// WHAT THIS DOES AND DOES NOT PROVE. It is not a mock and it fabricates nothing:
// the socket is real, the frames are real, the swallowed frame is a real frame
// that really arrived. What it removes is one frame's delivery, which is
// indistinguishable — from `web/src/lib/ws/client.ts`'s point of view — from the
// buffer discard the server performs on a slow consumer. So it proves the CLIENT
// half of the contract: gap detected, resync requested, snapshot applied, board
// recovered, no reload. The SERVER half (a slow consumer is dropped, its buffer
// discarded, and a `resync` frame enqueued with the gap visible) is covered by
// internal/wsgw's own hub tests, where a slow consumer can actually be built.
//
// Note the recorder sees the frame the shim swallows: `page.on('websocket')`
// reads the wire, and the wire is intact. That is exactly why the assertions
// below are about what the client SENDS in response, not about a gap in the
// received sequence.
// ---------------------------------------------------------------------------

import type { Page } from '@playwright/test';

/** Where the shim reports itself, on `window`. */
export const GAP_HANDLE = '__sharplineGapInjector';

export interface GapInjectorState {
  /** `seq` of the delta frame that was swallowed, or null if none yet. */
  readonly droppedSeq: number | null;
  /** How many documents this page context has loaded. A reload increments it. */
  readonly loads: number;
}

/**
 * Install the injector. MUST be called before the first navigation —
 * `addInitScript` runs on every document, and the shim has to be in place before
 * the app's bundle constructs its socket.
 */
export async function installGapInjector(page: Page): Promise<void> {
  await page.addInitScript((handle: string) => {
    const scope = window as unknown as Record<string, unknown>;

    // `loads` survives a reload only if the page navigates within the same
    // context, which is the point: the assertion is that recovery needed NO
    // reload, so a reload has to be observable.
    const previous = scope[handle] as { loads?: number } | undefined;
    const state = {
      droppedSeq: null as number | null,
      loads: (previous?.loads ?? 0) + 1,
    };
    scope[handle] = state;

    const Native = window.WebSocket;

    class GapInjectingWebSocket extends Native {
      override addEventListener(
        type: string,
        listener: EventListenerOrEventListenerObject | null,
        options?: boolean | AddEventListenerOptions,
      ): void {
        if (type !== 'message' || typeof listener !== 'function') {
          super.addEventListener(type, listener, options);
          return;
        }

        const wrapped = (event: Event): void => {
          if (state.droppedSeq === null) {
            const data: unknown = (event as MessageEvent).data;
            if (typeof data === 'string') {
              let frame: { type?: unknown; seq?: unknown } | null = null;
              try {
                frame = JSON.parse(data) as { type?: unknown; seq?: unknown };
              } catch {
                frame = null;
              }
              // Swallow exactly ONE delta. A delta rather than a snapshot or an
              // ack, because a delta is the frame a live board actually depends
              // on and because dropping the snapshot would test a different
              // thing (an empty board) than the one this file is about.
              if (frame !== null && frame.type === 'delta' && typeof frame.seq === 'number') {
                state.droppedSeq = frame.seq;
                return;
              }
            }
          }
          listener(event);
        };

        super.addEventListener(type, wrapped, options);
      }
    }

    window.WebSocket = GapInjectingWebSocket as unknown as typeof WebSocket;
  }, GAP_HANDLE);
}

/** Read the shim's state out of the page. */
export async function gapState(page: Page): Promise<GapInjectorState> {
  return page.evaluate((handle: string) => {
    const state = (window as unknown as Record<string, unknown>)[handle] as
      | GapInjectorState
      | undefined;
    return state ?? { droppedSeq: null, loads: 0 };
  }, GAP_HANDLE);
}
