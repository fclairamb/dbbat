import { defineConfig, devices } from "@playwright/test";

/**
 * Playwright configuration for testing against running dev server.
 * Use this when you already have `make dev` running.
 */
export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: undefined,
  reporter: [
    ["html"],
    ["list"],
    ["json", { outputFile: "test-results/results.json" }],
  ],

  // No global setup/teardown - server is already running
  // globalSetup: "./e2e/global-setup.ts",
  // globalTeardown: "./e2e/global-teardown.ts",

  use: {
    // Point to dev server
    baseURL: "http://localhost:8080/app/",
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
  },

  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
