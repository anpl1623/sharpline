# Provenance of the golden files in this directory

CLAUDE.md §10 requires that "provider normalization uses golden files against recorded
payloads". **No payload in this directory was recorded from a live API call, and none was
written by hand.** Every byte was extracted from The Odds API's own published
documentation. This file records exactly where each came from, when, and what — if
anything — was changed.

No `ODDS_API_KEY` was available when these were captured, so recording a live payload was
impossible; and inventing a "realistic-looking" response would have baked a guess about the
provider's wire format into the regression tests that are supposed to *detect* wire-format
drift. The provider's own documentation is the next-best authority and it is a real
published artifact, so that is what is stored here.

**Retrieved: 2026-08-17.** Re-fetch and diff before trusting these against a live key.

**A key existing later does not retroactively make these live captures.** If a live payload
is ever recorded, it belongs in a *separate* directory with its own provenance note and the
key stripped from every URL — never merged into this one. These files stay what they are:
documentation samples, with the documentation's own age visible in them (the sports list
still describes the 2021/2022 Super Bowl).

---

## How they were extracted

The documentation page renders each example response inside a syntax-highlighted
`<pre><code>` block. The raw HTML was fetched, the Prism `<span>` tags stripped, and HTML
entities unescaped. That transformation is lossless with respect to the JSON text: it
removes markup that was never part of the payload and restores `&quot;` to `"`.

```
docker run --rm --user 502:20 -v "$PWD:/out" \
  alpine:latest@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b \
  sh -c 'wget -q -O /out/v4guide.html https://the-odds-api.com/liveapi/guides/v4/'
```

(The fetch ran in a container, per the prime directive, and as UID 502 so nothing
root-owned lands in a shared volume.)

---

## Files

### `get_sports.json` — 5,801 bytes

- **Source:** <https://the-odds-api.com/liveapi/guides/v4/#get-sports>, "Example Response".
- **Request the docs show:** `GET /v4/sports/?apiKey={apiKey}`
- **Changes:** none. Parses as JSON exactly as published.
- **What it proves:** the sport catalogue decode, including `has_outrights` and the
  `group`/`title`/`description` split. Costs zero credits, so this endpoint is polled
  freely (ADR 0003, "Refresh the event and league catalogue aggressively").

### `get_odds_americanfootball_nfl_h2h_spreads_american.json.elided` — 15,455 bytes

- **Source:** <https://the-odds-api.com/liveapi/guides/v4/#get-odds>, "Example Response".
- **Request the docs show:**
  `GET /v4/sports/americanfootball_nfl/odds/?apiKey=YOUR_API_KEY&regions=us&markets=h2h,spreads&oddsFormat=american`
- **Changes:** none. The file is byte-for-byte what the docs publish, **including the
  documentation's own truncation**, which is why the extension is `.json.elided` and not
  `.json` — the file is deliberately NOT valid JSON.

  The published sample shows one complete event and then elides the rest of the array. It
  ends with the literal five bytes `,\n...` where the closing `]` would be, and no
  trailing newline.

  The repair is performed **in the test, in code you can read** — see `stripDocsElision`
  in `harness_test.go` — rather than by an invisible hand-edit here. The test asserts the
  exact elided suffix is present before repairing it, so if a future re-fetch returns a
  complete array the test fails loudly instead of silently accepting a different file.
- **What it proves:** the odds decode across 12 bookmakers × 2 markets in **American**
  odds format. Note what this sample does *not* have: no `sport_title` on the event, and
  no `last_update` at the market level. It predates both fields, which makes it the
  regression case for the decoder's fallbacks.

### `get_event_odds_americanfootball_nfl_player_pass_tds.json` — 2,830 bytes

- **Source:** <https://the-odds-api.com/liveapi/guides/v4/#get-event-odds>, "Example Response".
- **Request the docs show:**
  `GET /v4/sports/americanfootball_nfl/events/a512a48a58c4329048174217b2cc7ce0/odds?apiKey=YOUR_API_KEY&regions=us&markets=player_pass_tds&oddsFormat=american`
