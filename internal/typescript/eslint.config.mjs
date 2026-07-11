// Canonical bulwark ESLint config. This file is embedded into the bulwark
// binary and passed explicitly via --config, independent of whatever (if
// anything) the target package declares in its own devDependencies — see
// internal/typescript/typescript.go.
import security from "eslint-plugin-security";
import tsParser from "@typescript-eslint/parser";

export default [
  {
    // Every pattern is "**/"-prefixed so it matches at any nesting depth, not
    // just directly under the scanned package root — ESLint's flat-config
    // ignores use minimatch semantics (no implicit gitignore-style recursive
    // matching for a bare "dist/**"), so a build-output dir nested inside a
    // package (e.g. admin-site/web/dist) would otherwise slip through.
    ignores: [
      "**/node_modules/**",
      "**/dist/**",
      "**/build/**",
      "**/target/**",
      "**/vendor/**",
      "**/.git/**",
      "**/.bare/**",
      "**/.next/**",
      "**/coverage/**",
    ],
  },
  {
    // Without an explicit `files:`, ESLint's flat-config default applies and
    // only **/*.js, **/*.mjs and **/*.cjs are linted — so in a TypeScript
    // project bulwark would scan nothing and still report [PASS]. Naming the
    // TS extensions here is what actually puts .ts/.tsx in front of the
    // security rules.
    files: ["**/*.{js,mjs,cjs,jsx,ts,tsx,mts,cts}"],
    languageOptions: {
      // TypeScript is not valid JavaScript, so espree (ESLint's default
      // parser) throws on the first type annotation it meets. The
      // typescript-eslint parser reads both, and is used for .js too so every
      // file goes through one parser rather than two.
      //
      // Deliberately parser-only: no `parserOptions.project`. Every rule in
      // eslint-plugin-security is syntactic, so none of them need type
      // information — and requiring it would mean resolving each scanned
      // package's tsconfig, which bulwark has no business guessing at.
      parser: tsParser,
      ecmaVersion: "latest",
      sourceType: "module",
    },
    plugins: { security },
    rules: {
      ...security.configs.recommended.rules,
    },
  },
];
