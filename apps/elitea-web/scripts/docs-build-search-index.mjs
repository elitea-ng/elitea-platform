#!/usr/bin/env node
/**
 * Builds the docs search index (PREAMBLE decision 2). Walks
 * `src/entries/docs/content/**\/*.mdx`, strips MDX/JSX/Markdown syntax down
 * to plain prose, and writes `src/entries/docs/search-index.generated.json`
 * — the file `search.ts` loads into a `minisearch` instance at runtime.
 *
 * Run automatically as the first step of `npm run build:docs`. The output is
 * committed (like `src/routeTree.gen.ts`): a docs page edited without
 * re-running this script leaves a STALE index rather than a missing one, so
 * `tsc --noEmit` and a plain `npm run dev` never trip over an absent file.
 * Re-run it by hand after editing `content/**` between builds.
 *
 * No remark/rehype dependency here on purpose: search text does not need a
 * real AST, and keeping this script free of the vite.config.ts MDX toolchain
 * means it can run standalone, fast, with no Vite pipeline spun up.
 */
import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const DOCS_DIR = resolve(SCRIPT_DIR, '../src/entries/docs');
const CONTENT_DIR = join(DOCS_DIR, 'content');
const OUTPUT_FILE = join(DOCS_DIR, 'search-index.generated.json');

/** Every `.mdx` file under `dir`, recursively, `img/` excluded (binary
 * assets, never content). */
function walkMdxFiles(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    if (entry === 'img') continue;
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      out.push(...walkMdxFiles(full));
    } else if (entry.endsWith('.mdx')) {
      out.push(full);
    }
  }
  return out;
}

/** `dir/page.mdx` → `dir/page`; `index.mdx` at the content root → `''`
 * (the home page's slug, matching `nav.ts`'s convention). */
function slugFor(filePath) {
  const rel = relative(CONTENT_DIR, filePath).replace(/\\/g, '/').replace(/\.mdx$/, '');
  return rel === 'index' ? '' : rel;
}

const FRONTMATTER_RE = /^---\n([\s\S]*?)\n---\n?/;

/** A deliberately tiny frontmatter reader: `title:` and `description:` as
 * plain unquoted-or-quoted scalar strings, which is all this contract uses
 * (PREAMBLE decision 2 — no lists, no nesting). Good enough for a search
 * index; NOT a general YAML parser. */
function parseFrontmatter(source) {
  const match = FRONTMATTER_RE.exec(source);
  if (match === null) return { title: '', body: source };
  const block = match[1];
  const titleMatch = /^title:\s*(.+)$/m.exec(block);
  const title = titleMatch ? titleMatch[1].trim().replace(/^["']|["']$/g, '') : '';
  const body = source.slice(match[0].length);
  return { title, body };
}

/** Strips MDX/JSX/Markdown syntax down to prose worth indexing. Order
 * matters: fenced code and JSX tags are removed before the lighter-weight
 * inline markdown substitutions, so a `<Card title="...">` never leaves its
 * attribute text behind as prose. */
function toPlainText(mdx) {
  return mdx
    .replace(/^import .+$/gm, '')
    .replace(/^export .+$/gm, '')
    .replace(/```[\s\S]*?```/g, ' ')
    .replace(/<[^>]+>/g, ' ')
    .replace(/!\[[^\]]*\]\([^)]*\)/g, ' ')
    .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
    .replace(/^#{1,6}\s+/gm, '')
    .replace(/[*_`>#-]/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
}

function buildEntries() {
  const files = walkMdxFiles(CONTENT_DIR).sort();
  return files.map((filePath) => {
    const source = readFileSync(filePath, 'utf8');
    const { title, body } = parseFrontmatter(source);
    const slug = slugFor(filePath);
    return {
      id: slug === '' ? 'index' : slug,
      slug,
      title,
      text: toPlainText(body),
    };
  });
}

function main() {
  const entries = buildEntries();
  writeFileSync(OUTPUT_FILE, `${JSON.stringify(entries, null, 2)}\n`);
  console.log(`docs-build-search-index: wrote ${entries.length} entries to ${relative(process.cwd(), OUTPUT_FILE)}`);
}

main();
