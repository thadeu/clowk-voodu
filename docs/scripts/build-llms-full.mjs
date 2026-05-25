// build-llms-full.mjs
//
// Generates public/llms-full.txt by concatenating every MDX file
// under content/docs/ into one large markdown document. Run via the
// prebuild hook in package.json so the file is fresh on every
// deploy.
//
// Why a script instead of an app/llms-full/route.ts: this site uses
// `output: 'export'`, so route handlers are skipped at build time
// and `public/` is the only path to ship a static text file at the
// site root. Writing to public/ before `next build` runs is the
// simplest static pipeline.
//
// File format: stripped frontmatter, document order matches the
// site's nav (overview-first, then alphabetic within each section).
// Each page is preceded by a "# Title" + "URL: ..." block so LLM
// consumers can attribute snippets back to the canonical page.

import { readFile, readdir, writeFile, stat } from 'node:fs/promises';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = fileURLToPath(new URL('.', import.meta.url));
const ROOT = join(__dirname, '..');
const CONTENT_DIR = join(ROOT, 'content', 'docs');
const OUTPUT = join(ROOT, 'public', 'llms-full.txt');
const SITE_URL = 'https://voodu.clowk.in';

// Document order — overview entry points first within each section,
// everything else alphabetic. Matches the nav rendering on the live
// docs site so an LLM reading top-down sees the same shape an
// operator scrolling the sidebar would.
const SECTION_ORDER = [
  'getting-started',
  'manifests',
  'plugins',
  'cli',
  'architecture',
  'examples',
  'reference',
];

// Within a section, these names jump to the top. Everything else
// stays alphabetical so adding a new page doesn't require touching
// this script.
const SECTION_HEAD = {
  manifests: ['overview', 'deployment', 'statefulset', 'app'],
  architecture: ['overview', 'controller', 'reconciler'],
  examples: ['overview', 'hello-world', 'production-stack'],
  cli: ['apply', 'diff', 'logs'],
  plugins: ['build-your-own'],
};

async function walk(dir) {
  const entries = await readdir(dir, { withFileTypes: true });
  const out = [];

  for (const e of entries) {
    const p = join(dir, e.name);

    if (e.isDirectory()) {
      out.push(...(await walk(p)));
    } else if (e.isFile() && e.name.endsWith('.mdx')) {
      out.push(p);
    }
  }

  return out;
}

// Plain-frontmatter parser. The MDX files use the YAML-front-matter
// dialect Fumadocs ships — `title` and `description` strings, no
// nested structures. Avoids pulling gray-matter just for two
// fields.
function parseFrontmatter(src) {
  const match = src.match(/^---\n([\s\S]*?)\n---\n?([\s\S]*)$/);

  if (!match) {
    return { data: {}, content: src };
  }

  const data = {};

  for (const line of match[1].split('\n')) {
    const idx = line.indexOf(':');

    if (idx === -1) continue;

    const key = line.slice(0, idx).trim();
    let value = line.slice(idx + 1).trim();

    if (value.startsWith('"') && value.endsWith('"')) {
      value = value.slice(1, -1);
    } else if (value.startsWith("'") && value.endsWith("'")) {
      value = value.slice(1, -1);
    }

    data[key] = value;
  }

  return { data, content: match[2] };
}

function pathToUrl(absPath) {
  const rel = relative(CONTENT_DIR, absPath).replace(/\\/g, '/');
  const noExt = rel.replace(/\.mdx$/, '');
  const noIndex = noExt.replace(/\/?index$/, '');

  return `${SITE_URL}/docs${noIndex ? '/' + noIndex : ''}/`;
}

// Sort key: (section index, head-priority index, name).
function sortKey(absPath) {
  const rel = relative(CONTENT_DIR, absPath).replace(/\\/g, '/');
  const parts = rel.replace(/\.mdx$/, '').split('/');

  // Top-level index.mdx ranks before any section.
  if (parts.length === 1 && parts[0] === 'index') {
    return [-1, 0, ''];
  }

  const section = parts[0];
  const tail = parts.slice(1).join('/') || 'index';

  const sIdx = SECTION_ORDER.indexOf(section);
  const sectionRank = sIdx === -1 ? SECTION_ORDER.length : sIdx;

  const heads = SECTION_HEAD[section] || [];
  const headIdx = heads.indexOf(tail);
  const headRank = headIdx === -1 ? heads.length : headIdx;

  return [sectionRank, headRank, tail];
}

function compareSortKeys(a, b) {
  for (let i = 0; i < Math.max(a.length, b.length); i++) {
    if (a[i] < b[i]) {
      return -1;
    }

    if (a[i] > b[i]) {
      return 1;
    }
  }

  return 0;
}

async function main() {
  const files = await walk(CONTENT_DIR);

  files.sort((a, b) => compareSortKeys(sortKey(a), sortKey(b)));

  const header = [
    '# Voodu — full documentation',
    '',
    '> This file concatenates the entire voodu documentation in a single text stream',
    '> for LLM ingestion. Source: https://voodu.clowk.in/docs',
    '> Curated index: https://voodu.clowk.in/llms.txt',
    '',
    `Last built: ${new Date().toISOString()}`,
    '',
    '---',
    '',
  ].join('\n');

  const stamp = await stat(CONTENT_DIR);
  const blocks = [];

  for (const path of files) {
    const raw = await readFile(path, 'utf8');
    const { data, content } = parseFrontmatter(raw);
    const url = pathToUrl(path);
    const title = data.title || relative(CONTENT_DIR, path);
    const description = data.description ? `\n> ${data.description}\n` : '\n';

    blocks.push(
      `# ${title}\n\nURL: ${url}${description}\n${content.trim()}\n`,
    );
  }

  const out = header + blocks.join('\n---\n\n');

  await writeFile(OUTPUT, out, 'utf8');

  const bytes = Buffer.byteLength(out, 'utf8');

  console.warn(
    `[build-llms-full] wrote ${files.length} pages → ${OUTPUT} (${(bytes / 1024).toFixed(1)} KB, content mtime ${stamp.mtime.toISOString()})`,
  );
}

main().catch((err) => {
  console.error('[build-llms-full] failed:', err);
  process.exit(1);
});
