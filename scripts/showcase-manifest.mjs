#!/usr/bin/env node
/**
 * Stamp a showcase run with the version it captured.
 *
 * The showcase is regenerated on demand, never automatically on release, so
 * the only thing standing between the website and silently stale visuals is
 * this manifest. The website can read it to caption the media ("captured on
 * v0.22.0"), and a human can see at a glance how far behind the assets are.
 *
 * Version source is `.release-please-manifest.json` key `"."` — the same
 * source of truth the Makefile's VERSION/ldflags pipeline feeds
 * internal/version from.
 */
import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const REPO_ROOT = join(dirname(fileURLToPath(import.meta.url)), "..");

const outDir = process.argv[2];
if (!outDir) {
  console.error("usage: showcase-manifest.mjs <output-directory>");
  process.exit(2);
}

function releaseVersion() {
  const raw = readFileSync(join(REPO_ROOT, ".release-please-manifest.json"), "utf8");
  const manifest = JSON.parse(raw);
  const version = manifest["."];
  if (typeof version !== "string" || version === "") {
    throw new Error('.release-please-manifest.json has no "." entry');
  }
  return version;
}

function gitCommit() {
  try {
    return execFileSync("git", ["rev-parse", "HEAD"], {
      cwd: REPO_ROOT,
      encoding: "utf8",
    }).trim();
  } catch {
    return "unknown";
  }
}

const manifest = {
  version: releaseVersion(),
  commit: gitCommit(),
  generatedAt: new Date().toISOString(),
};

mkdirSync(outDir, { recursive: true });
writeFileSync(
  join(outDir, "manifest.json"),
  `${JSON.stringify(manifest, null, 2)}\n`,
);

console.log(
  `[showcase] manifest: v${manifest.version} @ ${manifest.commit.slice(0, 7)} (${manifest.generatedAt})`,
);
