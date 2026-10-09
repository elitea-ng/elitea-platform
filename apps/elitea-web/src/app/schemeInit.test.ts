import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { runInNewContext } from 'node:vm';

import { afterEach, describe, expect, it } from 'vitest';

import { COLOR_SCHEME_ATTRIBUTE, DEFAULT_COLOR_SCHEME, INIT_COLOR_SCHEME_PROPS } from '@/shared/brand';

/**
 * The first-paint colour-scheme script (ADR-0024 WP3) exists in two places:
 * inline in `index.html` (the main SPA; nginx pins it by sha256 in script-src) and as the
 * file `entries/admin/public/assets/scheme-init.js` (the admin CSP hash-pins
 * exactly one inline script). This test (1) pins the two bodies equal, (2)
 * pins them to the three constants the runtime theme reads, and (3) RUNS the
 * body — in a `node:vm` context whose globals stand in for the browser's —
 * for every stored-mode case.
 */
const APP_DIR = resolve(import.meta.dirname, '../..');
const INDEX_HTML = readFileSync(resolve(APP_DIR, 'index.html'), 'utf8');
const ADMIN_HTML = readFileSync(resolve(APP_DIR, 'src/entries/admin/index.html'), 'utf8');
const ADMIN_FILE = readFileSync(resolve(APP_DIR, 'src/entries/admin/public/assets/scheme-init.js'), 'utf8');

/** The inline body: the one plain `<script>` (no src, no type) in index.html. */
function inlineBody(html: string): string {
  const match = /<script>([\s\S]*?)<\/script>/.exec(html);
  if (match === null) throw new Error('index.html carries no inline <script>');
  return match[1] ?? '';
}

/** Comment lines and indentation are documentation; the executable text must match. */
function normalise(source: string): string {
  return source
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '' && !line.startsWith('//'))
    .join('\n');
}

const MAIN_BODY = inlineBody(INDEX_HTML);

interface BrowserLike {
  localStorage?: { getItem(key: string): string | null };
  matchMedia: (query: string) => { matches: boolean };
  document: Document;
}

function runScheme(body: string, stored: string | null, prefersDark: boolean): string | null {
  document.documentElement.removeAttribute(COLOR_SCHEME_ATTRIBUTE);
  const sandbox: BrowserLike = {
    localStorage: { getItem: (key) => (key === INIT_COLOR_SCHEME_PROPS.modeStorageKey ? stored : null) },
    matchMedia: (query) => ({ matches: prefersDark && query.includes('dark') }),
    document,
  };
  runInNewContext(body, sandbox);
  return document.documentElement.getAttribute(COLOR_SCHEME_ATTRIBUTE);
}

afterEach(() => {
  document.documentElement.removeAttribute(COLOR_SCHEME_ATTRIBUTE);
});

describe('first-paint colour-scheme script', () => {
  it('has the same executable body inline in index.html and in the admin static file', () => {
    expect(normalise(MAIN_BODY)).toBe(normalise(ADMIN_FILE));
  });

  it('is wired into both entries ahead of the bundle', () => {
    expect(INDEX_HTML.indexOf('<script>')).toBeLessThan(INDEX_HTML.indexOf('/src/app/main.tsx'));
    expect(ADMIN_HTML).toContain('<script src="/assets/scheme-init.js"></script>');
    expect(ADMIN_HTML.indexOf('scheme-init.js')).toBeLessThan(ADMIN_HTML.indexOf('./main.tsx'));
    // The admin entry loads channel C too (it rendered the default brand without it).
    expect(ADMIN_HTML).toContain('<script src="/api/v2/branding/bootstrap.js" vite-ignore></script>');
    // The admin CSP admits ONE inline script (the config marker the Go handler fills).
    expect(ADMIN_HTML.match(/<script>/g)).toBeNull();
  });

  it('speaks the same three literals as the runtime theme', () => {
    expect(MAIN_BODY).toContain(`"${INIT_COLOR_SCHEME_PROPS.modeStorageKey}"`);
    expect(MAIN_BODY).toContain(`"${COLOR_SCHEME_ATTRIBUTE}"`);
    expect(MAIN_BODY).toContain(`let mode = "${DEFAULT_COLOR_SCHEME}"`);
  });

  it.each([
    ['nothing stored', null, false, DEFAULT_COLOR_SCHEME],
    ['light stored', 'light', true, 'light'],
    ['dark stored', 'dark', false, 'dark'],
    ['system + OS dark', 'system', true, 'dark'],
    ['system + OS light', 'system', false, 'light'],
    ['garbage stored', 'purple', false, DEFAULT_COLOR_SCHEME],
  ])('%s → data-el-scheme=%s', (_label, stored, prefersDark, expected) => {
    expect(runScheme(MAIN_BODY, stored, prefersDark)).toBe(expected);
  });

  it('falls back to the default when storage throws', () => {
    document.documentElement.removeAttribute(COLOR_SCHEME_ATTRIBUTE);
    const sandbox: BrowserLike = { matchMedia: () => ({ matches: true }), document };
    Object.defineProperty(sandbox, 'localStorage', {
      get() {
        throw new Error('SecurityError');
      },
    });
    runInNewContext(MAIN_BODY, sandbox);
    expect(document.documentElement.getAttribute(COLOR_SCHEME_ATTRIBUTE)).toBe(DEFAULT_COLOR_SCHEME);
  });

  it('falls back to the default when storage is absent altogether', () => {
    document.documentElement.removeAttribute(COLOR_SCHEME_ATTRIBUTE);
    runInNewContext(MAIN_BODY, { matchMedia: () => ({ matches: true }), document });
    expect(document.documentElement.getAttribute(COLOR_SCHEME_ATTRIBUTE)).toBe(DEFAULT_COLOR_SCHEME);
  });
});
