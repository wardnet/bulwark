// Canonical bulwark ESLint config. This file is embedded into the bulwark
// binary and passed explicitly via --config, independent of whatever (if
// anything) the target package declares in its own devDependencies — see
// internal/typescript/typescript.go.
import security from "eslint-plugin-security";

export default [
  {
    ignores: ["node_modules/**", "dist/**", "build/**", ".next/**", "coverage/**"],
  },
  {
    plugins: { security },
    rules: {
      ...security.configs.recommended.rules,
    },
  },
];
