#!/usr/bin/env node
/**
 * Checks the BUILT site (run after `npm run build`):
 *
 *  - every internal link resolves to a built page, and its #anchor to an id
 *    on that page; no link escapes the base (a root-relative link without
 *    it would 404 under /docs/ and on GitHub Pages);
 *  - every inline <script> is listed by hash in nginx/security-headers.conf,
 *    the CSP the web image serves /docs/ with (no 'unsafe-inline').
 *    `--write-csp` rewrites that list from the build instead.
 *
 * DOCS_BASE must match the build's (default /docs/).
 */
import { createHash } from 'node:crypto';
import { existsSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const DIST = join(ROOT, 'dist');
const CSP_FILE = join(ROOT, 'nginx/security-headers.conf');
const base = process.env.DOCS_BASE ?? '/docs/';
const writeCsp = process.argv.includes('--write-csp');

function walk(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) return entry.name === 'pagefind' || entry.name === '_astro' ? [] : walk(full);
    return entry.name.endsWith('.html') ? [full] : [];
  });
}

const pages = new Map();
for (const file of walk(DIST)) {
  const html = readFileSync(file, 'utf8');
  pages.set(relative(DIST, file).replace(/\\/g, '/'), {
    ids: new Set([...html.matchAll(/\sid="([^"]+)"/g)].map((m) => m[1])),
    hrefs: [...html.matchAll(/<a\s[^>]*href="([^"]*)"/g)].map((m) => m[1].replaceAll('&amp;', '&')),
    scripts: [...html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g)].map((m) => m[1]),
  });
}

function pageFor(path) {
  const clean = decodeURIComponent(path).replace(/^\/+/, '');
  for (const candidate of [clean, `${clean.replace(/\/$/, '')}/index.html`, `${clean}.html`]) {
    if (pages.has(candidate)) return candidate;
  }
  return existsSync(join(DIST, clean)) ? clean : undefined;
}

const problems = [];
for (const [page, { hrefs }] of pages) {
  for (const href of hrefs) {
    if (/^([a-z]+:|\/\/)/i.test(href)) continue;
    const [path, anchor] = href.split('#');
    let target = page;
    if (path !== '') {
      if (!path.startsWith('/')) continue; // relative links are Starlight's own (none in content)
      if (!path.startsWith(base)) {
        problems.push(`${page}: ${href} is outside the base ${base}`);
        continue;
      }
      target = pageFor(path.slice(base.length));
      if (target === undefined) {
        problems.push(`${page}: ${href} matches no page`);
        continue;
      }
    }
    if (anchor && anchor !== '_top' && pages.has(target) && !pages.get(target).ids.has(decodeURIComponent(anchor))) {
      problems.push(`${page}: ${href} names no heading on ${target}`);
    }
  }
}

const hashes = [...new Set([...pages.values()].flatMap((p) => p.scripts).map((s) => `'sha256-${createHash('sha256').update(s).digest('base64')}'`))].sort();
const csp = readFileSync(CSP_FILE, 'utf8');
const marker = /(script-src 'self' 'wasm-unsafe-eval')[^;]*;/;
if (writeCsp) {
  writeFileSync(CSP_FILE, csp.replace(marker, `$1 ${hashes.join(' ')};`));
  console.log(`wrote ${hashes.length} inline script hashes to ${relative(ROOT, CSP_FILE)}`);
} else {
  const listed = new Set(marker.exec(csp)?.[0].match(/'sha256-[^']+'/g) ?? []);
  for (const hash of hashes) if (!listed.has(hash)) problems.push(`inline script ${hash} is not in ${relative(ROOT, CSP_FILE)} (run with --write-csp after checking it)`);
  for (const hash of listed) if (!hashes.includes(hash)) problems.push(`${relative(ROOT, CSP_FILE)} lists ${hash}, which no page has`);
}

if (pages.size < 100) problems.push(`only ${pages.size} pages were built`);
for (const problem of problems) console.error(problem);
console.log(`${pages.size} pages, ${[...pages.values()].reduce((n, p) => n + p.hrefs.length, 0)} links, ${hashes.length} inline scripts: ${problems.length} problem(s)`);
process.exit(problems.length === 0 ? 0 : 1);
