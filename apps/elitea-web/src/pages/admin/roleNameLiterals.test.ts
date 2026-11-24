import type { Dirent } from 'node:fs';
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, relative, sep } from 'node:path';

import { describe, expect, it } from 'vitest';

import reviewed from './roleNameLiterals.reviewed.json';

/**
 * Every client check by role NAME is reviewed (ADR-0059 decision 7, "Checks
 * by role name", story R0.13).
 *
 * A check that compares a role with the literal `super_admin` or `admin`
 * misses a custom role and reads nothing an elevation gives. Each such site
 * carries the decision the ADR made for it in `roleNameLiterals.reviewed.json`;
 * this test fails on a comparison the file does not list, and on a listed one
 * that is gone. The server has the same test over its Go and SQL:
 * `services/elitea-main/internal/api/role_name_literals_test.go`.
 *
 * A site is a comparison in a non-test TypeScript file of the three clients
 * (web, desktop, mobile): `=== 'super_admin'` (either side, `==`, `!=`,
 * `!==`), a `case 'admin':`, or an `.includes('admin')`. A query key, a tab
 * value or a label is no comparison and is not read. The site is keyed by its
 * file and the trimmed source line, not the line number, so an edit elsewhere
 * in the file does not move it.
 */

interface ReviewedSite {
  readonly file: string;
  readonly comparison: string;
  readonly count: number;
  readonly decision: string;
  readonly why: string;
}

const DECISIONS = new Set(['kept', 'compat', 'project', 'unrelated']);

/** The client source roots, relative to `apps/`. */
const ROOTS = ['elitea-web/src', 'elitea-desktop/src', 'elitea-mobile/src'] as const;

const SKIP_DIRS = new Set(['node_modules', 'dist', 'coverage', '.git', '__tests__', '__mocks__', 'generated']);
const SOURCE_RE = /\.(ts|tsx)$/;
const TEST_RE = /\.(test|spec|stories)\.(ts|tsx)$/;

const NAME = String.raw`['"\`](?:super_admin|admin)['"\`]`;
const COMPARISON_RE = new RegExp(
  String.raw`(?:[!=]==?\s*${NAME})|(?:${NAME}\s*[!=]==?)|(?:\bcase\s+${NAME}\s*:)|(?:\.includes\(\s*${NAME}\s*\))`,
);

/** Every comparison under `appsDir`, counted per file and trimmed line. */
function scanRoleNameComparisons(appsDir: string): Map<string, number> {
  const found = new Map<string, number>();
  const walk = (dir: string): void => {
    let entries: Dirent<string>[];
    try {
      entries = readdirSync(dir, { withFileTypes: true, encoding: 'utf8' });
    } catch {
      return;
    }
    for (const entry of entries) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) {
        if (!SKIP_DIRS.has(entry.name)) walk(path);
        continue;
      }
      if (!SOURCE_RE.test(entry.name) || TEST_RE.test(entry.name)) continue;
      const file = relative(appsDir, path).split(sep).join('/');
      for (const line of readFileSync(path, 'utf8').split('\n')) {
        const trimmed = line.trim();
        if (trimmed.startsWith('//') || trimmed.startsWith('*') || !COMPARISON_RE.test(trimmed)) continue;
        const key = `${file} | ${trimmed}`;
        found.set(key, (found.get(key) ?? 0) + 1);
      }
    }
  };
  for (const root of ROOTS) walk(join(appsDir, root));
  return found;
}

function compareWithReviewed(found: Map<string, number>, sites: readonly ReviewedSite[]): string[] {
  const problems: string[] = [];
  const listed = new Map(sites.map((site) => [`${site.file} | ${site.comparison}`, site]));
  for (const [key, count] of found) {
    const site = listed.get(key);
    if (!site) {
      problems.push(
        `unreviewed role name comparison: ${key}. Test a permission instead, or add the site with its decision to src/pages/admin/roleNameLiterals.reviewed.json.`,
      );
    } else if (site.count !== count) {
      problems.push(`${key}: ${count} sites, the reviewed row says ${site.count}.`);
    }
  }
  for (const key of listed.keys()) {
    if (!found.has(key)) problems.push(`stale reviewed row: ${key} is no longer in the source.`);
  }
  for (const site of sites) {
    if (!DECISIONS.has(site.decision) || site.why.trim() === '') {
      problems.push(`${site.file} | ${site.comparison}: decision "${site.decision}" or its reason is invalid.`);
    }
  }
  return problems.sort();
}

/** `apps/`, from this file's directory (`apps/elitea-web/src/pages/admin`). */
const APPS_DIR = join(import.meta.dirname, '..', '..', '..', '..');

describe('role name comparisons (ADR-0059 R0.13)', () => {
  it('lists every comparison with super_admin and admin in the reviewed file', () => {
    const found = scanRoleNameComparisons(APPS_DIR);
    expect(found.size, 'the scan found nothing; it is broken').toBeGreaterThan(0);
    expect(compareWithReviewed(found, reviewed.sites)).toEqual([]);
  });

  it('fails on an unreviewed comparison and on a stale row, and skips tests and labels', () => {
    const apps = mkdtempSync(join(tmpdir(), 'role-names-'));
    try {
      const dir = join(apps, 'elitea-web', 'src', 'feature');
      mkdirSync(dir, { recursive: true });
      writeFileSync(
        join(dir, 'roles.ts'),
        [
          "export const isOwner = (role: string): boolean => role === 'super_admin';",
          "export const key = ['admin', 'users'] as const;",
          "// role === 'admin' in a comment is no site",
          "export const held = (roles: string[]): boolean => roles.includes('admin');",
        ].join('\n'),
      );
      writeFileSync(join(dir, 'roles.test.ts'), "expect(role === 'admin').toBe(true);\n");

      const found = scanRoleNameComparisons(apps);
      expect([...found.keys()].sort()).toEqual([
        "elitea-web/src/feature/roles.ts | export const held = (roles: string[]): boolean => roles.includes('admin');",
        "elitea-web/src/feature/roles.ts | export const isOwner = (role: string): boolean => role === 'super_admin';",
      ]);

      const problems = compareWithReviewed(found, [
        {
          file: 'elitea-web/src/feature/roles.ts',
          comparison: "export const isOwner = (role: string): boolean => role === 'super_admin';",
          count: 1,
          decision: 'kept',
          why: 'fixture',
        },
        { file: 'elitea-web/src/feature/gone.ts', comparison: "x === 'admin'", count: 1, decision: 'kept', why: 'fixture' },
      ]);
      expect(problems).toHaveLength(2);
      expect(problems.join('\n')).toContain("roles.includes('admin')");
      expect(problems.join('\n')).toContain('stale reviewed row: elitea-web/src/feature/gone.ts');
    } finally {
      rmSync(apps, { recursive: true, force: true });
    }
  });
});
