import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist"] },
  {
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    files: ["**/*.{ts,tsx}"],
    languageOptions: {
      ecmaVersion: 2020,
      globals: globals.browser,
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true },
      ],
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_" },
      ],
    },
  },
  {
    // Playwright suites resolve `page.goto()` against the `baseURL` set in
    // playwright.config.ts, which is "http://localhost:8080/app/". A *relative*
    // path composes with it ("api-keys" -> /app/api-keys); an *absolute* one
    // replaces the whole path ("/api-keys" -> /api-keys), which the Go router
    // answers with a bare "404 page not found". The page then contains none of
    // the app, so every locator times out and the failure reads as a broken
    // feature rather than a stray leading slash.
    //
    // Written as an attribute path on the CallExpression so only the *first*
    // argument (the URL) is inspected. `\x2f` is a slash spelled so it does not
    // terminate the esquery regex literal. Allowed: "/" (the server redirects
    // the root to the app base) and anything already spelling out "/app/".
    files: ["e2e/**/*.{ts,tsx}", "showcase/**/*.{ts,tsx}"],
    rules: {
      "no-restricted-syntax": [
        "error",
        {
          selector:
            'CallExpression[callee.property.name="goto"][arguments.0.value=/^\\x2f(?!$)(?!app\\x2f)/]',
          message:
            'Absolute page.goto() path: Playwright resolves it against baseURL "http://localhost:8080/app/" by REPLACING the path, so "/foo" lands on http://localhost:8080/foo — the Go server\'s bare 404, not the app. Use a relative path ("foo") or spell the base out ("/app/foo").',
        },
      ],
    },
  }
);
