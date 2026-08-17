/**
 * PHASE 0 PLACEHOLDER.
 *
 * Two deliberate constraints govern this file:
 *
 * 1. CLAUDE.md §7 requires `/design-consultation` to run before a single
 *    component is written. That is phase 7. Nothing here is a design decision
 *    to be built on — it is a legible holding page.
 *
 * 2. NO MOCK DATA. There are no events, markets, selections or prices on this
 *    page because none have travelled the real path
 *    (provider -> ingest -> Kafka -> normalizer -> pricer -> Postgres/Redis
 *    -> api/stream -> browser) yet. An empty surface is correct; a populated
 *    one fed by a literal would be a defect.
 *
 * The disclaimer below is not decorative. CLAUDE.md §0 requires the
 * "simulation, not a licensed sportsbook" distinction to be stated on the
 * landing page, and it must survive the phase 7 redesign.
 */
export default function Home() {
  return (
    <main className="mx-auto flex min-h-dvh max-w-2xl flex-col justify-center gap-8 px-6 py-16">
      <header className="flex flex-col gap-2">
        <h1 className="text-4xl font-semibold tracking-tight">Sharpline</h1>
        <p className="text-[var(--color-ink-muted)]">
          A real-time sports odds platform, running entirely on self-hosted
          compute.
        </p>
      </header>

      <section
        aria-labelledby="disclaimer-heading"
        className="flex flex-col gap-3 border-t border-[var(--color-rule)] pt-6"
      >
        <h2
          id="disclaimer-heading"
          className="text-xs font-semibold uppercase tracking-[0.14em] text-[var(--color-ink-muted)]"
        >
          Not a licensed sportsbook
        </h2>
        <p className="text-sm leading-relaxed text-[var(--color-ink-muted)]">
          Sharpline is a sportsbook{" "}
          <strong className="font-semibold text-[var(--color-ink)]">
            simulation
          </strong>{" "}
          built as an engineering project. No real money moves through it. All
          wagering is play-money against a double-entry ledger, and there is no
          KYC, no geolocation gating, no payment processing, and no custody of
          funds.
        </p>
      </section>
    </main>
  );
}
