import js from "@eslint/js";
import tseslint from "typescript-eslint";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";

export default tseslint.config(
  { ignores: ["**/dist/**", "**/storybook-static/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["packages/devtools-client/src/**/*.{ts,tsx}"],
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": ["warn", { allowConstantExport: true }],
    },
  },
  {
    files: [
      "packages/devtools-client/src/components/ui/**",
      "packages/devtools-client/src/components/TimelineView.tsx",
    ],
    rules: {
      "react-refresh/only-export-components": "off",
    },
  },
);
