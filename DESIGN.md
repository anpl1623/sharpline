# Design System — Sharpline

**Instrument Dark.** Created 2026-08-17 by `/design-consultation`.

This file is the source of truth for every visual decision in `web/`. Read it before
writing a component. Do not deviate without explicit approval — record any approved
deviation in the Decisions Log at the bottom.

---

## The governing idea

**The resting state has near-zero visual energy, so that change carries all of it.**

Every constraint below exists to serve one outcome: a price moving is the loudest
event in the viewport. A board that is colourful at rest cannot make a moving price
loud. Restraint here is not taste — it is the mechanism.

The one thing a first-time viewer should remember 10 seconds after seeing Sharpline:
**the numbers are alive.**

---

## Product Context

- **What this is:** A real-time sports odds platform. Odds are ingested, normalized,
  priced, and streamed live to the browser over WebSocket, computed entirely on
  self-hosted hardware.
- **Who it's for:** Two audiences, served by one surface. It must read as a
  **faithful consumer sportsbook first** — a bettor navigates it without instruction —
  and as an **engineering instrument second**, where staleness, provenance, and
  sequence state are visible to anyone who looks.
- **Space:** Sportsbook / live-odds. Category peers: FanDuel, DraftKings, Pinnacle.
  Adjacent reference field: trading terminals and prediction markets.
- **Project type:** Dense live-data web application. Dark mode only.
- **Non-negotiable content:** the "simulation, not a licensed sportsbook" disclaimer
  (CLAUDE.md §0) appears on the landing page and survives every redesign.

---

## Aesthetic Direction

- **Direction:** Industrial / Utilitarian
- **Decoration level:** minimal — typography and rules do all the work
- **Mood:** An instrument, not a casino. Quiet, dense, precise. Nothing decorative,
  nothing glowing, nothing celebratory. The product's energy is entirely in its data.
- **Reference sites:** none — no competitive research was run this session. The
  category conventions honored below are drawn from design knowledge, not audit.

---

## Typography

**Typeface is how the two audiences are separated.** The consumer surface is sans;
the engineering layer is mono. There is no mode switch and no debug drawer — the
register change is a texture change, readable in peripheral vision.

| Role | Font | Rationale |
|---|---|---|
| Display / hero / league heads | **Clash Grotesk** 500–600 | Flat-sided, wide confident digits, institutional at Semibold. Scoped narrowly: landing poster and section heads only. First family to cut if a two-family system is ever wanted. |
| Body / UI / **all prices** | **Instrument Sans** 400–700 | Neo-grotesque, slightly narrow, technical without being cold. True tabular lining figures; `0/O` and `1/l` never collide at 13px; strong `+`/`−`. |
| Data / tables | **Instrument Sans**, `font-variant-numeric: tabular-nums` | Same family. Hierarchy comes from weight and size, never a second face. |
| Engineering layer + code | **JetBrains Mono** 400–500 | Deliberately different texture. Every mono glyph on screen means *the machine is talking*. |

**Prices are NOT monospace.** Mono buys column alignment, but `tabular-nums` buys
identical alignment at ~15% less width, and sans keeps the board reading as a consumer
book rather than a terminal. Mono on prices would collapse the two-register system.

**Explicitly rejected:** Geist (Vercel's face on a Next.js app — reads as
`create-next-app` default), Inter, Space Grotesk, Roboto, system-ui as a primary face.

### Loading

No font CDN reaches the browser. This project computes on hardware the author controls;
fonts follow the same rule.

- Instrument Sans, JetBrains Mono → `next/font/google` (self-hosted at build time)
- Clash Grotesk → Fontshare woff2 committed to `web/public/fonts/`, loaded via
  `next/font/local`

### Scale

```
display   clamp(44px, 6vw, 76px) / 0.95   600   Clash Grotesk   −0.02em
h1        32px / 1.15   650
h2        24px / 1.25   600
h3        18px / 1.30   600
price-lg  24px / 1.00   600   tabular      event detail hero price
price     15px / 1.00   550   tabular      board cell — the workhorse
price-sm  13px / 1.00   550   tabular      parlay legs, bet slip
body      15px / 1.50   400
ui        13px / 1.35   500
label     11px / 1.20   600   0.08em, uppercase
mono      12px / 1.40   400   JetBrains Mono — status rail, provenance
```

