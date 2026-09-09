#!/usr/bin/env node
/**
 * Builds two generated JSON files from `src/entries/docs/content/**\/*.mdx`
 * (PREAMBLE decision 2):
 *
 *  - `search-index.generated.json` — `{ id, slug, title, text }` per page,
 *    `text` being the full page stripped down to plain prose. `search.ts`
 *    loads this into a `minisearch` instance, LAZILY (dynamic `import()`):
 *    at ~85 KiB gzip for the real 126-page content set, it is too heavy to
 *    ship before first paint, and nothing needs search results before a
 *    reader actually types a query.
 *  - `page-meta.generated.json` — `{ slug, title, description }` per page,
 *    no prose. `content-registry.ts` loads this EAGERLY: it needs
 *    frontmatter synchronously (nav/search/prev-next, the page's `<h1>`,
 *    `document.title`, `<meta name="description">`) while still loading
 *    each page's actual component LAZILY, one chunk per slug. Reading
 *    frontmatter off a real per-page `import.meta.glob(..., { eager: true,
 *    import: 'frontmatter' })` looked like the obvious way to do that, but
 *    Rollup then reports every page as both statically AND dynamically
 *    imported from the same module (`content-registry.ts`) and folds every
 *    page's compiled component back into the eagerly-loaded graph — the
 *    exact bundle bloat lazy-loading exists to remove (measured: 353 KiB
 *    gzip initial, no smaller than the all-eager baseline). Reusing the
 *    heavier `search-index.generated.json` for this instead (it already has
 *    `title`/`description`) would reintroduce the same problem one level
 *    up: `content-registry.ts` importing THAT file eagerly would pull its
 *    ~85 KiB of prose into the initial bundle just to reach two small
 *    fields. A separate, small, prose-free file is what actually stays
 *    eager-safe.
 *
 * Run automatically as the first step of `npm run build:docs`. Both outputs
 * are committed (like `src/routeTree.gen.ts`): a docs page edited without
 * re-running this script leaves them STALE rather than missing, so
 * `tsc --noEmit` and a plain `npm run dev` never trip over an absent file.
 * Re-run it by hand after editing `content/**` between builds.
 *
 * No remark/rehype dependency here on purpose: neither output needs a real
 * AST, and keeping this script free of the vite.config.ts MDX toolchain
 * means it can run standalone, fast, with no Vite pipeline spun up.
 */
import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const DOCS_DIR = resolve(SCRIPT_DIR, '../src/entries/docs');
const CONTENT_DIR = join(DOCS_DIR, 'content');
const SEARCH_INDEX_FILE = join(DOCS_DIR, 'search-index.generated.json');
const PAGE_META_FILE = join(DOCS_DIR, 'page-meta.generated.json');

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
 * index (and, now, `content-registry.ts`'s per-page metadata); NOT a general
 * YAML parser. */
function parseFrontmatter(source) {
  const match = FRONTMATTER_RE.exec(source);
  if (match === null) return { title: '', description: '', body: source };
  const block = match[1];
  const readField = (name) => {
    const fieldMatch = new RegExp(`^${name}:\\s*(.+)$`, 'm').exec(block);
    return fieldMatch ? fieldMatch[1].trim().replace(/^["']|["']$/g, '') : '';
  };
  const body = source.slice(match[0].length);
  return { title: readField('title'), description: readField('description'), body };
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

function buildPages() {
  const files = walkMdxFiles(CONTENT_DIR).sort();
  return files.map((filePath) => {
    const source = readFileSync(filePath, 'utf8');
    const { title, description, body } = parseFrontmatter(source);
    const slug = slugFor(filePath);
    return { slug, title, description, body };
  });
}

function main() {
  const pages = buildPages();

  const searchEntries = pages.map((page) => ({
    id: page.slug === '' ? 'index' : page.slug,
    slug: page.slug,
    title: page.title,
    text: toPlainText(page.body),
  }));
  writeFileSync(SEARCH_INDEX_FILE, `${JSON.stringify(searchEntries, null, 2)}\n`);

  const pageMetaEntries = pages.map((page) => ({
    slug: page.slug,
    title: page.title,
    description: page.description,
  }));
  writeFileSync(PAGE_META_FILE, `${JSON.stringify(pageMetaEntries, null, 2)}\n`);

  console.log(
    `docs-build-search-index: wrote ${searchEntries.length} entries to ${relative(process.cwd(), SEARCH_INDEX_FILE)} ` +
      `and ${pageMetaEntries.length} entries to ${relative(process.cwd(), PAGE_META_FILE)}`,
  );
}

main();
