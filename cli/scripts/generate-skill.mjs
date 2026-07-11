#!/usr/bin/env node
// Generates the Hiver Agent Skill into the CLI dist so it ships with the package.
// Everything is built locally from the canonical docs source (../docs, authored
// in this repo) — no network fetch. Output is self-contained:
//
//   packages/commands/dist/skills/hiver/
//     SKILL.md        (page index; links are relative ./docs/*.md paths)
//     docs/*.md       (one markdown file per page, cross-links also relative)
//
// Runs as part of `npm run build` (and therefore `prepack`, so it's published).

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const DOCS_DIR = path.resolve(__dirname, "../../docs"); // canonical page.mdx source
const OUT_DIR = path.resolve(__dirname, "../packages/commands/dist/skills/hiver");
const DOCS_HOST = "hiver.sh"; // for rewriting any absolute links found in a page

// Section grouping + page order for the skill index. Mirrors the docs sidebar
// (hive-site: src/lib/docs-nav.ts) — keep in sync when adding/moving doc pages.
// Entries whose page.mdx doesn't exist are skipped automatically.
const docsNav = [
  {
    title: "Overview",
    links: [
      { href: "/docs", label: "Introduction" },
      { href: "/docs/installation", label: "Installation" },
      { href: "/docs/getting-started", label: "Getting Started" },
      { href: "/docs/cli", label: "CLI" },
    ],
  },
  {
    title: "Examples",
    links: [{ href: "/docs/examples/open-work", label: "Open Work" }],
  },
  {
    title: "Guides",
    links: [
      { href: "/docs/examples/agent-cli", label: "Agent CLI" },
      { href: "/docs/examples/agent-sdk-anthropic", label: "Claude Agent SDK" },
      { href: "/docs/examples/agent-sdk-openai", label: "OpenAI Agents SDK" },
      { href: "/docs/examples/browser-use", label: "Browser use" },
      { href: "/docs/examples/python-sandbox", label: "Python" },
      { href: "/docs/examples/node-sandbox", label: "Node.js" },
      { href: "/docs/examples/bash-sandbox", label: "Bash" },
      { href: "/docs/examples/custom-docker-image", label: "Custom Docker image" },
      { href: "/docs/mocking", label: "Mocking" },
    ],
  },
  {
    title: "Execution",
    links: [
      { href: "/docs/execution", label: "Exec & Streaming" },
      { href: "/docs/pseudo-terminals", label: "Pseudo Terminals" },
      { href: "/docs/ingress", label: "Ingress" },
    ],
  },
  {
    title: "File System",
    links: [
      { href: "/docs/file-policy", label: "File Access" },
      { href: "/docs/remote-file-systems", label: "Local Files" },
      { href: "/docs/gcs", label: "GCS" },
      { href: "/docs/s3", label: "S3" },
      { href: "/docs/azure", label: "Azure Blob" },
      { href: "/docs/google-drive", label: "Google Drive" },
      { href: "/docs/snapshots", label: "Snapshots" },
    ],
  },
  {
    title: "Files",
    links: [
      { href: "/docs/read-file", label: "Read file" },
      { href: "/docs/write-file", label: "Write file" },
      { href: "/docs/list-directory", label: "List directory" },
      { href: "/docs/delete-file", label: "Delete file" },
    ],
  },
  {
    title: "Network",
    links: [
      { href: "/docs/network-policy", label: "Access control" },
      { href: "/docs/network/overrides", label: "Overrides" },
    ],
  },
  {
    title: "Logs",
    links: [{ href: "/docs/events", label: "Events" }],
  },
  {
    title: "Isolation",
    links: [
      { href: "/docs/runtime/container", label: "Container" },
      { href: "/docs/runtime/microvm", label: "MicroVM" },
    ],
  },
  {
    title: "Utilities",
    links: [
      { href: "/docs/utils/allow-sandbox", label: "Allow sandbox" },
      { href: "/docs/utils/python-packages", label: "Python packages" },
      { href: "/docs/utils/npm-packages", label: "Node.js packages" },
    ],
  },
  {
    title: "Deploy",
    links: [
      { href: "/docs/deploy/kubernetes", label: "Kubernetes" },
      { href: "/docs/deploy/gke", label: "GKE" },
    ],
  },
  {
    title: "Resources",
    links: [
      { href: "/docs/comparison", label: "Hiver vs. alternatives" },
      { href: "/docs/release-notes", label: "Release Notes" },
    ],
  },
];

// --- mdx -> markdown (ported from hive-site: src/lib/docs-md.ts) ----------

// Reads the raw page.mdx for a doc slug (e.g. ["deploy","gke"], or [] for /docs).
async function readDocSource(segments) {
  if (segments.some((s) => !s || s.includes("..") || s.includes("/"))) return null;
  const filePath = path.join(DOCS_DIR, ...segments, "page.mdx");
  if (filePath !== DOCS_DIR && !filePath.startsWith(DOCS_DIR + path.sep)) return null;
  try {
    return await fs.readFile(filePath, "utf8");
  } catch {
    return null;
  }
}

// Applies a per-line filter to everything outside fenced ``` blocks.
function filterProseLines(src, shouldDrop) {
  return src
    .split(/(```[\s\S]*?```)/g)
    .map((seg, i) =>
      i % 2 === 1
        ? seg
        : seg
            .split("\n")
            .filter((line) => !shouldDrop(line))
            .join("\n")
    )
    .join("");
}