---

## Color

**Approach: restrained.** Five hues exist in the entire product.

One rule, and it is strict:
**green means money · cyan and amber mean direction · red means something is wrong.**
No colour ever does two jobs. **No price is ever green or red.**

### Ground & ink

| Token | OKLCH | Hex | Use |
|---|---|---|---|
| `ground-0` | `oklch(0.145 0.006 260)` | `#0B0D12` | Page |
| `ground-1` | `oklch(0.185 0.008 260)` | `#14171E` | Surface, card, row |
| `ground-2` | `oklch(0.225 0.009 260)` | `#1D212A` | Raised, price cell |
| `ground-3` | `oklch(0.265 0.010 260)` | `#262B35` | Input well |
| `rule` | `oklch(0.32 0.010 260)` | `#333945` | Hairline |
| `rule-hi` | `oklch(0.40 0.012 260)` | `#454C5A` | Emphasized border |
| `ink` | `oklch(0.96 0.004 260)` | `#F2F4F8` | Primary — ~17:1 |
| `ink-2` | `oklch(0.78 0.008 260)` | `#B4BAC6` | Secondary — ~9:1 |
| `ink-muted` | `oklch(0.62 0.010 260)` | `#838B9A` | Muted — ~5.5:1, AA body ✓ |
| `ink-faint` | `oklch(0.48 0.010 260)` | `#5B6270` | ~3.1:1 — **decorative / disabled only, never body text** |

### Meaning

| Token | OKLCH | Hex | Use |
|---|---|---|---|
| `delta-out` | `oklch(0.78 0.13 210)` | `#4FC3E8` | Implied probability **fell** — price lengthened |
| `delta-in` | `oklch(0.80 0.14 70)` | `#F0A93B` | Implied probability **rose** — shortened, steam |
| `money` | `oklch(0.76 0.16 155)` | `#3ECF8E` | Stake, payout, place-bet CTA, settled win |
| `loss` | `oklch(0.65 0.19 25)` | `#F0563E` | Error, settled loss. **Never a price.** |
| `info` | `oklch(0.72 0.10 250)` | `#7B9BE8` | Resync, suspension, neutral system state |

### Why cyan/amber and not green/red

A deliberate departure from every book in the category:

- Direction-of-change *is* the information on this product, and red/green is the exact
  failure case for the ~8% of men with deuteranopia. Blue↔orange is the canonical
  colourblind-safe axis.
- Both hold at low chroma on a near-black ground, where saturated red vibrates.
- Amber = heat maps onto steam, a headline feature (§6, §9).
- It frees green to mean *money* and nothing else, everywhere in the product.

**Cost, accepted:** green-up/red-down is real muscle memory for anyone who bets or
trades. Mitigated because direction is *also* carried by an arrow glyph and by the
numeral itself — colour is redundant, never load-bearing alone.

### Signals

`+EV` and steam use a low-alpha tinted badge (8% fill, 40% border) in their hue.
**Arbitrage is the only saturated fill in the entire interface** — full `money` background,
`#06251A` text. It is rare enough to earn a shout, and making it the single loudest
object on screen means it is never missed and never imitated by anything else.

### Dark mode

**Dark only.** This is not a dark theme over a light one; there is no light palette.
A light theme would be a separate design decision and is currently out of scope
(see Open Decisions).

---

## Spacing

- **Base unit:** 4px
- **Density:** two densities on purpose — **compact** for the board, **comfortable**
  everywhere else
- **Scale:** `2xs 2 · xs 4 · sm 8 · base 12 · md 16 · lg 24 · xl 32 · 2xl 48 · 3xl 64`

| Measure | Value |
|---|---|
| Board row height | 36px |
| Board price cell | 15px tall, 2px gap, stacked two per market |
| Board tap target | see the note below — 44px is not achievable at a 36px row |
| Non-board control height | 44–48px |

**The board tap target does not add up, and this is the correction.** A 36px row
holding two stacked 15px cells with a 2px gap leaves each cell a box roughly 17px
tall — 44px cannot be "carried by padding" out of 36px of row when two targets
share it. The 44×44 figure is WCAG 2.2 SC 2.5.5, which is **AAA**; the rest of
this file targets AA (`ink-muted` is annotated "AA body ✓"). The AA requirement is
SC 2.5.8 Target Size (Minimum), **24×24 CSS px**.

