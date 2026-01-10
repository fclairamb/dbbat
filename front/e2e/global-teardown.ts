/**
 * Global teardown for Playwright tests.
 *
 * This script:
 * 1. Stops the dbbat server process
 * 2. Stops PostgreSQL via docker-compose
 * 3. Cleans up temporary files
 */
import { spawn } from "node:child_process";
import { readFileSync, unlinkSync, existsSync } from "node:fs";
import { join } from "node:path";

const PROJECT_ROOT = join(import.meta.dirname, "../..");
const PID_FILE = join(import.meta.dirname, ".test-server.pid");

/**
 * Execute a command and return a promise that resolves when it completes.
 */
function execCommand(
  command: string,
  args: string[],
  options: { cwd?: string } = {}
): Promise<void> {
  return new Promise((resolve, reject) => {
    console.log(`[teardown] Running: ${command} ${args.join(" ")}`);
    const proc = spawn(command, args, {
      cwd: options.cwd || PROJECT_ROOT,
      stdio: "inherit",
    });

    proc.on("close", (code) => {
      if (code === 0) {
        resolve();
      } else {
        // Don't fail teardown on non-zero exit codes
        console.warn(`[teardown] Command exited with code ${code}`);
        resolve();
      }
    });

    proc.on("error", (err) => {
      console.warn(`[teardown] Command error:`, err);
      resolve(); // Don't fail teardown
    });
  });
}

/**
 * Kill a process by PID.
 */
async function killProcess(pid: number): Promise<void> {
  console.log(`[teardown] Killing process ${pid}...`);

  try {
    // Try SIGTERM first (graceful shutdown)
    process.kill(pid, "SIGTERM");

    // Wait a bit for graceful shutdown
    await new Promise((resolve) => setTimeout(resolve, 2000));

    // Check if still running
    try {
      process.kill(pid, 0); // Check if process exists
      // Still running, force kill
      console.log(`[teardown] Process ${pid} still running, forcing kill...`);
      process.kill(pid, "SIGKILL");
    } catch {
      // Process is dead
      console.log(`[teardown] Process ${pid} terminated successfully.`);
    }
  } catch (err: any) {
    if (err.code === "ESRCH") {
      // Process doesn't exist
      console.log(`[teardown] Process ${pid} not found (already stopped).`);
    } else {
      console.warn(`[teardown] Error killing process ${pid}:`, err);
    }
  }
}

/**
 * Global teardown function executed after all tests.
 */
export default async function globalTeardown(): Promise<void> {
  console.log("\n[teardown] Starting global teardown for E2E tests...\n");

  try {
    // Step 1: Stop dbbat server
    if (existsSync(PID_FILE)) {
      console.log("[teardown] Step 1: Stopping dbbat server...");
      const pid = parseInt(readFileSync(PID_FILE, "utf-8").trim(), 10);

      if (!isNaN(pid)) {
        await killProcess(pid);
      }

      // Clean up PID file
      unlinkSync(PID_FILE);
      console.log("[teardown] Server stopped successfully.\n");
    } else {
      console.log("[teardown] No PID file found, server may already be stopped.\n");
    }

    // Step 2: Stop PostgreSQL
    console.log("[teardown] Step 2: Stopping PostgreSQL...");
    await execCommand("docker", ["compose", "down"], { cwd: PROJECT_ROOT });
    console.log("[teardown] PostgreSQL stopped successfully.\n");

    console.log("[teardown] Global teardown completed successfully!\n");
  } catch (err) {
    console.error("[teardown] Global teardown failed:", err);
    // Don't throw - we want teardown to always succeed
  }
}
