// @vitest-environment node
/**
 * The source guard of the dangerous actions (ADR-0059 decision 6; R3.8b,
 * issue 2614). The marked actions are the server's action table, written to
 * `adminDangerousActions.json` by the golden test
 * `TestAdminDangerousActionsFileMatchesTheActionTable`, so a new marked
 * action on the server fails here until the console asks for its reason.
 *
 * A source test, the pattern of `mutationWrapper.guard.test.ts`. It reads the
 * admin page sources (tests excluded) and checks:
 *
 *  1. every action key the sources name ('METHOD /api/… [outcome]') is a key
 *     of the registry, so a renamed route cannot leave a dead reason behind;
 *  2. every registry key is named by a source, or is in NOT_CONVERTED with
 *     the reason it has no console control here;
 *  3. every API hook that sends a reason is called only from a file that
 *     renders the dialog (`<AdminDangerousActionDialog` or
 *     `useAdminDangerousConfirm(`), directly or through the files that
 *     import it, so no reason is sent that an operator did not give;
 *  4. only `adminDangerousAction.ts` builds the reason (`reason_code`, the
 *     `Elitea-Reason` headers), so every request encodes and caps it the
 *     same way;
 *  5. every call of a generated client function of a marked operation sits
 *     in a hook that sends a reason.
 */
import { readFileSync, readdirSync } from 'node:fs';
import { basename, dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import registry from '../adminDangerousActions.json';

const ADMIN_DIR = fileURLToPath(new URL('..', import.meta.url));
const API_DIR = join(ADMIN_DIR, 'api');

/**
 * Registry keys without a console control in this change. Each entry says
 * why; remove it when a control sends the reason.
 */
const NOT_CONVERTED: Readonly<Record<string, string>> = {
  'PUT /api/v2/admin/plugin_config_values/administration/{plugin} break_glass':
    'the break_glass section editor is out of scope of R3.8b (issue 2614)',
  'POST /api/v2/admin/modes/administration': 'no console control creates a mode',
  'DELETE /api/v2/admin/modes/administration': 'no console control deletes a mode',
  'PUT /api/v2/admin/user_project_permissions/administration': 'no console control writes it',
  'POST /api/v2/admin/identity_providers/administration/{key}/rebind': 'no console control rebinds an identity',
};

/** The generated client functions of marked operations. */
const MARKED_OPERATIONS = [
  'resetBrandingSettings',
  'createExecutionSuspension',
  'revokeInventoryCredential',
  'bulkInviteMembers',
  'releaseLegalHold',
  'setPlatformRetentionPolicy',
  'setProjectRetentionOverride',
  'deleteProjectRetentionOverride',
] as const;

const REASON_HELPER_RE = /\b(reasonFields|reasonFieldsKeepText|reasonHeaders)\s*\(/;
const DIALOG_RE = /<AdminDangerousActionDialog\b|\buseAdminDangerousConfirm\s*\(/;
const KEY_RE = /['"`]((?:GET|POST|PUT|PATCH|DELETE) \/api\/[^'"`]*)['"`]/g;
const OWNER = 'adminDangerousAction.ts';

const SOURCE_RE = /\.(ts|tsx)$/;
const TEST_RE = /\.(test|spec|stories)\.(ts|tsx)$/;

function walk(dir: string, skip: ReadonlySet<string>): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return skip.has(entry.name) ? [] : walk(path, skip);
    return SOURCE_RE.test(entry.name) && !TEST_RE.test(entry.name) ? [path] : [];
  });
}

/** Source without comments, so a comment that names a key is not a use. */
function stripComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, ' ').replace(/(^|[^:'"`])\/\/.*$/gm, '$1');
}

/** Each top-level function or const of a module, with its text up to the next one. */
function declarations(source: string): Map<string, string> {
  const bodies = new Map<string, string>();
  const starts = [
    ...source.matchAll(/^(?:export\s+)?(?:(?:async\s+)?function\s+([A-Za-z0-9_]+)|const\s+([A-Za-z0-9_]+)\s*[=:])/gm),
  ];
  const boundaries = [...source.matchAll(/^(?:export\s|function\s|const\s|interface\s|type\s|class\s)/gm)].map(
    (match) => match.index,
  );
  for (const start of starts) {
    const name = start[1] ?? start[2];
    if (name === undefined) continue;
    const end = boundaries.find((index) => index > start.index) ?? source.length;
    bodies.set(name, source.slice(start.index, end));
  }
  return bodies;
}

/** The hooks of the API sources whose body sends a reason. */
function reasonHooks(apiSources: readonly string[]): Set<string> {
  const found = new Set<string>();
  for (const source of apiSources) {
    for (const [name, body] of declarations(stripComments(source))) {
      if (name.startsWith('use') && REASON_HELPER_RE.test(body)) found.add(name);
    }
  }
  return found;
}

/** The marked generated functions a declaration calls without a reason helper. */
function unmarkedCalls(apiSource: string): string[] {
  const found: string[] = [];
  for (const [name, body] of declarations(stripComments(apiSource))) {
    for (const operation of MARKED_OPERATIONS) {
      if (new RegExp(`\\b${operation}\\s*\\(`).test(body) && !REASON_HELPER_RE.test(body)) {
        found.push(`${name} → ${operation}`);
      }
    }
  }
  return found;
}

