import { createHash } from 'node:crypto';
import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { describe, expect, it } from 'vitest';

/**
 * The Content-Security-Policy that nginx sends with every elitea-web page
 * (`nginx/security-headers.conf`).
 *
 * Two kinds of proof:
 *  - the policy string carries the required directives and nothing that
 *    re-opens script execution (no 'unsafe-inline'/'unsafe-eval' in
 *    script-src);
 *  - every inline script the nginx-served HTML carries is authorised by a
 *    sha256 in `script-src`, and no hash is left over from a script that no
 *    longer exists. The hashes are computed here from the source HTML, and from
 *    `dist/` as well when a build output is present, so a changed inline
 *    script fails this test until the header is re-hashed. It needs no CI
 *    change: it runs wherever `vitest --project node` runs, and the `dist/`
 *    half switches on by itself after a build.
 */
const APP_DIR = resolve(import.meta.dirname, '../..');
const NGINX_DIR = resolve(APP_DIR, 'nginx');
const HEADERS_CONF = readFileSync(resolve(NGINX_DIR, 'security-headers.conf'), 'utf8');
const SPA_CONF = readFileSync(resolve(NGINX_DIR, 'spa.conf'), 'utf8');

/** HTML documents nginx serves: the source pages and, after a build, their output. */
const SOURCE_PAGES = ['index.html', 'src/entries/docs/index.html'] as const;
const BUILT_PAGES = ['dist/app/index.html', 'dist/docs/index.html', 'dist/docs/404.html'] as const;

function cspValue(conf: string): string {
  const match = /^add_header\s+Content-Security-Policy\s+"([^"]*)"\s+always;/m.exec(conf);
  if (match === null) throw new Error('security-headers.conf sets no Content-Security-Policy');
  return match[1] ?? '';
}

function directives(policy: string): Map<string, string[]> {
  const map = new Map<string, string[]>();
  for (const part of policy.split(';')) {
    const [name, ...sources] = part.trim().split(/\s+/);
    if (name !== undefined && name !== '') map.set(name, sources);
  }
  return map;
}

/** sha256 source expressions of every inline `<script>` (no `src`) in `html`. */
function inlineScriptHashes(html: string): string[] {
  const hashes: string[] = [];
  for (const match of html.matchAll(/<script(?<attrs>[^>]*)>(?<body>[\s\S]*?)<\/script>/g)) {
    if (/\bsrc\s*=/.test(match.groups?.attrs ?? '')) continue;
    const digest = createHash('sha256').update(match.groups?.body ?? '', 'utf8').digest('base64');
    hashes.push(`'sha256-${digest}'`);
  }
  return hashes;
}

const POLICY = cspValue(HEADERS_CONF);
const DIRECTIVES = directives(POLICY);
const SCRIPT_SRC = DIRECTIVES.get('script-src') ?? [];
const POLICY_HASHES = SCRIPT_SRC.filter((source) => source.startsWith("'sha256-"));

