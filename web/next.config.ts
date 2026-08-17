import type { NextConfig } from "next";

/**
 * CLAUDE.md §7:
 *   - `output: "standalone"` so the prod runtime stage copies only
 *     `.next/standalone`, `.next/static` and `public`.
 *   - The browser talks to the API through the reverse proxy, NEVER to a
 *     container hostname. Both public URLs are therefore *proxy paths*, not
 *     origins: Caddy maps `/api/*` -> api:8080 and `/ws` -> stream:8081.
 *     Server-side fetches (which run inside the network) may use in-network
 *     service names; those are read from non-public env vars at request time
 *     and must never be inlined here.
 *
 * NEXT_PUBLIC_* values are inlined at BUILD time, so the defaults below are
 * what ships in the image unless overridden with --build-arg.
 */
const nextConfig: NextConfig = {
  output: "standalone",
  reactStrictMode: true,
  poweredByHeader: false,
  env: {
    NEXT_PUBLIC_API_URL: process.env["NEXT_PUBLIC_API_URL"] ?? "/api",
    NEXT_PUBLIC_WS_URL: process.env["NEXT_PUBLIC_WS_URL"] ?? "/ws",
  },

  /**
   * Image budget — see `outputFileTracingExcludes` below.
   *
   * `sharp` (+ its bundled libvips) is 18MB of the standalone output and is
   * loaded only when Next's built-in optimizer resizes an image at request
   * time. It is excluded to hold §7's "under 200MB" target, so the optimizer
   * is switched off explicitly rather than left to fail at request time.
   *
   * PHASE 7 DECISION POINT: if the odds board needs optimized remote images
   * (team/league logos), either drop the `sharp`/`@img` excludes below and
   * accept ~205MB, or keep this off and serve pre-sized static assets.
   */
  images: {
    unoptimized: true,
  },

  /**
   * The Next file tracer is conservative: it walks `next.config.ts` and pulls
   * in the whole TypeScript compiler, and it keeps optional build-time
   * transformers bundled inside `next/dist/compiled`. None of it can execute
   * in the standalone server, and together it is ~34MB — the difference
   * between missing and meeting §7's 200MB target on the mandated
   * node:24-alpine base (162MB of which is the base itself).
   *
   * Everything excluded here is build-time or dev-time only, and each entry
   * was verified by running the built image and serving a request. Do NOT add
   * a package here without proving the runtime does not load it — two
   * plausible-looking excludes (`next-devtools`, `compiled/babel`) turned out
   * to be required on every boot. See the notes below.
   */
  outputFileTracingExcludes: {
    "*": [
      // Pulled in solely because next.config is TypeScript. The resolved
      // config is serialized into .next/required-server-files.json at build
      // time; the runtime never parses the .ts file.
      "node_modules/typescript/**",
      // browserslist data, consumed by the CSS build pipeline only.
      "node_modules/caniuse-lite/**",
      // Native image codecs — see `images.unoptimized` above.
      "node_modules/sharp/**",
      "node_modules/@img/**",
      // AMP validation. This product does not emit AMP pages.
      "node_modules/next/dist/compiled/amphtml-validator/**",
      // The Babel *transform* pipeline is only loaded when a .babelrc is
      // present; this app uses SWC. NOTE: `compiled/babel` itself must stay —
      // next-devtools/server/shared.js requires `compiled/babel/code-frame`
      // on every boot.
      "node_modules/next/dist/compiled/babel-packages/**",
      // CSS transforms run at build time; the server ships compiled CSS.
      "node_modules/next/dist/compiled/postcss-preset-env/**",
      "node_modules/next/dist/compiled/cssnano-simple/**",
      // Fast refresh is dev-only.
      "node_modules/next/dist/compiled/react-refresh/**",
      // NOTE: `next/dist/next-devtools` and `next/dist/compiled/next-devtools`
      // look dev-only but are NOT. `server/patch-error-inspect.js` requires
      // `../next-devtools/server/shared` on every boot, so excluding them
      // makes the standalone server crash at startup with
      // "Cannot find module '../next-devtools/server/shared'". Leave them in.
    ],
  },
};

export default nextConfig;
