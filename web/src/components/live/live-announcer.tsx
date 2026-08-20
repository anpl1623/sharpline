'use client';

/**
 * THE single `aria-live` region for the whole application. Mounted once, in the
 * root layout, and never per page.
 *
 * # Why it is throttled, in one sentence
 *
 * Firing a live region on every price tick is the single worst thing this
 * interface could do to a screen reader user: a board with a hundred moving
 * markets would produce a continuous, unintelligible stream of interruptions and
 * make the page unusable — not degraded, unusable.
 *
 * So the region says one batched sentence at most once every five seconds:
 * "14 markets moved". The batching and the clock live in
 * `useMarketsMovedAnnouncement`; this component is the region and nothing else.
 *
 * Individual price changes are NOT announced. They are exposed through
 * `aria-describedby` on the focused cell, where they are read on demand rather
 * than pushed — DESIGN.md § Accessibility.
 *
 * # Silence is a state
 *
 * An idle board emits the empty string, and the empty string renders NO child at
 * all. It never says "0 markets moved", and it never repeats itself into a
 * screen reader for a page that is doing nothing.
 *
 * # Why the child is keyed and the region is not
 *
 * Assistive technology does not reliably re-announce a region whose text is
 * replaced with the SAME text, and two consecutive five-second windows can
 * easily both read "14 markets moved". Keying the child forces React to replace
 * the text node, which the announcement machinery does notice.
 *
 * Keying the REGION itself would work too, but it would tear down and rebuild
 * the live region on every announcement, and a live region has to exist in the
 * accessibility tree *before* its content changes to be observed at all. The
 * container therefore stays put for the lifetime of the app; only its contents
 * churn.
 */

import { useMarketsMovedAnnouncement } from '@/lib/ws/provider';

export function LiveAnnouncer() {
  const { message, key } = useMarketsMovedAnnouncement();

  return (
    <div
      className="sr-only"
      aria-live="polite"
      aria-atomic="true"
      /* Not `role="status"`: `status` is an implicit live region in its own
       * right and doubling them makes some screen readers announce twice. */
    >
      {message === '' ? null : <span key={key}>{message}</span>}
    </div>
  );
}
