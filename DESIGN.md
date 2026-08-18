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
| Board tap target | 44px (padding carries it) |
| Non-board control height | 44–48px |

---

## Layout

- **Approach:** hybrid — grid-disciplined for the app, poster for the landing page
- **Grid:** 12-column at ≥1280px, 8 at ≥768px, 4 below
- **Board width:** full bleed, no max-width. Density is the point.
- **Content max width:** 1200px · **prose:** 68ch
- **Board columns:** Game | Moneyline | Spread | Total, each market column holding two
  stacked price cells
- **Bet slip:** right rail ≥1000px, bottom sheet below

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
- Every price cell is keyboard focusable with a visible focus ring on `rule-hi`.

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
4. **Mobile board** — the bottom-sheet slip and collapsed status pip are specified but
   not designed. Needs its own pass before phase 7 mobile work.

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
