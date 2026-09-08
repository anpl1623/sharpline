'use client';

/**
 * An empty slip.
 *
 * # It is a CORRECT state and must not look like a fault
 *
 * The same rule the board's empty state follows: no warning colour, no icon, no
 * apology. Nobody has picked a price yet, which is the state every session
 * starts in. What it does instead is say the one thing a first-time viewer needs
 * — that a price on the board IS the control — because the affordance is not
 * obvious from a board of numbers, and a slip that says only "empty" leaves the
 * product's central interaction undiscovered.
 *
 * # Nothing is invented to fill it
 *
 * No suggested bets, no popular parlays, no example ticket. CLAUDE.md's rule
 * against fabricated data is usually read as being about odds, and it is exactly
 * as binding here: an "example" leg in an empty slip is a fabricated wager on a
 * fabricated market, and it is worse than an empty panel because somebody will
 * screenshot it.
 *
 * # The disclaimer belongs here
 *
 * CLAUDE.md §0 requires the "simulation, not a licensed sportsbook" statement to
 * survive every redesign. The landing page carries it; so does the one panel in
 * the product that asks somebody to stake something, at the moment they have not
 * yet staked anything.
 */

export function SlipEmpty() {
  return (
    <div className="flex flex-col items-start gap-3 px-4 py-8">
      <h3 className="t-h3 text-ink">Your slip is empty</h3>
      <p className="t-body text-ink-2">
        Select a price on the board or on an event to put it here. Selecting it
        again takes it off.
      </p>
      <p className="t-ui text-ink-muted">
        Two or more selections can be a parlay, a round robin or a teaser.
      </p>
      <p className="t-ui text-ink-muted">
        Play money. This is a simulation, not a licensed sportsbook, and no real
        money moves.
      </p>
    </div>
  );
}

/**
 * The slip before its stored contents have been read.
 *
 * A distinct state from empty, and the distinction matters for exactly one
 * frame: an unread slip and an empty one look identical, so rendering the empty
 * copy first would flash "Your slip is empty" at somebody who has six legs on
 * it. It holds the panel's shape and says nothing at all.
 */
export function SlipUnread() {
  return (
    <div className="px-4 py-8" aria-hidden="true">
      <div className="h-4 w-32 rounded-price bg-ground-2" />
    </div>
  );
}
