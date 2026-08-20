/**
 * The one error shape, parsed.
 *
 * Every non-2xx response from this API has exactly one body:
 *
 *     { "error": { code, message, request_id, invalid_params? } }
 *
 * so the client writes one decoder and one branch. `code` is the stable
 * machine-readable token to switch on; the HTTP status is a coarser
 * classification of the same fact.
 *
 * # request_id is a DEVELOPER-facing detail, never the user-facing message
 *
 * It correlates the response with the server log line and the trace span that
 * produced it, and it is the only handle anyone has on what actually happened.
 * It belongs in a `<details>`, a copy button, or the engineering layer — not in
 * the sentence a bettor reads when the board fails to load.
 *
 * # Nothing here ever touches a credential
 *
 * `ApiError` carries a status, a code, a message, a request id and the invalid
 * parameter list. It never carries the request body, the URL's query string, or
 * a header, because an access token would end up in every error boundary, every
 * log line and every crash report the moment one of those was included.
 */

import type { SchemaError, SchemaInvalidParam } from '@/lib/api/schema';

/** The server's closed set of error codes. */
export type ApiErrorCode = SchemaError['code'];

/**
 * Failures that never reached the server, or reached it and came back
 * unreadable. They are spelled differently from the server's codes so a
 * `switch` over `ApiErrorCode` cannot silently absorb one.
 */
export type TransportErrorCode =
  | 'network'
  | 'timeout'
  | 'aborted'
  | 'malformed_response';

export type AnyApiErrorCode = ApiErrorCode | TransportErrorCode;

const TRANSPORT_CODES: readonly TransportErrorCode[] = [
  'network',
  'timeout',
  'aborted',
  'malformed_response',
];

/** Whether a code describes a failure that never got a server answer. */
export function isTransportCode(code: AnyApiErrorCode): code is TransportErrorCode {
  return (TRANSPORT_CODES as readonly string[]).includes(code);
}

export interface ApiErrorInit {
  readonly status: number;
  readonly code: AnyApiErrorCode;
  readonly message: string;
  readonly requestId?: string | null;
  readonly invalidParams?: readonly SchemaInvalidParam[] | undefined;
  readonly cause?: unknown;
}

/**
 * A failed API call.
 *
 * `status` is 0 for a transport failure — nothing came back, so there is no
 * status to report, and 0 is distinguishable from every real one.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: AnyApiErrorCode;
  readonly requestId: string | null;
  readonly invalidParams: readonly SchemaInvalidParam[];

  constructor(init: ApiErrorInit) {
    super(init.message);
    this.name = 'ApiError';
    this.status = init.status;
    this.code = init.code;
    this.requestId = init.requestId ?? null;
    this.invalidParams = init.invalidParams ?? [];
    if (init.cause !== undefined) {
      this.cause = init.cause;
    }
  }

  /** Whether the request never reached a server answer. */
  get isTransport(): boolean {
    return isTransportCode(this.code);
  }

  /**
   * Whether retrying could plausibly succeed. A 4xx is the caller's fault and
   * will fail identically on the next attempt; a transport failure, a 429 and a
   * 5xx are all worth one more try. This is what the query client's `retry`
   * predicate reads.
   */
  get isRetryable(): boolean {
    if (this.code === 'aborted') return false;
    if (this.isTransport) return true;
    if (this.status === 429) return true;
    return this.status >= 500;
  }

  /** Whether this is an authentication failure the UI should react to. */
  get isUnauthenticated(): boolean {
    return this.status === 401;
  }

  /**
   * The developer-facing one-liner: the code, the status and the request id.
   * Render it in a detail disclosure or the engineering layer, never as the
   * primary message.
   */
  get detail(): string {
    const parts = [`${this.code} (HTTP ${String(this.status)})`];
    if (this.requestId !== null && this.requestId !== '') {
      parts.push(`request ${this.requestId}`);
    }
    return parts.join(' · ');
  }

  /** The reason recorded against one named parameter, if there is one. */
  reasonFor(parameter: string): string | null {
    const found = this.invalidParams.find((param) => param.name === parameter);
    return found?.reason ?? null;
  }
}

