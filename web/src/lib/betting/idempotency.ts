/**
 * The `Idempotency-Key` a submit carries, and the rule for when it rotates.
 *
 * # What the key is for
 *
 * The wager identifier is derived deterministically from `(user, this key)` — and,
 * for a round robin, additionally from the combination index. A replayed submit
 * therefore attempts to INSERT the primary key it already inserted and is refused
 * by the database rather than by a lookup that could race; the service reads the
 * existing wager back and answers `200` with it. That is a property of a unique
 * index, which is why a retry is safe even when the first attempt's response
 * never reached the client at all.
 *
 * # ONE KEY PER ATTEMPT, NOT PER CLICK — and the asymmetry that sets the rule
 *
 * The key is minted once per slip and is REUSED across every submit of that slip,
 * including a resubmit after a `409 price_moved` and a retry after a timeout. It
 * rotates in exactly one place: when the slip is emptied, which is what a
 * successful placement does.
 *
 * The tempting alternative is to rotate whenever the slip changes, so that an
 * edited slip is "a different intended action". It is rejected, and the reason is
 * that the two ways of being wrong are not remotely symmetric:
 *
 *   - Reusing a spent key books NOTHING and returns the original ticket, flagged
 *     `replayed: true`. The customer sees a ticket they did place, and the slip
 *     says so in as many words.
 *   - Rotating too eagerly books a SECOND BET. A submit that times out has an
 *     unknown outcome; if the customer nudges the stake and presses again with a
 *     fresh key, the first attempt — which may well have landed — is now a bet
 *     nobody knows about, and money has moved twice for one intention.
 *
 * One of those is a confusing screen and the other is a double charge, so the
 * rule falls out: rotate as rarely as correctness allows, and never on an edit.
 *
 * # The key is not a secret and is not user input
 *
 * It identifies a submit, it is generated locally, it never leaves this origin
 * except as a request header, and it carries no information about the slip. The
 * server bounds it at 255 bytes and rejects a missing or empty one with a `400`.
 */

/**
 * A fresh key for one slip attempt.
 *
 * `crypto.randomUUID` where it exists — every browser this app targets, and it
 * is a v4 UUID from the platform CSPRNG. The fallback is not a weaker random
 * number dressed up as a UUID: it is `getRandomValues` over 16 bytes, hex
 * encoded, which is the same 128 bits from the same generator without the
 * version nibbles. Neither path uses `Math.random`, because a key collision
 * between two tabs of the same account would make one submit silently return the
 * other's ticket.
 *
 * There is no third fallback. If neither API is present this throws, and that is
 * correct: without a source of randomness there is no safe key, and placing a
 * bet with an unsafe one is worse than not placing it.
 */
export function newIdempotencyKey(): string {
  const source = globalThis.crypto;
  if (typeof source?.randomUUID === 'function') {
    return source.randomUUID();
  }

  const bytes = new Uint8Array(16);
  source.getRandomValues(bytes);
  let out = '';
  for (const byte of bytes) out += byte.toString(16).padStart(2, '0');
  return out;
}

/**
 * A key placeholder for a render that has no key yet.
 *
 * The store mints its real key lazily — `crypto` is unavailable while the store
 * module is evaluated during server rendering, and a key generated there would
 * be shipped in the HTML and shared by every viewer of that page. This constant
 * is what the slip holds until the first client interaction, and it is never
 * sent: the placement path refuses to submit while the key is this value.
 */
export const NO_IDEMPOTENCY_KEY = '';

/** Whether a key is real and may be presented. */
export function isUsableIdempotencyKey(key: string): boolean {
  return key !== NO_IDEMPOTENCY_KEY && key.length <= 255;
}
