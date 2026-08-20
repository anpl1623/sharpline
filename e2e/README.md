# `e2e/` — Playwright critical path

End-to-end tests for the phase 7 critical path, run as a one-shot container
against the live compose stack through the proxy.

```
make e2e
```

That is the whole interface. There is no other supported way to run this, and
running it any other way breaks the prime directive.

---

## How it actually runs

```
make e2e
  └─ docker compose --profile e2e ... run --rm e2e
       ├─ image:   deploy/docker/playwright.Dockerfile
       │             mcr.microsoft.com/playwright:v1.62.1-noble, digest-pinned
       ├─ user:    1001:1001, non-root
       ├─ mount:   repo e2e/ → /workspace   (bind, read-write)
       ├─ env:     PLAYWRIGHT_BASE_URL=http://proxy
       └─ needs:   service `proxy`, healthy
```

`http://proxy` is the Caddy reverse proxy on the internal compose network — the
**only** published entrypoint in the system (CLAUDE.md §9). These tests reach the
app exactly the way a browser does: `/` and `/board` to Next, `/api/v1/*` to the
Go API, `/ws` to the stream gateway. No spec ever addresses `api:8080`,
`stream:8081` or any other container hostname.

**There is no `webServer` block in `playwright.config.ts`, deliberately.** The
stack is already up. Letting Playwright start a server would put a process
outside a container of its own, which is the one exception CLAUDE.md's prime
directive does not grant.

### Overriding the target

```bash
PLAYWRIGHT_BASE_URL=https://sharpline.localhost make e2e
```

`ignoreHTTPSErrors: true` is set because Caddy issues its own internal-CA
certificate in dev.

### Reports

Playwright writes back through the bind mount as uid 1001:

| Path | What |
|---|---|
| `e2e/playwright-report/` | HTML report — open `index.html` |
| `e2e/test-results/` | traces, screenshots, `junit.xml` |

Traces are captured `on-first-retry`, screenshots `only-on-failure`. All of it is
git-ignored.

---

## VERSION LOCKSTEP — read before touching `package.json`

Three things must read **exactly** `1.62.1`, with no caret and no range:

1. the image tag in `deploy/docker/playwright.Dockerfile`
   (`mcr.microsoft.com/playwright:v1.62.1-noble`)
2. the global `@playwright/test` that Dockerfile installs
3. `devDependencies["@playwright/test"]` in `e2e/package.json`

The browser binaries baked into the image are usable only by `@playwright/test`
at that exact version. A mismatch fails with:

```
Executable doesn't exist at /ms-playwright/chromium-NNNN
```

`web/package.json` pins the same version for the same reason. They move together
or not at all.

**`e2e/node_modules` does not exist and must never be committed.** The image
symlinks `/node_modules` → `/usr/lib/node_modules`, which puts the global install
on Node's upward resolution path from `/workspace`. Nothing here runs
`npm install`; `package.json` exists to pin the version and to document it.

`package.json` deliberately has **no `"type": "module"`**. Playwright's
TypeScript loader is most reliable on the CommonJS path, where extensionless
relative imports (`../support/env`) resolve without an ESM loader hook.
`tsconfig.json` is editor configuration only — Playwright transpiles but does
not type-check, and nothing in the container runs `tsc`.

---

## What this suite asserts

Every number, name and price checked here travelled the whole pipeline:

```
synthetic provider → ingest → Kafka → pricer → Postgres → api/stream → browser
```

**There is no mock data, no fixture, no seeded user and no golden file in this
directory.** Nothing here knows the name of a team, a league or a book, and
nothing may learn one — a suite that hardcodes an entity is a suite that stops
testing the pipeline. The assertions are therefore about *shape*, *structure*
and *protocol*, which are the things that stay true of a live system whose
contents change between two reads.

| Spec | Asserts |
|---|---|
| `landing.spec.ts` | The landing page renders with one named `h1`, offers a route into the board, and **states that this is a simulation and not a licensed sportsbook** (CLAUDE.md §0, DESIGN.md "Non-negotiable content"). |
| `board.spec.ts` | The board resolves to **populated XOR explicitly empty**, and whichever branch it takes is internally consistent. Table semantics: column headers exist, price cells are cells, every price cell has an accessible name naming a market, a selection and a price. |
| `live.spec.ts` | The gateway connects, `hello` arrives first at `seq` 1 on protocol `sharpline.v1`, `seq` is monotonic, a subscribe is acked and answered with a snapshot, **a sequence gap is answered by a client resync**, the status surface names its state, and prices move — on the wire and on screen. |
| `auth.spec.ts` | **The critical path.** Register a brand-new account against the live API, browse the board signed in, survive a reload, sign out, and confirm the board is still browsable signed out. Plus: **no credential ever appears in a URL** — not in a navigation, not in a request, not in the WebSocket handshake. |
| `event-detail.spec.ts` | Following an event link from the board lands on a page naming that same event, with a market tree carrying at least one real price. |
| `a11y.spec.ts` | Exactly one `aria-live="polite"` region; nothing `assertive`; one `h1` per page; every `img` has an `alt`; the odds-format control is a radiogroup naming all three formats with exactly one selected; every price cell takes focus and has a name. |

### An empty board is a PASS

The synthetic provider may genuinely hold no events inside the requested window.
A board that says so, in words, has done its job. What is **not** a pass is a
third state — no rows *and* no empty state — which is exactly what
`support/board.ts` fails on.