Where that leaves the board, stated plainly rather than rounded up:

| Surface | Price-cell target | SC 2.5.8 (24×24, AA) |
|---|---|---|
| Board, < 768px (56px row) | ~88 × 27px | **met** |
| Board, ≥ 768px (36px row) | ~88 × 17px | **not met vertically** |
| Every non-board control | 44–48px | met, and AAA too |

Touch — the modality target size exists for — is the 56px row, and it clears the
AA minimum. The shortfall is confined to the pointer-driven desktop board, where
the density is the product. Closing it means a ~52px row (two 24px cells + gap +
padding), which is a 44% taller board and a different product. **That trade is a
design decision, not a code fix, and it is Open Decision #6.** Until it is taken,
the board renders the largest honest target it can — the full market-column width
by half the row height, with the two stacked links never overlapping — and this
file does not claim a number it does not hit.


---

## Layout

- **Approach:** hybrid — grid-disciplined for the app, poster for the landing page
- **Grid:** 12-column at ≥1280px, 8 at ≥768px, 4 below
- **Board width:** full bleed, no max-width. Density is the point.
- **Content max width:** 1200px · **prose:** 68ch
- **Board columns:** Game | Moneyline | Spread | Total, each market column holding two
  stacked price cells
- **Bet slip:** right rail ≥1000px, bottom sheet below

### The mobile board (< 768px) — resolved 2026-08-19

Open Decision #4 said the mobile board was "specified but not designed" and needed its own
pass before phase 7 mobile work. This is that pass. It resolves the two pieces phase 7
owns; the third — the bottom-sheet bet slip — is deferred with a stated reason.

**The problem.** The desktop board is `Game | Moneyline | Spread | Total`, three market
columns each holding two stacked price cells. At 375px that is six 15px prices plus a
game name in ~340px of usable width: ~42px per cell. Below the 44px tap target, below
readable at the `price` step, and the delta rail — 2px of a 42px cell — stops reading as
a rail and starts reading as a border artefact.

**Resolved: the table survives, the viewport scrolls it.** At < 768px the board keeps
its `<table>`, keeps all three market columns, and keeps every price cell at its full
desktop size. The `Game` column becomes `position: sticky; left: 0` and the market
columns scroll horizontally inside an `overflow-x: auto` wrapper. The row grows from
36px to 56px so the two competitors stack instead of truncating.

Rejected: **card-per-event**, the category's usual mobile answer — one card per game with
the markets stacked vertically inside it. It is rejected on three counts, and the first is
the one that matters:

1. **It destroys the column scan.** A board is a table because the eye compares one
   market across many games in a single vertical sweep. Stacking markets inside a card
   turns that sweep into a paging operation. The governing idea of this system is that a
   moving price is the loudest event in the viewport; a layout where only one game is
   ever in the viewport has nothing to be loud *against*, and the recency gradient the
   decay rail exists to produce cannot form at all.
2. **It breaks the accessibility contract.** "Every price is a table cell with an
   accessible name (market, selection, price)" is a structural promise. A card grid
   re-implements that with ARIA and gets it subtly wrong.
3. **It triples scroll length** for the same information.

The accepted cost is real and is named: **horizontal scrolling is a worse gesture than
vertical scrolling**, and a market column can be off-screen at rest. It is mitigated by
the sticky game column (the row is never orphaned from its identity), by the columns
being ordered by demand — moneyline first.

**Scroll-snapping on the column boundary was specified here and has been dropped.** It does
not compose with a sticky first column: a snap point aligns against the scrollport's start
edge, which a `position: sticky` cell does not move, so the board loaded already scrolled
right with the Moneyline column painted underneath the game cell. `scroll-padding` is the
documented fix and fails too, because it takes a length while the game column's width is
content-driven. A partial column at rest is a nicety; a hidden first column is a defect.
The reasoning is repeated at the CSS in `globals.css` so nobody re-adds it.

| Measure | ≥ 768px | < 768px |
|---|---|---|
| Board row height | 36px | 56px (two-line game cell) |
| Price cell | unchanged | unchanged — this is the point |
| Game column | static | `sticky left: 0`, 132px, `ground-1` backdrop |
| Market columns | fit | `overflow-x: auto` (see note on snapping) |
| Status rail | 24px mono rail, persistent | collapsed to a pip — below |

