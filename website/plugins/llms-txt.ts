/**
 * Generates llms.txt and llms-full.txt (https://llmstxt.org/) at build time
 * from website/docs/**\/*.{md,mdx}. Deliberately a small local plugin rather
 * than a third-party dependency (docusaurus-plugin-llms) — the logic below
 * is small and we'd rather own it than take on permanent Renovate churn for
 * a ~100-line build step.
 *
 * The two files are generated, not hand-maintained, specifically so a
 * renamed/removed/added doc page can never let them silently rot. That
 * guarantee lives in the routesPaths assertion in postBuild() below: if a
 * generated URL isn't a route Docusaurus actually built, the build fails.
 *
 * See specs/todos/2026-08-02-02-llms-txt.md for the full rationale.
 */
import fs from "node:fs/promises";
import path from "node:path";
import type { Plugin, Props } from "@docusaurus/types";

const SITE_URL = "https://dbbat.com";
const REPO_URL = "https://github.com/fclairamb/dbbat";
const DOCS_DIRNAME = "docs";
// docs/changelog.md is an MDX wrapper (`import Changelog from '../../CHANGELOG.md'`)
// with no prose of its own — it is special-cased in loadPage() below to pull
// its title/description from frontmatter and its body from the real file.
const CHANGELOG_DOC = "changelog.md";
const DEFAULT_POSITION = Number.POSITIVE_INFINITY;

type Section = "Docs" | "Configuration" | "Installation" | "API";

// Section headers follow the Proposal in the spec verbatim (Docs /
// Configuration / Installation / API). Anything not from one of the three
// named directories below (installation/configuration/api) — i.e. the root
// docs and features/* — is bucketed under "Docs".
const SECTION_BY_TOP_DIR: Record<string, Section> = {
  installation: "Installation",
  configuration: "Configuration",
  api: "API",
};

interface DocPage {
  url: string;
  title: string;
  annotation: string;
  body: string;
  section: Section;
}

interface Frontmatter {
  [key: string]: string;
}