// Converts page.mdx into plain-ish markdown: drops import/metadata boilerplate,
// flattens <Tabs>/<Tab> into headings, <Callout> into a blockquote, and strips
// purely-visual self-closing components.
function mdxToMarkdown(src) {
  let out = filterProseLines(
    src,
    (line) =>
      /^\s*import\s.+from\s+["'].+["'];?\s*$/.test(line) ||
      /^\s*export const metadata\s*=.*$/.test(line)
  );

  out = out.replace(/<Tab\s+label="([^"]*)"\s*>/g, "\n### $1\n");
  out = out.replace(/<\/Tab>\s*/g, "");
  out = out.replace(/<\/?Tabs>\s*/g, "");

  out = out.replace(/<Callout[^>]*>([\s\S]*?)<\/Callout>/g, (_m, body) =>
    body
      .trim()
      .split("\n")
      .map((l) => `> ${l}`.trimEnd())
      .join("\n")
  );

  out = out.replace(/<(?!CodeBlock\b)[A-Z]\w*(?:\s[^<>]*)?\/>\s*/g, "");

  return out.replace(/\n{3,}/g, "\n\n").trim() + "\n";
}

// --- link rewriting -------------------------------------------------------

// A doc page path (no leading slash / anchor) -> its file inside the bundle.
function pathToLocal(p) {
  if (p === "docs" || p === "docs.md") return "docs/index.md";
  return p.endsWith(".md") ? p : `${p}.md`;
}

// Given a link URL, return the relative in-bundle path it should point to (from
// `fromLocal`), or null to leave it untouched. Only same-host / root-relative
// /docs links whose target we actually bundled get rewritten.
function docLinkToRelative(url, fromLocal, known) {
  const hashIdx = url.indexOf("#");
  const anchor = hashIdx >= 0 ? url.slice(hashIdx) : "";
  const noAnchor = hashIdx >= 0 ? url.slice(0, hashIdx) : url;

  let pathname;
  if (/^https?:\/\//i.test(noAnchor)) {
    let u;
    try {
      u = new URL(noAnchor);
    } catch {
      return null;
    }
    if (u.host !== DOCS_HOST) return null;
    pathname = u.pathname;
  } else if (noAnchor.startsWith("/")) {
    pathname = noAnchor;
  } else {
    return null; // already relative
  }

  const p = pathname.replace(/^\/+/, "");
  if (p !== "docs" && p !== "docs.md" && !p.startsWith("docs/")) return null;

  const local = pathToLocal(p);
  if (!known.has(local)) return null;

  return path.posix.relative(path.posix.dirname(fromLocal), local) + anchor;
}

// Rewrite every markdown link `](url)` that resolves to a bundled doc page so it
// points at the local file relative to `fromLocal`.
function rewriteDocLinks(content, fromLocal, known) {
  return content.replace(/\]\(([^)\s]+)((?:\s+"[^"]*")?)\)/g, (match, url, title) => {
    const rel = docLinkToRelative(url, fromLocal, known);
    return rel ? `](${rel}${title})` : match;
  });
}

// --- SKILL.md assembly ----------------------------------------------------

function buildSkillMd(sections) {
  const sectionsMd = sections
    .map(({ title, links }) => `### ${title}\n\n${links.join("\n")}`)
    .join("\n\n");

  return `---
name: hiver
description: Use when writing code against Hiver (the runtime for AI agents) — installing the CLI/SDK, calling exec/file/network APIs, deploying on Kubernetes/GKE, or debugging sandbox behavior. Bundles the Hiver documentation as markdown so an agent can pull only the pages it needs into context.
license: MIT
---

# Hiver docs

Hiver is the runtime for autonomous AI agents: microVM-isolated compute, a file-system layer, an exec/streaming API, network policy, and an event log.

The full documentation is bundled next to this file under \`docs/\` and linked below as relative paths. Read only the pages relevant to the current task instead of loading this whole index's worth of content.

## How to use this skill

1. Scan the page list below and pick the page(s) that match the task (installing, a specific API like exec or file access, a runtime detail, deployment).
2. Open the linked \`docs/*.md\` file for each one — it's plain markdown already on disk.
3. If a page links to another doc page you need, that link is a relative path to another bundled \`.md\` file.

## Pages

${sectionsMd}
`;
}

async function build() {
  // Resolve every nav entry to its page.mdx source, skipping ones that don't
  // exist. Collect the bundled files (for link rewriting) and section index.
  const pages = []; // { local, markdown }
  const sections = []; // { title, links: string[] }

  for (const section of docsNav) {
    const links = [];
    for (const { href, label } of section.links) {
      const slug = href.replace(/^\/docs\/?/, "");
      const segments = slug ? slug.split("/") : [];
      const src = await readDocSource(segments);
      if (src === null) continue;

      const local = slug ? `docs/${slug}.md` : "docs/index.md";
      pages.push({ local, markdown: mdxToMarkdown(src) });
      links.push(`- [${label}](${local})`);
    }
    if (links.length > 0) sections.push({ title: section.title, links });
  }

  if (pages.length === 0) {
    throw new Error(`no doc pages found under ${DOCS_DIR} — is the docs/ source present?`);
  }

  const known = new Set(pages.map((p) => p.local));

  // Fresh output dir so removed pages don't linger.
  await fs.rm(OUT_DIR, { recursive: true, force: true });
  await fs.mkdir(OUT_DIR, { recursive: true });

  await fs.writeFile(path.join(OUT_DIR, "SKILL.md"), buildSkillMd(sections));

  for (const { local, markdown } of pages) {
    const dest = path.join(OUT_DIR, local);
    await fs.mkdir(path.dirname(dest), { recursive: true });
    await fs.writeFile(dest, rewriteDocLinks(markdown, local, known));
  }

  console.log(
    `Wrote skill to ${path.relative(process.cwd(), OUT_DIR)} (SKILL.md + ${pages.length} docs)`
  );
}

build().catch((err) => {
  console.error(`error: generate-skill: ${err.message}`);
  process.exit(1);
});