**Resolved: the collapsed status pip.** The persistent 24px mono status rail is 6% of a
812px viewport and it is the first thing to cut. At < 768px it collapses to a single 8px
pip in the header — the ONLY full-radius object in the product, alongside avatars, exactly
as the radius table already allows.

The pip carries connection state by fill, and **never by fill alone**: it has an
accessible name that states the same fact in words, so the colour is redundant for a
screen reader and for a colourblind reader both.

| State | Fill | Accessible name |
|---|---|---|
| Streaming | `money` | "Live — streaming" |
| Resyncing | `info` | "Resyncing" |
| Reconnecting | `info`, 1.2s pulse | "Reconnecting" |
| Disconnected | `loss` | "Disconnected" |
| Idle / no socket | `ink-faint` | "Not connected" |

`money` on the pip is not a violation of "green means money". The pip is not a price and
carries no quantity; it is the one place in the product where green means *this machine is
working*, and it is the same judgement that already puts green on the place-bet CTA. It is
listed here so it is a decision rather than a drift.

Tapping the pip expands the full rail content — connection id, sequence number, channel
count, staleness, provenance — into a 6px-radius sheet from the top. Same mono register,
same values, same order as the desktop rail. Nothing is *lost* on mobile, only folded.

**Deferred, with a reason: the bottom-sheet bet slip.** Open Decision #4 also named the
slip. The bet slip does not exist — CLAUDE.md §11 puts wagering in phase 8 — and designing
a container for a thing with no contents produces a spec that phase 8 will discard. The
sheet's mechanics (6px radius, 180ms `short` enter on the `enter` curve, drag-to-dismiss,
focus trap) are settled by the motion and radius systems already; what is undesigned is
its *content*, and that is phase 8's to design against a real slip. Open Decision #4 is
therefore **closed for the board and the status pip, and re-opened narrowly as Open
Decision #5** for the slip alone.

### Border radius — hierarchical and small

Uniform bubble-radius is the primary AI-slop tell. Radii here are small and they mean
something.

```
price cell, input   2px    ← effectively square; this is the density signal
card, row group     4px
modal, sheet        6px
pip, avatar         9999px  ← the ONLY full radius in the product
```

---

## Motion

**Approach: minimal-functional everywhere, with the entire budget spent on one element.**

No entrance animations. No scroll-driven anything. No page transitions. That restraint
is what makes a 2px rail lighting up the most eventful thing in the viewport.

```
micro    80ms    hover, focus
short   180ms    digit roll, sheet
medium  240ms    drawer
decay  2500ms    delta rail ONLY — nothing else may use this duration

enter   cubic-bezier(0.16, 1, 0.3, 1)      expo-out, snappy
exit    ease-in 120ms
decay   cubic-bezier(0.7, 0, 0.84, 0)      holds bright, then drops fast
```

### The delta rail — the signature element

The category standard is a full-cell background flash for ~300ms. On a board with
hundreds of cells updating that is a strobe — the exact "noise, not information" failure
CLAUDE.md §7 names. Instead, every price cell carries a 2px vertical rule on its
leading edge:

1. **0ms** — rail snaps to full chroma, no transition in. Hard onset the eye catches
   peripherally.
2. **180ms** — the numeral does a single-line vertical roll, old digit out, new in.
3. **2500ms** — rail decays on `cubic-bezier(0.7, 0, 0.84, 0)`, holding bright then
   dropping fast.

The payoff: across the whole board the decay reads as a **recency gradient**. A glance
shows which markets moved in the last few seconds without reading a single number.

**Implementation constraints:**
- `transform` and `opacity` only. Never animate layout.
- Per-cell decay timers; do not re-render the row.
- `prefers-reduced-motion`: kill the digit roll (instant swap), shorten the rail to a
  400ms linear fade. Do not remove the rail — it carries information.

---

## Accessibility

CLAUDE.md §7 is explicit about this and it is not optional.

- Every price is a table cell with an accessible name (market, selection, price).
- Price deltas go to a **throttled** `aria-live="polite"` region batching to **one
  announcement per 5s** — e.g. "14 markets moved". **Never fire a live region per tick.**
- Individual price changes are exposed via `aria-describedby` on focus, not announced.
- Colour is never the sole carrier of direction: an arrow glyph and the numeral itself
  both encode it.