function stripFrontmatter(raw: string): { frontmatter: Frontmatter; body: string } {
  const match = raw.match(/^---\n([\s\S]*?)\n---\n?/);
  if (!match) {
    return { frontmatter: {}, body: raw };
  }

  const frontmatter: Frontmatter = {};
  for (const line of match[1].split("\n")) {
    const kv = line.match(/^([A-Za-z0-9_]+):\s*(.*)$/);
    if (kv) {
      frontmatter[kv[1]] = kv[2].trim().replace(/^["']|["']$/g, "");
    }
  }

  return { frontmatter, body: raw.slice(match[0].length) };
}

/** Docusaurus-style URL: /docs/<path, ext stripped>, index segments collapsed. */
function toURL(relPosixPath: string): string {
  let noExt = relPosixPath.replace(/\.mdx?$/, "");
  if (noExt === "index") {
    noExt = "";
  } else if (noExt.endsWith("/index")) {
    noExt = noExt.slice(0, -"/index".length);
  }

  return noExt ? `/docs/${noExt}` : "/docs";
}

function firstHeading(body: string): string | undefined {
  const match = body.match(/^#\s+(.+)$/m);

  return match ? match[1].trim() : undefined;
}

function stripInlineMarkdown(text: string): string {
  return text
    .replace(/`([^`]+)`/g, "$1")
    .replace(/\*\*([^*]+)\*\*/g, "$1")
    .replace(/\*([^*]+)\*/g, "$1")
    .replace(/\[([^\]]+)\]\([^)]+\)/g, "$1")
    .trim();
}

/** First non-empty prose paragraph after the leading `# ` heading, markdown-stripped, one line. */
function firstParagraphAfterHeading(body: string): string {
  const lines = body.split("\n");
  let headingSeen = false;
  const paragraph: string[] = [];

  for (const line of lines) {
    if (!headingSeen) {
      if (/^#\s+/.test(line)) {
        headingSeen = true;
      }
      continue;
    }

    const trimmed = line.trim();
    const isBlank = trimmed === "";
    const isNonProse = /^(#|:::|import\s|<)/.test(trimmed);

    if (isBlank || isNonProse) {
      if (paragraph.length > 0) {
        break;
      }
      continue;
    }

    paragraph.push(trimmed);
  }

  return stripInlineMarkdown(paragraph.join(" "));
}

/**
 * Rewrites links so the concatenated llms-full.txt never contains a link
 * that only resolves in the context of the live site. Site-root-relative
 * links (`/docs/...`, the convention used throughout website/docs) get the
 * origin prepended; filesystem-relative links (`./foo.md`, `../foo.md`) are
 * resolved against the doc's own directory. Absolute URLs and in-page
 * anchors are left untouched.
 */
function rewriteRelativeLinks(body: string, docDir: string): string {
  return body.replace(/\]\(([^)]+)\)/g, (full, target: string) => {
    if (/^[a-z][a-z0-9+.-]*:/i.test(target) || target.startsWith("#")) {
      return full;
    }

    if (target.startsWith("/")) {
      return `](${SITE_URL}${target})`;
    }

    const [targetPath, hash] = target.split("#");
    const resolved = path.posix.normalize(path.posix.join(docDir, targetPath));
    const url = toURL(resolved);

    return `](${SITE_URL}${url}${hash ? `#${hash}` : ""})`;
  });
}

async function readCategoryPosition(dirPath: string): Promise<number> {
  try {
    const raw = await fs.readFile(path.join(dirPath, "_category_.json"), "utf-8");
    const json = JSON.parse(raw) as { position?: number };

    return typeof json.position === "number" ? json.position : DEFAULT_POSITION;
  } catch {
    return DEFAULT_POSITION;
  }
}

function frontmatterPosition(frontmatter: Frontmatter): number {
  if (!frontmatter.sidebar_position) {
    return DEFAULT_POSITION;
  }
  const parsed = Number(frontmatter.sidebar_position);

  return Number.isFinite(parsed) ? parsed : DEFAULT_POSITION;
}

/**
 * Walks docs/ and returns doc-relative POSIX paths in the same order the
 * autogenerated sidebar would show them: top level sorted by
 * _category_.json `position` (dirs) / frontmatter `sidebar_position` (root
 * files), ties broken alphabetically; each category directory's own files
 * sorted the same way.
 */
async function collectOrderedRelPaths(docsDir: string): Promise<string[]> {
  const entries = await fs.readdir(docsDir, { withFileTypes: true });

  const topEntries: { name: string; position: number; isDir: boolean }[] = [];
  for (const entry of entries) {
    if (entry.name.startsWith(".")) {
      continue;
    }
    if (entry.isDirectory()) {
      const position = await readCategoryPosition(path.join(docsDir, entry.name));
      topEntries.push({ name: entry.name, position, isDir: true });
    } else if (/\.mdx?$/.test(entry.name)) {
      const raw = await fs.readFile(path.join(docsDir, entry.name), "utf-8");
      const { frontmatter } = stripFrontmatter(raw);
      topEntries.push({ name: entry.name, position: frontmatterPosition(frontmatter), isDir: false });
    }
  }

  topEntries.sort((a, b) => a.position - b.position || a.name.localeCompare(b.name));

  const relPaths: string[] = [];
  for (const top of topEntries) {
    if (!top.isDir) {
      relPaths.push(top.name);
      continue;
    }

    const dirPath = path.join(docsDir, top.name);
    const children = await fs.readdir(dirPath, { withFileTypes: true });
    const childEntries: { name: string; position: number }[] = [];
    for (const child of children) {
      if (!child.isFile() || !/\.mdx?$/.test(child.name)) {
        continue;
      }
      const raw = await fs.readFile(path.join(dirPath, child.name), "utf-8");
      const { frontmatter } = stripFrontmatter(raw);
      childEntries.push({ name: child.name, position: frontmatterPosition(frontmatter) });
    }
    childEntries.sort((a, b) => a.position - b.position || a.name.localeCompare(b.name));

    for (const child of childEntries) {
      relPaths.push(path.posix.join(top.name, child.name));
    }
  }

  return relPaths;
}

async function loadPage(docsDir: string, siteDir: string, relPath: string): Promise<DocPage> {
  const absPath = path.join(docsDir, relPath);
  const docDir = path.posix.dirname(relPath) === "." ? "" : path.posix.dirname(relPath);
  const url = toURL(relPath);
  const topDir = docDir.split("/")[0] ?? "";
  const section = SECTION_BY_TOP_DIR[topDir] ?? "Docs";

  if (relPath === CHANGELOG_DOC) {
    // The doc file itself is just an MDX import wrapper — pull metadata from
    // its frontmatter and the actual prose from the real root CHANGELOG.md
    // rather than emitting the bare `import Changelog from ...` statement.
    const raw = await fs.readFile(absPath, "utf-8");
    const { frontmatter } = stripFrontmatter(raw);
    const changelogPath = path.join(siteDir, "..", "CHANGELOG.md");
    const changelogBody = await fs.readFile(changelogPath, "utf-8");

    return {
      url,
      title: frontmatter.title ?? "Changelog",
      annotation: frontmatter.description ?? "Release history and version changes",
      body: rewriteRelativeLinks(changelogBody, docDir),
      section,
    };
  }

  const raw = await fs.readFile(absPath, "utf-8");
  const { frontmatter, body } = stripFrontmatter(raw);
  const title = firstHeading(body) ?? frontmatter.title ?? relPath;
  const annotation = firstParagraphAfterHeading(body) || frontmatter.description || "";

  return {
    url,
    title,
    annotation,
    body: rewriteRelativeLinks(body, docDir),
    section,
  };
}

function renderLlmsTxt(tagline: string, pages: DocPage[]): string {
  const lines: string[] = [];
  lines.push("# DBBat", "", `> ${tagline}`, "");
  lines.push(
    "DBBat is a transparent database proxy for query observability, access control, and safety across PostgreSQL, Oracle, MySQL/MariaDB, and MongoDB. This file indexes the documentation; see llms-full.txt for the whole thing concatenated into one document.",
    "",
  );

  const sections: Section[] = ["Docs", "Configuration", "Installation", "API"];
  for (const section of sections) {
    const sectionPages = pages.filter((page) => page.section === section);
    if (sectionPages.length === 0) {
      continue;
    }

    lines.push(`## ${section}`);
    for (const page of sectionPages) {
      const annotation = page.annotation ? `: ${page.annotation}` : "";
      lines.push(`- [${page.title}](${SITE_URL}${page.url})${annotation}`);
    }
    lines.push("");
  }

  lines.push("## Optional");
  lines.push(`- [Full documentation text](${SITE_URL}/llms-full.txt): every doc page concatenated into one file`);
  lines.push(`- [GitHub repository](${REPO_URL}): source code, issues, releases`);
  lines.push(`- [API Reference](${SITE_URL}/docs/api): REST API overview and where to find the OpenAPI spec`);
  lines.push("");

  return lines.join("\n");
}

function renderLlmsFullTxt(tagline: string, pages: DocPage[]): string {
  const parts: string[] = ["# DBBat — Full Documentation", "", `> ${tagline}`, ""];
  parts.push(
    `The OpenAPI 3.0 specification is not inlined here (it's large — roughly 4000 lines). Read it at [internal/api/openapi.yml](${REPO_URL}/blob/main/internal/api/openapi.yml) on GitHub, or fetch it live from a running instance at \`GET /api/openapi.yml\`.`,
    "",
    "---",
    "",
  );

  for (const page of pages) {
    parts.push(`# ${page.title}`, `Source: ${SITE_URL}${page.url}`, "", page.body.trim(), "", "---", "");
  }

  return parts.join("\n");
}

export default function llmsTxtPlugin(): Plugin {
  return {
    name: "llms-txt",
    async postBuild({ siteConfig, siteDir, routesPaths, outDir }: Props) {
      const docsDir = path.join(siteDir, DOCS_DIRNAME);
      const relPaths = await collectOrderedRelPaths(docsDir);
      const pages = await Promise.all(relPaths.map((relPath) => loadPage(docsDir, siteDir, relPath)));

      // Rot guard: every generated URL must be a route Docusaurus actually
      // built this run. A renamed/removed/moved doc becomes a hard build
      // failure instead of a silently dead link in llms.txt — this is what
      // makes "auto-generated" actually mean something.
      const missing = pages.filter((page) => !routesPaths.includes(page.url));
      if (missing.length > 0) {
        throw new Error(
          `[llms-txt] Generated URL(s) not found in routesPaths — llms.txt would ship dead link(s): ${missing
            .map((page) => page.url)
            .join(", ")}`,
        );
      }

      const tagline = siteConfig.tagline ?? "";
      const llmsTxt = renderLlmsTxt(tagline, pages);
      const llmsFullTxt = renderLlmsFullTxt(tagline, pages);

      await fs.writeFile(path.join(outDir, "llms.txt"), llmsTxt, "utf-8");
      await fs.writeFile(path.join(outDir, "llms-full.txt"), llmsFullTxt, "utf-8");
    },
  };
}