Every spec that needs an event skips cleanly when the board is empty, with a
message saying so.

### Skips are deliberate, not laziness

The feed is a stochastic market maker. "A price changed in the next 45 seconds"
is an assertion about an RNG, and a flaky assertion about an RNG is worse than
an honest skip — it teaches everyone to stop reading a red suite. So `live.spec.ts`
splits by determinism:

- **Hard assertions** — the protocol. `hello` first, `seq` from 1, monotonic,
  subscribe → ack → snapshot, gap → resync, no credential in the URL. None of
  that depends on whether a price moved.
- **Skipping assertions** — movement. If no delta arrives in the window, or no
  delta lands on a market inside the sampled slice, the test skips with the
  observed wire summary in the message.

The protocol assertions read the WebSocket directly via `page.on('websocket')`,
so they need **no cooperation from the frontend** — no test id, no exposed
counter, no debug hook. `internal/wsgw/doc.go` is the contract.

---

## What this suite deliberately does NOT assert

- **Anything in phase 8.** No bet slip, no stake input, no payout, no
  cash-out, no placement, no settlement. CLAUDE.md §11 puts wagering in phase 8,
  and there is no placeholder for it here.
- **Pixel appearance.** No screenshot comparison. DESIGN.md is enforced by
  `/design-review`, not by a golden PNG that a font hint would break.
- **Colour.** Cyan/amber direction is checked structurally where it can be (the
  delta rail firing at all), never by sampling a pixel.
- **Specific data.** No team name, no league name, no book name, no price value.

---

## Structure

```
e2e/
├── playwright.config.ts      chromium only, --no-sandbox, no webServer
├── support/
│   ├── env.ts                base URL, routes, the timing budget, credentials
│   ├── selectors.ts          THE UI CONTRACT — every dependency on web/, in one file
│   ├── board.ts              populated XOR empty, and the wait that decides
│   ├── odds.ts               reading a rendered price without knowing its value
│   ├── stream.ts             WebSocket recorder: frames, seq, gaps, resyncs
│   ├── auth.ts               register / sign in / sign out against the live API
│   └── security.ts           "no credential in a URL", enforced
└── tests/
    ├── landing.spec.ts  board.spec.ts  live.spec.ts
    ├── auth.spec.ts     event-detail.spec.ts  a11y.spec.ts
```

`support/selectors.ts` is the file to open first. Every dependency this suite has
on the frontend lives there, so a rename in `web/` is a one-file fix.

### What the frontend must provide

Verified present unless marked:

| Contract | Where | Status |
|---|---|---|
| `/`, `/board`, `/register`, `/login` routes | app router | present (auth routes probed canonical-first, with a header-link fallback) |
| `"simulation"` and `"not a licensed sportsbook"` in the landing copy | `layout/disclaimer.tsx` | present |
| A link to `/board` from the landing page | `app/page.tsx` | present |
| Exactly one `<h1>` per page | — | present on `/` |
| `role="radiogroup"` named "Odds format", three `role="radio"` with `aria-checked` | `layout/odds-format-toggle.tsx` | present |
| One `aria-live="polite"` announcer **without** `role="status"` | `live/live-announcer.tsx` | present |
| `aria-label="Stream status"` on the rail; `.status-pip[data-state]` on the pip | `layout/status-rail.tsx`, `status-pip.tsx` | present |
| Sign-out as `menuitem`/button/link; account trigger named `/account\|profile\|signed in as/` | `auth/account-menu.tsx` | present |
| `Email` / `Password` labels; `Create account` / `Sign in` submit buttons | `auth/*-form.tsx` | present |
| **`.price-cell` on every price cell**, plus `.digit-roll-current` around the numeral | board components | **required** — the delta rail depends on both |
| **An accessible name on every price cell naming market, selection and price** | board components | **required** by DESIGN.md |
| **`data-testid="board-empty"`** on the empty state, or copy matching `/no (events\|markets\|games\|odds)/i` | board components | **required** — one or the other |
| `data-testid="price-cell"`, `data-testid="price-value"`, `data-testid="stream-status"`, `data-testid="legal-disclaimer"` | anywhere | optional — each is an alternative to the role/class above, never the only path |

The `:not([role="status"])` in the live-region assertion is deliberate:
`live-announcer.tsx` states that `role="status"` is an implicit live region in
its own right and the price announcer therefore omits it, while the search
box's result count carries it. Counting them together would conflate "the board
announces twice" with "search reports its result count", which are not the same
defect. The suite asserts exactly one of the former and at most one of the
latter.

Locator priority, in order:

1. **ARIA role + accessible name.** DESIGN.md and CLAUDE.md §7 already require
   these to exist, so depending on them is depending on a promise already made.
2. **`data-testid`**, where the design defines no role — the board's empty state
   and the price cell.
3. **A design-system class from `web/src/app/globals.css`** — `.price-cell`,
   `.digit-roll-current`, `.rail-decaying`, `.status-pip`. These are published
   contracts, not incidental CSS: a board that does not carry them has no delta
   rail. Each is always ORed with a test id, so either satisfies the locator.

No test depends on another test's state. Each run registers its own account
against a unique address, because the API has no test-user seeding and
registering the same address twice is a 409.

The only wall-clock waits in the suite are the bounded polls in `live.spec.ts`
that sample the stochastic feed over a window; they return the moment the first
delta lands. Everything else is a web-first assertion or an `expect.poll`.