/** The relative imports of a source, resolved to files of `files`. */
function importsOf(path: string, source: string, files: ReadonlySet<string>): string[] {
  const found: string[] = [];
  for (const match of source.matchAll(/from\s+['"](\.{1,2}\/[^'"]+)['"]/g)) {
    const base = resolve(dirname(path), match[1] ?? '');
    for (const candidate of [base, `${base}.ts`, `${base}.tsx`]) {
      if (files.has(candidate)) found.push(candidate);
    }
  }
  return found;
}

interface Sources {
  readonly pages: ReadonlyMap<string, string>;
  readonly api: ReadonlyMap<string, string>;
}

function readSources(): Sources {
  const read = (paths: string[]) => new Map(paths.map((path) => [path, stripComments(readFileSync(path, 'utf8'))]));
  return {
    pages: read(walk(ADMIN_DIR, new Set(['api', '__tests__']))),
    api: read(walk(API_DIR, new Set())),
  };
}

/** The files that call a reason hook and reach no dialog through their importers. */
function undialogedCallers(pages: ReadonlyMap<string, string>, hooks: ReadonlySet<string>): string[] {
  const files = new Set(pages.keys());
  const importers = new Map<string, string[]>();
  for (const [path, source] of pages) {
    for (const imported of importsOf(path, source, files)) {
      importers.set(imported, [...(importers.get(imported) ?? []), path]);
    }
  }
  const reachesDialog = (start: string): boolean => {
    const seen = new Set<string>();
    const queue = [start];
    while (queue.length > 0) {
      const path = queue.shift() ?? '';
      if (seen.has(path)) continue;
      seen.add(path);
      if (DIALOG_RE.test(pages.get(path) ?? '')) return true;
      queue.push(...(importers.get(path) ?? []));
    }
    return false;
  };
  const names = [...hooks].join('|');
  const call = new RegExp(`\\b(${names})\\s*\\(`);
  return [...pages]
    .filter(([path, source]) => call.test(source) && !reachesDialog(path))
    .map(([path]) => relative(ADMIN_DIR, path));
}

describe('the dangerous-action guard', () => {
  const sources = readSources();
  const keys = new Set(Object.keys(registry.actions));
  const named = new Set(
    [...sources.pages.values(), ...sources.api.values()].flatMap((source) =>
      [...source.matchAll(KEY_RE)].map((match) => match[1] ?? ''),
    ),
  );

  it('reads a registry with actions and reason codes', () => {
    expect(keys.size).toBeGreaterThan(0);
    expect(registry.reason_codes).toContain('other');
  });

  it('names only keys of the server registry', () => {
    expect([...named].filter((key) => !keys.has(key))).toEqual([]);
  });

  it('asks for the reason of every marked action, or says why not', () => {
    const missing = [...keys].filter((key) => !named.has(key) && NOT_CONVERTED[key] === undefined);
    expect(missing).toEqual([]);
  });

  it('keeps no NOT_CONVERTED entry that a control now covers, or that is not in the registry', () => {
    const stale = Object.keys(NOT_CONVERTED).filter((key) => named.has(key) || !keys.has(key));
    expect(stale).toEqual([]);
  });

  it('sends a reason only from a file that shows the dialog', () => {
    const hooks = reasonHooks([...sources.api.values()]);
    expect(hooks.size).toBeGreaterThan(0);
    expect(undialogedCallers(sources.pages, hooks)).toEqual([]);
  });

  it('builds the reason in one module', () => {
    const builders = [...sources.pages, ...sources.api]
      .filter(([path, source]) => basename(path) !== OWNER && /\breason_code\b|Elitea-Reason/.test(source))
      .map(([path]) => relative(ADMIN_DIR, path));
    expect(builders).toEqual([]);
  });

  it('sends a reason with every call of a marked generated operation', () => {
    const found = [...sources.api.values()].flatMap(unmarkedCalls);
    expect(found).toEqual([]);
  });
});

describe('the guard on fixtures', () => {
  it('finds a marked operation called without a reason', () => {
    const api = `export function useReset() {\n  return useMutation({ mutationFn: () => resetBrandingSettings() });\n}\n`;
    expect(unmarkedCalls(api)).toEqual(['useReset → resetBrandingSettings']);
    const marked = `export function useReset() {\n  return useMutation({ mutationFn: (r) => resetBrandingSettings(reasonHeaders('K', r)) });\n}\n`;
    expect(unmarkedCalls(marked)).toEqual([]);
  });

  it('finds a reason hook called from a page that shows no dialog', () => {
    const hooks = reasonHooks([`export function useDrop() {\n  return reasonHeaders('K', r);\n}\n`]);
    const page = join(ADMIN_DIR, 'Fixture.tsx');
    const child = join(ADMIN_DIR, 'FixtureChild.ts');
    const shown = new Map([
      [page, `import { x } from './FixtureChild';\nconst d = <AdminDangerousActionDialog open />;`],
      [child, `const drop = useDrop();`],
    ]);
    expect(undialogedCallers(shown, hooks)).toEqual([]);
    const hidden = new Map([[child, `const drop = useDrop();`]]);
    expect(undialogedCallers(hidden, hooks)).toEqual(['FixtureChild.ts']);
  });
});
