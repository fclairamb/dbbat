/**
 * Global setup for Playwright tests.
 *
 * This script:
 * 1. Builds the frontend (embedded into backend resources)
 * 2. Builds the backend binary with embedded frontend
 * 3. Starts PostgreSQL via docker-compose
 * 4. Starts the dbbat server with DBB_RUN_MODE=test
 * 5. Waits for the server to be ready
 * 6. Stores server process info for teardown
 *
 * In CI environments (when CI=true), the setup is skipped because:
 * - The binary is already built by the CI pipeline
 * - PostgreSQL is already started by the CI workflow
 * - The dbbat server is already running
 * Only server readiness check is performed.
 */
import { spawn, type ChildProcess } from "node:child_process";
import { writeFileSync } from "node:fs";
import { join } from "node:path";

const PROJECT_ROOT = join(import.meta.dirname, "../..");
const PID_FILE = join(import.meta.dirname, ".test-server.pid");
const SERVER_URL = "http://localhost:8080/app/";
const MAX_RETRIES = 30; // 30 seconds
const RETRY_DELAY = 1000; // 1 second

// Check if running in CI environment
const IS_CI = process.env.CI === "true";

/**
 * Execute a command and return a promise that resolves when it completes.
 */
function execCommand(
  command: string,
  args: string[],
  options: { cwd?: string; env?: NodeJS.ProcessEnv } = {},
): Promise<void> {
  return new Promise((resolve, reject) => {
    console.log(`[setup] Running: ${command} ${args.join(" ")}`);
    const proc = spawn(command, args, {
      cwd: options.cwd || PROJECT_ROOT,
      env: { ...process.env, ...options.env },
      stdio: "inherit",
    });

    proc.on("close", (code) => {
      if (code === 0) {
        resolve();
      } else {
        reject(new Error(`Command failed with code ${code}`));
      }
    });

    proc.on("error", (err) => {
      reject(err);
    });
  });
}

/**
 * Start a background process and return the ChildProcess object.
 */
function startBackgroundProcess(
  command: string,
  args: string[],
  options: { cwd?: string; env?: NodeJS.ProcessEnv } = {},
): ChildProcess {
  console.log(`[setup] Starting: ${command} ${args.join(" ")}`);
  const proc = spawn(command, args, {
    cwd: options.cwd || PROJECT_ROOT,
    env: { ...process.env, ...options.env },
    stdio: "inherit",
    detached: false,
  });

  return proc;
}

/**
 * Wait for the server to be ready by polling the health endpoint.
 */
async function waitForServer(url: string, maxRetries: number): Promise<void> {
  console.log(`[setup] Waiting for server at ${url}...`);

  for (let i = 0; i < maxRetries; i++) {
    try {
      const response = await fetch(url);
      if (response.ok || response.status === 404) {
        // 404 is OK - it means the server is up, just not found at this exact path
        console.log("[setup] Server is ready!");
        return;
      }
    } catch {
      // Server not ready yet
    }

    await new Promise((resolve) => setTimeout(resolve, RETRY_DELAY));
  }

  throw new Error(
    `Server did not become ready after ${maxRetries * RETRY_DELAY}ms`,
  );
}

/**
 * Global setup function executed before all tests.
 */
export default async function globalSetup(): Promise<void> {
  console.log("[setup] Starting global setup for E2E tests...\n");

  // In CI, the server is already started by the CI workflow
  // We only need to wait for it to be ready
  if (IS_CI) {
    console.log(
      "[setup] Running in CI environment - skipping build and server start.\n",
    );
    console.log("[setup] Waiting for server to be ready...");
    await waitForServer(SERVER_URL, MAX_RETRIES);
    console.log("[setup] Global setup completed successfully!\n");
    return;
  }

  try {
    // Step 1: Build frontend
    console.log("[setup] Step 1: Building frontend...");
    await execCommand("make", ["build-front"], { cwd: PROJECT_ROOT });
    console.log("[setup] Frontend built successfully.\n");

    // Step 2: Build backend
    console.log("[setup] Step 2: Building backend...");
    await execCommand("make", ["build"], { cwd: PROJECT_ROOT });
    console.log("[setup] Backend built successfully.\n");

    // Step 3: Start PostgreSQL with docker-compose
    console.log("[setup] Step 3: Starting PostgreSQL...");
    await execCommand("docker", ["compose", "up", "-d", "postgres"], {
      cwd: PROJECT_ROOT,
    });
    console.log("[setup] PostgreSQL started successfully.\n");

    // Wait for PostgreSQL to be ready
    console.log("[setup] Waiting for PostgreSQL to be ready...");
    await new Promise((resolve) => setTimeout(resolve, 5000)); // Wait 5 seconds
    console.log("[setup] PostgreSQL should be ready.\n");

    // Step 4: Start dbbat server with DBB_RUN_MODE=test
    console.log("[setup] Step 4: Starting dbbat server in test mode...");
    const serverProcess = startBackgroundProcess("./bin/dbbat", ["serve"], {
      cwd: PROJECT_ROOT,
      env: {
        ...process.env,
        DBB_RUN_MODE: "test",
        DBB_LISTEN_PG: ":5432",
        DBB_LISTEN_API: ":8080",
        DBB_DSN:
          "postgres://postgres:postgres@localhost:5001/dbbat?sslmode=disable",
        DBB_KEY: "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=", // base64 encoded 32-byte key
        DBB_RATE_LIMIT_ENABLED: "false", // Disable rate limiting for E2E tests
      },
    });

    // Store server PID for teardown
    writeFileSync(PID_FILE, serverProcess.pid!.toString());
    console.log(
      `[setup] Server process started with PID ${serverProcess.pid}\n`,
    );

    // Step 5: Wait for server to be ready
    await waitForServer(SERVER_URL, MAX_RETRIES);

    console.log("[setup] Global setup completed successfully!\n");
  } catch (err) {
    console.error("[setup] Global setup failed:", err);
    // Clean up on failure
    try {
      await execCommand("docker", ["compose", "down"], { cwd: PROJECT_ROOT });
    } catch {
      // Ignore cleanup errors
    }
    throw err;
  }
}
