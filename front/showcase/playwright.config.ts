/**
 * Playwright configuration for the *showcase* runner — the website's marketing
 * screenshots and videos.
 *
 * Deliberately separate from ../playwright.config.ts:
 *
 * - It targets a demo-mode instance on its own ports, not the e2e test-mode
 *   one, so the two can never fight over a database.
 * - It runs single-worker and serial: captures share one scenario and a shared
 *   live connection, and parallelism would make the output non-deterministic.
 * - It never runs as part of `make test-e2e`, and it must never gate CI. On
 *   pull requests it runs only as a non-blocking rot guard with its output
 *   thrown away (see .github/workflows/showcase.yml).
 */
import { defineConfig, devices } from "@playwright/test";
import { join } from "node:path";
import { BASE_URL, VIEWPORT, WORK_DIR } from "./config";

export default defineConfig({
  testDir: ".",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: !!process.env.CI,
  timeout: 120_000,
  reporter: [["list"]],

  globalSetup: "./global-setup.ts",
  outputDir: join(WORK_DIR, "test-results"),

  use: {
    baseURL: BASE_URL,
    viewport: VIEWPORT,
    trace: "retain-on-failure",
    // Screenshot/video capture is the *product* here, so the failure-capture
    // machinery stays off: it would litter the output directory.
    screenshot: "off",
    video: "off",
  },

  projects: [
    {
      // Retina-density stills: the website serves these at 1x CSS pixels.
      name: "screenshots",
      testMatch: /screenshots\.spec\.ts/,
      use: {
        ...devices["Desktop Chrome"],
        viewport: VIEWPORT,
        deviceScaleFactor: 2,
      },
    },
    {
      // 1x for video: the recorder scales frames to the requested size anyway,
      // and 2x only inflates the bitrate for no visible gain after transcode.
      name: "video",
      testMatch: /approval-video\.spec\.ts/,
      use: {
        ...devices["Desktop Chrome"],
        viewport: VIEWPORT,
        deviceScaleFactor: 1,
        video: { mode: "on", size: VIEWPORT },
      },
    },
  ],
});