- **Changes:** none. Parses as JSON exactly as published.
- **What it proves:** the complementary case to the file above — this one **has**
  `sport_title` and **has** market-level `last_update`, and has **no** bookmaker-level
  `last_update`. Between the two samples both branches of the `observed_at` resolution
  are exercised against real published bytes. It also carries the `description` field
  that player props use to name the player, and a single response object rather than an
  array.

### `openapi_v4.json` — 56,833 bytes

- **Source:** <https://api.swaggerhub.com/apis/the-odds-api/odds-api/4>, the machine-readable
  OpenAPI 3.0.1 document that the documentation's "Schema" links point at
  (`app.swaggerhub.com/apis-docs/the-odds-api/odds-api/4`).
- **Changes:** none.
- **What it is for:** it is not a payload and no test decodes it as one. It is the
  authoritative field-by-field schema, and `wire_test.go` reads it to assert that every
  property this package decodes is a property the provider actually documents — so a typo
  in a struct tag fails the build instead of silently producing a zero value. It is also
  where the documented status codes come from (401 / 404 / 422 / 429 / 500) and where the
  quota response headers are declared as integers.

---

## Documented facts these tests depend on

Recorded here so a reviewer does not have to re-derive them, with the page each came from.

| Fact | Source |
|---|---|
| Host is `https://api.the-odds-api.com` (IPv6: `ipv6-api.the-odds-api.com`) | guides/v4 §Host; OpenAPI `servers` |
| `/v4/sports` and `/v4/sports/{sport}/events` cost **0** credits | guides/v4 §Usage Quota Costs |
| `/v4/sports/{sport}/odds` costs `markets × regions` | guides/v4 §Usage Quota Costs |
| Every response carries `x-requests-remaining`, `x-requests-used`, `x-requests-last` (integers) | guides/v4 §Response Headers; OpenAPI response `headers` |
| Market-level `last_update` is the **recommended** recency field, in preference to the bookmaker-level one | OpenAPI, `markets[].last_update` description |
| A group of 10 `bookmakers` counts as one region-equivalent | OpenAPI, `bookmakers` parameter description |
| Rate limit is **30 requests/second**; exceeding it returns **429** | <https://the-odds-api.com/guide/rate-limit.html> |
| Quota exhaustion is `OUT_OF_USAGE_CREDITS` and arrives as **401**, not 429 | <https://the-odds-api.com/liveapi/guides/v4/api-error-codes.html> + OpenAPI 401 description |
| Bad key is `MISSING_KEY` / `INVALID_KEY` / `DEACTIVATED_KEY`, also **401** | same |
| Bad parameters are **422** | OpenAPI 422 description |
| Unknown/expired event id is **404** (event-odds endpoints only) | OpenAPI 404 description |
| "If no events are returned, the request will not count against the usage quota" | guides/v4 §More info |

## What the samples cannot cover, and what was done instead

Two published payloads cannot reach every branch of the mapping. States such as an event
missing one competitor, a market key this build does not serve, or a quote carrying no
timestamp at all are real — a provider will produce them eventually — but neither sample
contains one.

Those branches are covered by **table-driven unit tests over `normalizer.RawEvent` values
in `mapping_test.go`**, which are inputs to pure functions and never travel over HTTP.
That is deliberately a different thing from what this directory holds: nothing constructed
in a test is ever served as a provider response, and no file here was written by hand. If
a new wire-format case needs an HTTP-level test, the payload must come from the provider's
published documentation and be recorded above — not invented.

---

**Not documented anywhere, and therefore not assumed by this package:** the JSON envelope
of an error response. The docs name the error *codes* but never show an error *body*. So
`classifyErrorCode` matches the documented code tokens as substrings of the raw body rather
than decoding a guessed `{"message": ..., "error_code": ...}` shape. When no documented
token is found the error stays classified by status code alone. See `errors.go`.