describe('Content-Security-Policy in nginx/security-headers.conf', () => {
  it('carries every required directive', () => {
    for (const name of [
      'default-src',
      'script-src',
      'style-src',
      'img-src',
      'connect-src',
      'font-src',
      'worker-src',
      'frame-ancestors',
      'base-uri',
      'form-action',
      'object-src',
    ]) {
      expect(DIRECTIVES.has(name), `missing ${name}`).toBe(true);
    }
  });

  it('pins the restrictive directives to their strict values', () => {
    expect(DIRECTIVES.get('default-src')).toEqual(["'self'"]);
    expect(DIRECTIVES.get('frame-ancestors')).toEqual(["'none'"]);
    expect(DIRECTIVES.get('object-src')).toEqual(["'none'"]);
    expect(DIRECTIVES.get('base-uri')).toEqual(["'self'"]);
    expect(DIRECTIVES.get('form-action')).toEqual(["'self'"]);
    expect(DIRECTIVES.get('connect-src')).toEqual(["'self'"]);
  });

  it("never allows 'unsafe-inline' or 'unsafe-eval' in script-src", () => {
    expect(SCRIPT_SRC).not.toContain("'unsafe-inline'");
    expect(SCRIPT_SRC).not.toContain("'unsafe-eval'");
    expect(SCRIPT_SRC).not.toContain("'wasm-unsafe-eval'");
    expect(SCRIPT_SRC).not.toContain("'unsafe-hashes'");
  });

  it("limits script-src to 'self' and sha256 hashes (no host, scheme or wildcard)", () => {
    expect(SCRIPT_SRC[0]).toBe("'self'");
    for (const source of SCRIPT_SRC.slice(1)) {
      expect(source).toMatch(/^'sha256-[A-Za-z0-9+/]{43}='$/);
    }
  });

  it("allows 'unsafe-inline' for style-src only", () => {
    expect(DIRECTIVES.get('style-src')).toContain("'unsafe-inline'");
    for (const [name, sources] of DIRECTIVES) {
      if (name === 'style-src') continue;
      expect(sources, name).not.toContain("'unsafe-inline'");
      expect(sources, name).not.toContain("'unsafe-eval'");
    }
  });

  it('does not use a wildcard source in any fetch directive', () => {
    for (const [name, sources] of DIRECTIVES) expect(sources, name).not.toContain('*');
  });

  it('authorises exactly the inline scripts in the source HTML (no missing hash, no stale hash)', () => {
    const wanted = new Set<string>();
    for (const page of SOURCE_PAGES) {
      const hashes = inlineScriptHashes(readFileSync(resolve(APP_DIR, page), 'utf8'));
      expect(hashes.length, `${page} has no inline script to hash`).toBeGreaterThan(0);
      for (const hash of hashes) wanted.add(hash);
    }
    expect([...POLICY_HASHES].sort()).toEqual([...wanted].sort());
  });

  it('has no inline event-handler attribute in the served HTML (script-src would refuse it)', () => {
    for (const page of SOURCE_PAGES) {
      const html = readFileSync(resolve(APP_DIR, page), 'utf8');
      expect(/<[a-z][^>]*\son[a-z]+\s*=/i.test(html), page).toBe(false);
    }
  });

  const builtPages = BUILT_PAGES.filter((page) => existsSync(resolve(APP_DIR, page)));

  it.skipIf(builtPages.length === 0)('authorises every inline script in the built HTML (when dist/ exists)', () => {
    for (const page of builtPages) {
      const hashes = inlineScriptHashes(readFileSync(resolve(APP_DIR, page), 'utf8'));
      expect(hashes.length, `${page} has no inline script`).toBeGreaterThan(0);
      for (const hash of hashes) expect(POLICY_HASHES, page).toContain(hash);
    }
  });
});

/** Top-level `location ... { }` blocks of spa.conf with their bodies. */
function locationBlocks(conf: string): { head: string; body: string }[] {
  const blocks: { head: string; body: string }[] = [];
  const stripped = conf.replace(/^\s*#.*$/gm, '');
  const start = /\blocation\s+([^{]+)\{/g;
  for (let match = start.exec(stripped); match !== null; match = start.exec(stripped)) {
    let depth = 1;
    let index = start.lastIndex;
    while (depth > 0 && index < stripped.length) {
      const char = stripped[index];
      if (char === '{') depth += 1;
      if (char === '}') depth -= 1;
      index += 1;
    }
    blocks.push({ head: (match[1] ?? '').trim(), body: stripped.slice(start.lastIndex, index - 1) });
  }
  return blocks;
}

describe('nginx/spa.conf', () => {
  const blocks = locationBlocks(SPA_CONF);

  it('has location blocks to check', () => {
    expect(blocks.length).toBeGreaterThanOrEqual(10);
  });

  // add_header does not inherit into a location that declares its own, and
  // every location here sets Cache-Control, so each one must include the file.
  it.each(blocks.map((block) => [block.head, block.body] as const))(
    'location %s includes security-headers.conf',
    (_head, body) => {
      expect(body).toContain('include /etc/nginx/snippets/security-headers.conf;');
    },
  );

  it('sets no add_header at server level that a location would silently drop', () => {
    const outsideLocations = locationBlocks(SPA_CONF).reduce((rest, block) => rest.replace(block.body, ''), SPA_CONF);
    expect(outsideLocations).not.toMatch(/^\s*add_header\s/m);
  });

  it('is the only nginx file that sets a Content-Security-Policy', () => {
    expect(SPA_CONF).not.toContain('Content-Security-Policy');
    expect(readFileSync(resolve(NGINX_DIR, 'opener-isolation.conf'), 'utf8')).not.toMatch(
      /^add_header\s+Content-Security-Policy/m,
    );
  });
});