- `ink-faint` (3.1:1) is barred from body text. Decorative and disabled states only.
- Every price cell is keyboard focusable with a visible focus ring on `ink-muted`
  (was `rule-hi`, which measures ~2.25:1 and fails WCAG 2.2 SC 1.4.11 — see the
  Decisions Log entry of 2026-08-19).

---

## Category conventions deliberately kept

These are safe on purpose. Innovating here buys nothing and costs literacy.

1. **Dark ground, dense league-grouped table, markets as columns.** Every book does
   this and users navigate it without thinking.
2. **Persistent bet slip** with the price-change accept/reject interstitial.
3. **Green primary CTA on place-bet.** The one irreversible click stays instantly legible.
4. **American odds default,** with a format toggle in the header.

---

## Open Decisions

1. **Light theme** — currently out of scope. Dark is not "primary", it is the only
   theme. Revisit only with a stated reason.
2. **Clash Grotesk** — narrowly scoped to display. If a two-family system is preferred,
   this is the one to cut; Instrument Sans absorbs the display role at 650.
3. **Board full-bleed vs. capped** — spec says full bleed. Validate against a real
   ultrawide before locking.
4. **Mobile board** — ~~RESOLVED 2026-08-19~~. See "The mobile board (< 768px)" under
   Layout: sticky game column with horizontally scrolling market columns, and the status
   rail collapsed to a pip. The slip half of this decision is re-opened as #5.
5. **Mobile bet slip (bottom sheet)** — ~~RESOLVED 2026-08-20~~. Phase 8 built the slip
   and designed its content. One panel implementation serves both containers — right rail
   at ≥1000px, bottom sheet below — with drag-to-dismiss on the handle only. See the
   Decisions Log entries of 2026-08-20.
6. **Desktop board row height vs. WCAG 2.2 SC 2.5.8** — the 36px row misses the AA 24×24
   target minimum vertically (see Spacing). Options: raise the row to ~52px and lose 44%
   of the density the board exists for; keep 36px and accept a documented AA shortfall on
   pointer devices only; or ship both behind the compact/comfortable density switch this
   file already contemplates. Needs a decision. Mobile already meets it.

---

## Preview

A live, self-contained preview of this system — real fonts, real palette, a ticking
board with working delta rails — lives at:

```
~/.gstack/projects/anpl1623-sharpline/designs/design-system-20260817/preview.html
```

---

## Decisions Log

