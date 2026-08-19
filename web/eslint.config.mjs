// eslint-config-next 16 ships NATIVE flat config, so the @eslint/eslintrc
// FlatCompat shim this file used for v15 is both unnecessary and broken here:
// feeding an already-flat config back through the eslintrc compatibility layer
// produces "TypeError: Converting circular structure to JSON" when the
// validator tries to serialise the shared React plugin object.
//
// The subpath exports are imported directly instead.
import nextCoreWebVitals from "eslint-config-next/core-web-vitals";
import nextTypeScript from "eslint-config-next/typescript";

/** @type {import('eslint').Linter.Config[]} */
const eslintConfig = [
  {
    ignores: [".next/**", "node_modules/**", "out/**", "next-env.d.ts"],
  },
  ...nextCoreWebVitals,
  ...nextTypeScript,
];

export default eslintConfig;
