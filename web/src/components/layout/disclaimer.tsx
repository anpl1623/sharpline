/**
 * The "simulation, not a licensed sportsbook" statement.
 *
 * CLAUDE.md §0 and DESIGN.md § Product Context both make this non-negotiable
 * content: it appears on the landing page and it survives every redesign. It
 * lives in its own module so there is exactly ONE copy of the wording in the
 * codebase and any surface that needs it — the landing poster, the status rail,
 * a future footer — quotes the same sentences rather than paraphrasing them.
 *
 * The prose below is carried over verbatim from the phase-0 landing page. It was
 * restyled here, not rewritten: an unlicensed real-money book is a legal
 * liability, and softening the wording of the thing that says this is not one is
 * the only edit to this file that would be a defect.
 */

import { cn } from '@/lib/utils';

/** The heading, so the rail and the poster cannot drift apart. */
export const DISCLAIMER_HEADING = 'Not a licensed sportsbook';

/**
 * The one-line form, for the engineering rail where there are 24 pixels and no
 * room for a paragraph. Upper case because it sits in the mono register.
 */
export const DISCLAIMER_SHORT = 'SIMULATION — NOT A LICENSED SPORTSBOOK';

export interface DisclaimerProps {
  readonly className?: string | undefined;
  /**
   * The id given to the heading, so a caller can point `aria-labelledby` at it.
   * Defaults to a stable literal because there is only ever one of these on a
   * page.
   */
  readonly headingId?: string | undefined;
}

export function Disclaimer({
  className,
  headingId = 'disclaimer-heading',
}: DisclaimerProps) {
  return (
    <section
      aria-labelledby={headingId}
      className={cn('flex flex-col gap-3 border-t border-rule pt-6', className)}
    >
      <h2 id={headingId} className="t-label text-ink-muted">
        {DISCLAIMER_HEADING}
      </h2>
      <p className="max-w-prose t-body text-ink-2">
        Sharpline is a sportsbook{' '}
        <strong className="font-semibold text-ink">simulation</strong> built as
        an engineering project. No real money moves through it. All wagering is
        play-money against a double-entry ledger, and there is no KYC, no
        geolocation gating, no payment processing, and no custody of funds.
      </p>
    </section>
  );
}
