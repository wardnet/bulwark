// Canonical bulwark ESLint config. This file is embedded into the bulwark
// binary and passed explicitly via --config, independent of whatever (if
// anything) the target package declares in its own devDependencies — see
// internal/typescript/typescript.go.
import security from "eslint-plugin-security";

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
    plugins: { security },
    rules: {
      ...security.configs.recommended.rules,
    },
  },
];
