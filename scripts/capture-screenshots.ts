#!/usr/bin/env bun
/**
 * Screenshot capture script for documentation website.
 * Captures screenshots from the local server running in demo mode.
 *
 * Usage:
 *   1. Start server: make demo (or DBB_RUN_MODE=demo ./dbbat serve)
 *   2. Run: bun run scripts/capture-screenshots.ts
 *
 * Or set DEMO_URL to capture from the live demo:
 *   DEMO_URL=https://demo.dbbat.com/app bun run scripts/capture-screenshots.ts
 */

import { chromium } from "playwright";
import * as path from "path";
import * as fs from "fs";

const DEMO_URL = process.env.DEMO_URL || "http://localhost:8080/app";
const SCREENSHOTS_DIR = path.join(__dirname, "../website/static/img/screenshots");

// Helper to wait for animations to complete
async function waitForAnimations(page: import("playwright").Page) {
  await page.waitForLoadState("networkidle");
  // Wait for CSS animations/transitions to complete
  await page.waitForTimeout(500);
}

async function captureScreenshots() {
  // Ensure screenshots directory exists
  if (!fs.existsSync(SCREENSHOTS_DIR)) {
    fs.mkdirSync(SCREENSHOTS_DIR, { recursive: true });
  }

  console.log(`Capturing screenshots from: ${DEMO_URL}`);
  console.log("Launching browser...");
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({
    viewport: { width: 1280, height: 800 },
  });
  const page = await context.newPage();

  try {
    // Capture login page
    console.log("Capturing login page...");
    await page.goto(`${DEMO_URL}/login`);
    await waitForAnimations(page);
    await page.screenshot({
      path: path.join(SCREENSHOTS_DIR, "screenshot-login.png"),
    });
    console.log("  ✓ screenshot-login.png");

    // Login (demo mode uses admin/admin)
    console.log("Logging in...");
    await page.getByTestId("login-username").fill("admin");
    await page.getByTestId("login-password").fill("admin");
    await page.getByTestId("login-submit").click();

    // Wait for redirect to authenticated area
    await page.waitForURL((url) => !url.pathname.includes("/login"), {
      timeout: 15000,
    });
    await waitForAnimations(page);
    console.log("  ✓ Logged in successfully");

    // Capture Dashboard
    console.log("Capturing dashboard...");
    await page.screenshot({
      path: path.join(SCREENSHOTS_DIR, "screenshot-dashboard.png"),
    });
    console.log("  ✓ screenshot-dashboard.png");

    // Navigate to Users and capture
    console.log("Capturing users page...");
    await page.getByRole("link", { name: /users/i }).click();
    await page.waitForURL(/\/users/);
    await waitForAnimations(page);
    await page.screenshot({
      path: path.join(SCREENSHOTS_DIR, "screenshot-users.png"),
    });
    console.log("  ✓ screenshot-users.png");

    // Navigate to Databases and capture
    console.log("Capturing databases page...");
    await page.getByRole("link", { name: /databases/i }).click();
    await page.waitForURL(/\/databases/);
    await waitForAnimations(page);
    await page.screenshot({
      path: path.join(SCREENSHOTS_DIR, "screenshot-databases.png"),
    });
    console.log("  ✓ screenshot-databases.png");

    // Navigate to Grants and capture
    console.log("Capturing grants page...");
    await page.getByRole("link", { name: /grants/i }).click();
    await page.waitForURL(/\/grants/);
    await waitForAnimations(page);
    await page.screenshot({
      path: path.join(SCREENSHOTS_DIR, "screenshot-grants.png"),
    });
    console.log("  ✓ screenshot-grants.png");

    // Navigate to Connections and capture
    console.log("Capturing connections page...");
    await page.getByRole("link", { name: /connections/i }).click();
    await page.waitForURL(/\/connections/);
    await waitForAnimations(page);
    await page.screenshot({
      path: path.join(SCREENSHOTS_DIR, "screenshot-connections.png"),
    });
    console.log("  ✓ screenshot-connections.png");

    // Navigate to Queries and capture
    console.log("Capturing queries page...");
    await page.getByRole("link", { name: /queries/i }).click();
    await page.waitForURL(/\/queries/);
    await waitForAnimations(page);
    await page.screenshot({
      path: path.join(SCREENSHOTS_DIR, "screenshot-queries.png"),
    });
    console.log("  ✓ screenshot-queries.png");

    // Navigate to Audit and capture
    console.log("Capturing audit page...");
    await page.getByRole("link", { name: /audit/i }).click();
    await page.waitForURL(/\/audit/);
    await waitForAnimations(page);
    await page.screenshot({
      path: path.join(SCREENSHOTS_DIR, "screenshot-audit.png"),
    });
    console.log("  ✓ screenshot-audit.png");

    console.log("\nAll screenshots captured successfully!");
    console.log(`Screenshots saved to: ${SCREENSHOTS_DIR}`);
  } catch (error) {
    console.error("Error capturing screenshots:", error);
    // Take debug screenshot on error
    await page.screenshot({
      path: path.join(SCREENSHOTS_DIR, "debug-error.png"),
    });
    throw error;
  } finally {
    await browser.close();
  }
}

captureScreenshots().catch((error) => {
  console.error("Failed to capture screenshots:", error);
  process.exit(1);
});
