/**
 * Source checks on the docs content, before a build (`npm run check`).
 * Links and heading anchors are checked on the built HTML instead
 * (scripts/check-dist.mjs), where every link a component renders exists too.
 */
import assert from 'node:assert/strict';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const CONTENT = join(ROOT, 'src/content/docs');
const SCREENSHOTS = join(ROOT, 'src/screenshots');
const MAX_SCREENSHOT_BYTES = 250 * 1024;

// The component contract: src/plugins/remark-docs-components.ts.
const COMPONENTS = new Set(
  [...readFileSync(join(ROOT, 'src/plugins/remark-docs-components.ts'), 'utf8').matchAll(/^  (\w+): '/gm)].map((m) => m[1]),
);

function walk(dir) {
  return readdirSync(dir).flatMap((entry) => {
    const full = join(dir, entry);
    return statSync(full).isDirectory() ? walk(full) : entry.endsWith('.mdx') ? [full] : [];
  });
}

const pages = walk(CONTENT).map((file) => {
  const source = readFileSync(file, 'utf8');
  const match = /^---\n([\s\S]*?)\n---\n?/.exec(source);
  const front = match?.[1] ?? '';
  const body = source.slice(match?.[0].length ?? 0);
  const prose = body.replace(/```[\s\S]*?```/g, '');
  const field = (name) => new RegExp(`^${name}:\\s*(.+)$`, 'm').exec(front)?.[1]?.trim();
  const slug = relative(CONTENT, file).replace(/\\/g, '/').replace(/\.mdx$/, '');
  return {
    slug: slug === 'index' ? '' : slug,
    title: field('title'),
    description: field('description'),
    h1: [...prose.matchAll(/^#(?!#)\s+(.+)$/gm)].map((m) => m[1]),
    screenshots: [...prose.matchAll(/<Screenshot\s+[^>]*\bid=["']([^"']+)["']/g)].map((m) => m[1]),
    mermaid: [...body.matchAll(/^```mermaid\n([\s\S]*?)^```/gm)].map((m) => m[1]),
    components: new Set([...prose.replace(/`[^`\n]*`/g, '').matchAll(/<\/?([A-Z][A-Za-z0-9]*)\b/g)].map((m) => m[1])),
  };
});

const navSlugs = readdirSync(join(ROOT, 'src/nav')).flatMap((file) =>
  [...readFileSync(join(ROOT, 'src/nav', file), 'utf8').matchAll(/slug: '([^']*)'/g)].map((m) => m[1]),
);
const manifestIds = new Set([...readFileSync(join(ROOT, 'shots.manifest.ts'), 'utf8').matchAll(/^\s+id: "([^"]+)"/gm)].map((m) => m[1]));
const screenshotFiles = readdirSync(SCREENSHOTS).filter((f) => f.endsWith('.webp'));

describe('docs content', () => {
  it('has pages', () => assert.ok(pages.length > 100, `only ${pages.length} pages found`));

  it('nav and content files are a bijection', () => {
    const files = new Set(pages.map((p) => p.slug));
    const nav = new Set(navSlugs);
    assert.deepEqual([...nav].filter((s) => !files.has(s)), [], 'nav slugs with no file');
    assert.deepEqual([...files].filter((s) => !nav.has(s)), [], 'files missing from the nav');
    assert.equal(navSlugs.length, nav.size, 'a slug is listed twice in the nav');
  });

  it('every page has a title and a description, and no H1 of its own', () => {
    for (const page of pages) {
      assert.ok(page.title, `${page.slug || 'index'}: no title`);
      assert.ok(page.description, `${page.slug || 'index'}: no description`);
      assert.deepEqual(page.h1, [], `${page.slug || 'index'}: the title is the H1`);
    }
  });

  it('every component used is part of the contract', () => {
    for (const page of pages) {
      const unknown = [...page.components].filter((name) => !COMPONENTS.has(name));
      assert.deepEqual(unknown, [], `${page.slug || 'index'} uses components outside the contract`);
    }
  });

  // The browser shows a diagram that does not parse as its source text, so
  // a syntax error would reach readers with every other check green.
  it('every mermaid diagram parses', async () => {
    // Some diagram types sanitise labels while parsing, and DOMPurify has no
    // DOM in Node; parsing needs no real sanitising, so it passes text through.
    const { default: purify } = await import('dompurify');
    Object.assign(purify, { addHook() {}, removeHook() {}, removeHooks() {}, sanitize: (text) => String(text) });
    const { default: mermaid } = await import('mermaid');
    const diagrams = pages.flatMap((page) => page.mermaid.map((source) => ({ slug: page.slug, source })));
    assert.ok(diagrams.length > 0, 'no mermaid diagrams found: the fence pattern no longer matches');
    for (const { slug, source } of diagrams) {
      await assert.doesNotReject(mermaid.parse(source), `${slug}: a mermaid diagram does not parse`);
    }
  });

  it('every <Screenshot id> is in shots.manifest.ts', () => {
    for (const page of pages) {
      for (const id of page.screenshots) assert.ok(manifestIds.has(id), `${page.slug}: ${id} is not in shots.manifest.ts`);
    }
  });

  it('every committed screenshot is referenced and at most 250 KB', () => {
    const referenced = new Set(pages.flatMap((p) => p.screenshots));
    for (const file of screenshotFiles) {
      assert.ok(referenced.has(file.replace(/\.webp$/, '')), `${file} is referenced by no page`);
      const size = statSync(join(SCREENSHOTS, file)).size;
      assert.ok(size <= MAX_SCREENSHOT_BYTES, `${file} is ${size} bytes, over 250 KB`);
    }
  });
});