| Date | Decision | Rationale |
|---|---|---|
| 2026-08-17 | Initial design system created | `/design-consultation`. No competitive research run; worked from design knowledge and the CLAUDE.md charter. |
| 2026-08-17 | Serve both audiences from one surface — consumer book first, instrument second | Splitting them would make two products. Texture (sans vs. mono) separates the registers instead. |
| 2026-08-17 | Memorable thing: "the numbers are alive" | The only claim a static screenshot cannot make, and the one that is literally true of this system. |
| 2026-08-17 | Cyan/amber delta instead of green/red | Colourblind-safe axis, holds at low chroma on near-black, frees green for money. Accepted cost: overrides trader muscle memory. |
| 2026-08-17 | Delta rail with 2500ms decay instead of a 300ms cell flash | A flash on a dense board is a strobe. Decay adds a second information channel: how long ago. |
| 2026-08-17 | Engineering layer as a permanent 24px mono status rail, not a debug drawer | Satisfies "shows its work" without a second product. Costs 24px of permanent vertical space. |
| 2026-08-17 | Prices set in Instrument Sans with `tabular-nums`, not mono | Same alignment, ~15% denser, and keeps the consumer read. |
| 2026-08-17 | Dark only, no light palette | Charter §7 makes dark primary; a light theme is a separate decision nobody has asked for. |
| 2026-08-19 | Open Decision #4 resolved for the board and the status pip; the slip half re-opened as #5 | The board and the pip are phase 7 and were blocking it. A container for a bet slip that does not exist yet would be a spec phase 8 discards. See "The mobile board (< 768px)". |
| 2026-08-19 | **DEVIATION, approved:** focus ring moves from `rule-hi` to `ink-muted` | `rule-hi` measures ~2.25:1 on `ground-0`, ~1.86:1 on `ground-2` and ~1.4:1 against a control's own `rule` border — all below the 3:1 WCAG 2.2 SC 1.4.11 requires of a focus indicator, and near-invisible on a bordered control. This file's own Accessibility section says accessibility here "is not optional", so it contradicted itself and the accessibility half wins. `ink-muted` clears 3:1 on every ground a focused control can sit on (5.68 / 4.69 / 3.36) and is already a sanctioned foreground token, so no sixth hue enters. One token in `globals.css` reverts it. |
| 2026-08-19 | Board tap target corrected from 44px to the real arithmetic; AA 24×24 named as the target | 44px (SC 2.5.5, AAA) is unreachable from a 36px row shared by two stacked cells, and a spec that states an impossible number is worse than one that states a shortfall. Mobile's 56px row meets AA; the desktop row does not, and that trade is Open Decision #6 rather than a silent redesign. |
| 2026-08-20 | **Open Decision #5 CLOSED.** The slip's content is ONE panel, rendered unchanged in the rail (≥1000px) and in the bottom sheet (below) | The rejected alternative was a compact mobile variant showing fewer facts. It fails on the same grounds card-per-event failed for the mobile board: a slip that shows less on a phone asks somebody to commit money with less information, and the small screen is where that matters most. What differs between the two containers is chrome — a header, a grab handle — not content. Below 1000px the rail is `display:none` rather than unmounted, which is hydration-safe where a media-query hook is not; the cost is that every id the panel emits must be per-instance (`useId`), and it is. |
| 2026-08-20 | Bottom sheet: drag-to-dismiss on the HANDLE ONLY, 96px threshold, `transform` written straight to the DOM | Dragging anywhere on the surface fights the leg list's own scrolling, and a sheet that closes when somebody tries to scroll their four-leg parlay is worse than one that does not drag at all. The offset bypasses React because a `pointermove` fires at the refresh rate and routing each one through state would re-render the whole slip sixty times a second while a finger moves. Only `transform` is animated; the 180ms `enter` glide-back is the same curve everything else uses. |
| 2026-08-20 | **NEW ELEMENT: the "on the slip" trailing rule.** A price cell on the slip carries a 2px rule on its TRAILING edge, mirroring the delta rail on the leading one | It spends NO HUE, and that is the decision rather than an oversight. Five colours exist and each does one job; "this is on my slip" is not direction, not money, not wrong and not system state, and giving one of them a second job is exactly what the colour rule forbids. `ink-2` is a foreground weight rather than a meaning, so the mark reads as a mark and the board stays quiet at rest. The two edges never collide: leading is movement and decays, trailing is intent and persists. Colour is not the sole carrier — the cell is a toggle with `aria-pressed` whose accessible name says "on the bet slip". |
| 2026-08-20 | The price cell becomes a TOGGLE, not a link to the event | Clicking a price is how every book in the category adds a selection, and this file already keeps that convention deliberately. Nothing is lost: the game cell already links to the event on the board and in every league group, and one oddity goes away — on the event page the price cell used to link to the page it was already on. Only a quoted, OPEN price is interactive, which is what stops the slip ever holding a leg the book is not offering. |
| 2026-08-20 | ONE green figure per slip — the potential payout — plus the green CTA, rather than green on every money value | § Color permits `money` on stake and payout both. Using it on both spends the highlight twice: green is a highlight, and a panel with three green numbers has highlighted nothing. The payout is the number somebody is playing for, so it takes the colour, and it and the place-bet button read together as "this is the money, and this is the button that commits to it". The tempting mistake this rule exists to prevent is the reverse one — greening the ticket PRICE because it was computed from the same legs. A payout is money and may be green; `decimal_odds` is a price and may not, on the slip, on the receipt or on a placed ticket. |
| 2026-08-20 | The slip's price-move announcement is a SECOND live region, and is allowed to be | § Accessibility bans a live region that fires per tick, and the app-wide one in `components/live` is throttled to one batched sentence every five seconds. This one is different in kind: its text is a function of HOW MANY LEGS ARE BLOCKING, so a market that moves the same leg forty times produces one announcement, and it is silent unless something is actually blocking. It is required — a keyboard user whose Place button silently goes dead has no way to discover why. The panel is mounted twice below 1000px but only one copy is ever in the accessibility tree: `display:none` removes the rail, and the sheet exists only while open. |