/** Narrowing guard. Prefer this to `instanceof` at call sites. */
export function isApiError(value: unknown): value is ApiError {
  return value instanceof ApiError;
}

// -----------------------------------------------------------------------------
// Envelope parsing
// -----------------------------------------------------------------------------

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function readInvalidParams(value: unknown): readonly SchemaInvalidParam[] {
  if (!Array.isArray(value)) return [];
  const params: SchemaInvalidParam[] = [];
  for (const entry of value) {
    if (!isRecord(entry)) continue;
    const { name, reason } = entry;
    if (typeof name === 'string' && typeof reason === 'string') {
      params.push({ name, reason });
    }
  }
  return params;
}

/**
 * Turns a decoded response body into an `ApiError`, defensively.
 *
 * A body that does not have the documented shape is not thrown away and is not
 * trusted either: the status still classifies the failure, and the message
 * falls back to a fixed string. A proxy returning an HTML error page must not
 * produce `undefined` on screen.
 */
export function apiErrorFromBody(status: number, body: unknown): ApiError {
  const envelope = isRecord(body) ? body['error'] : undefined;
  if (!isRecord(envelope)) {
    return new ApiError({
      status,
      code: 'malformed_response',
      message: fallbackMessageFor(status),
    });
  }

  const code = envelope['code'];
  const message = envelope['message'];
  const requestId = envelope['request_id'];

  return new ApiError({
    status,
    code:
      typeof code === 'string'
        ? (code as AnyApiErrorCode)
        : 'malformed_response',
    message: typeof message === 'string' && message !== ''
      ? message
      : fallbackMessageFor(status),
    requestId: typeof requestId === 'string' ? requestId : null,
    invalidParams: readInvalidParams(envelope['invalid_params']),
  });
}

/**
 * The user-facing sentence when the server did not supply one. Fixed strings
 * chosen from a closed set, matching the server's own discipline: an error
 * message is an untrusted output surface and must never carry a driver message,
 * a path or a stack frame.
 */
function fallbackMessageFor(status: number): string {
  if (status === 0) return 'The server could not be reached.';
  if (status === 401) return 'Sign in to continue.';
  if (status === 403) return 'This account cannot perform that action.';
  if (status === 404) return 'Not found.';
  if (status === 429) return 'Too many requests. Try again shortly.';
  if (status >= 500) return 'The server failed to answer.';
  return 'The request was rejected.';
}

/** A transport failure — DNS, a dropped connection, a CORS refusal. */
export function networkError(cause: unknown): ApiError {
  return new ApiError({
    status: 0,
    code: 'network',
    message: fallbackMessageFor(0),
    cause,
  });
}

/** The request exceeded its own deadline. */
export function timeoutError(milliseconds: number, cause?: unknown): ApiError {
  return new ApiError({
    status: 0,
    code: 'timeout',
    message: `The server did not answer within ${String(milliseconds)}ms.`,
    cause,
  });
}

/** The caller aborted — a navigation, a superseded query. Not a failure. */
export function abortedError(cause?: unknown): ApiError {
  return new ApiError({
    status: 0,
    code: 'aborted',
    message: 'The request was cancelled.',
    cause,
  });
}

/** A 2xx whose body did not decode as JSON. */
export function malformedResponseError(cause?: unknown): ApiError {
  return new ApiError({
    status: 0,
    code: 'malformed_response',
    message: 'The server sent a response this client could not read.',
    cause,
  });
}

/**
 * The sentence to put in front of a user for any thrown value, including one
 * that is not an `ApiError` at all. Never returns an empty string, and never
 * leaks a stack.
 */
export function userFacingMessage(error: unknown): string {
  if (isApiError(error)) return error.message;
  return 'Something went wrong.';
}

/**
 * The developer-facing detail for any thrown value, or `null` when there is
 * nothing useful to show. Pair it with `userFacingMessage`.
 */
export function developerDetail(error: unknown): string | null {
  if (isApiError(error)) return error.detail;
  if (error instanceof Error && error.message !== '') return error.message;
  return null;
}
