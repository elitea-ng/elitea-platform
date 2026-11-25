// @vitest-environment node
/**
 * The mutation guard of the read-only primitive (ADR-0059 decision 6; R3.4a,
 * issue 2534): an admin page runs every mutation through the read-only
 * wrapper's guard, `useAdminWritable(itemId).guard(...)`, so at `read` no
 * write leaves the page whatever control reached it.
 *
 * A source test, not an oxlint plugin (the pattern of
 * `src/app/router.lazy-guard.test.ts`). It reads the admin API modules for
 * their mutation hooks (a hook whose body calls `useMutation`, directly or
 * through another hook of the same modules; `function useX` or
 * `const useX =`), then every admin page source
 * outside `api/` and the tests, and finds each call of one of those hooks or
 * of `useMutation` itself. A call is wrapped when it is the argument of a
 * `.guard(` call: `access.guard(useSaveProjectBudget())`.
 *
 * Every admin page is converted (R3.4b, issue 2613), so there is no
 * allowlist: any unwrapped mutation fails. The one exception is a short list
 * of mutation hooks that READ (`READ_MUTATIONS`).
 */
import { readFileSync, readdirSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

const ADMIN_DIR = fileURLToPath(new URL('..', import.meta.url));
const API_DIR = join(ADMIN_DIR, 'api');
const GENERATED_DIR = fileURLToPath(new URL('../../../shared/api/generated', import.meta.url));

/**
 * Mutation hooks that READ: a GET the API module carries as a mutation so
 * its result never enters the query cache. They work at the read level, so
 * they are not guarded. Each entry says what it reads; never add a write.
 */
const READ_MUTATIONS: ReadonlySet<string> = new Set([
  // GET of one global secret's plaintext, gated on configuration.secrets.secret.view.
  'useRevealAdminSecret',
  // GET of the branding package, the browser's save-as.
  'useDownloadBrandingPackage',
]);

const SOURCE_RE = /\.(ts|tsx)$/;
const TEST_RE = /\.(test|spec|stories)\.(ts|tsx)$/;

function walk(dir: string, skip: ReadonlySet<string>): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return skip.has(entry.name) ? [] : walk(path, skip);
    return SOURCE_RE.test(entry.name) && !TEST_RE.test(entry.name) ? [path] : [];
  });
}

/** Source without comments, so a comment that names a hook is not a call. */
function stripComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, ' ').replace(/(^|[^:'"`])\/\/.*$/gm, '$1');
}

/**
 * Each hook of a module, `function useX` or `const useX =`, with the text up
 * to the next top-level declaration.
 */
function hookBodies(source: string): Map<string, string> {
  const bodies = new Map<string, string>();
  const starts = [
    ...source.matchAll(/^(?:export\s+)?(?:(?:async\s+)?function\s+(use[A-Za-z0-9_]*)|const\s+(use[A-Za-z0-9_]*)\s*[=:])/gm),
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

/**
 * The mutation hooks of the given API module sources: a hook is one when its
 * body calls `useMutation(` or another mutation hook, to a fixed point.
 */
function mutationHooks(apiSources: readonly string[]): Set<string> {
  const bodies = new Map<string, string>();
  for (const source of apiSources) {
    for (const [name, body] of hookBodies(stripComments(source))) bodies.set(name, body);
  }
  const found = new Set<string>(['useMutation']);
  let grew = true;
  while (grew) {
    grew = false;
    for (const [name, body] of bodies) {
      if (found.has(name)) continue;
      const calls = [...found].some((hook) => new RegExp(`\\b${hook}\\s*\\(`).test(body));
      if (calls) {
        found.add(name);
        grew = true;
      }
    }
  }
  return found;
}

/** The hooks a page source calls outside `.guard(`, in source order. */
function unwrappedMutations(pageSource: string, hooks: ReadonlySet<string>): string[] {
  const source = stripComments(pageSource);
  const names = [...hooks].join('|');
  const call = new RegExp(`(\\.guard\\(\\s*)?\\b(${names})\\s*(?:<[^>()]*>)?\\(`, 'g');
  const found: string[] = [];
  for (const match of source.matchAll(call)) {
    const name = match[2];
    // A declaration (`function useX(`) is not a call.
    const before = source.slice(Math.max(0, match.index - 9), match.index);
    if (match[1] === undefined && name !== undefined && !/function\s*$/.test(before)) found.push(name);
  }
  return found;
}

function apiSources(): string[] {
  return walk(API_DIR, new Set()).map((path) => readFileSync(path, 'utf8'));
}

function pageFindings(hooks: ReadonlySet<string>): Map<string, string[]> {
  const findings = new Map<string, string[]>();
  for (const path of walk(ADMIN_DIR, new Set(['api', '__tests__']))) {
    const calls = unwrappedMutations(readFileSync(path, 'utf8'), hooks).filter((name) => !READ_MUTATIONS.has(name));
    if (calls.length > 0) findings.set(relative(ADMIN_DIR, path), calls);
  }
  return findings;
}

const FIXTURE_API = `
import { useMutation } from '@tanstack/react-query';
export function useSaveThing() {
  return useMutation({ mutationFn: save });
}
function useInnerSave() {
  return useSaveThing();
}
export function useSaveTwice() {
  return useInnerSave();
}
export function useThings() {
  return useQuery({ queryKey: ['things'] });
}
export const useDeleteThing = () => useMutation({ mutationFn: remove });
`;

const FIXTURE_UNWRAPPED_PAGE = `
import { useSaveThing, useThings } from './api/thingsApi';
// useSaveThing() in a comment is not a call.
export function ThingsPage() {
  const things = useThings();
  const save = useSaveThing();
  return null;
}
`;

const FIXTURE_WRAPPED_PAGE = `
import { useAdminWritable } from './adminWriteAccess';
import { useSaveThing, useSaveTwice } from './api/thingsApi';
export function ThingsPage() {
  const access = useAdminWritable('things');
  const save = access.guard(useSaveThing());
  const twice = access.guard(
    useSaveTwice(),
  );
  return null;
}
`;

describe('the mutation guard', () => {
  const fixtureHooks = mutationHooks([FIXTURE_API]);

  it('finds the mutation hooks of an API module, through wrapping hooks too', () => {
    expect(fixtureHooks).toEqual(
      new Set(['useMutation', 'useSaveThing', 'useInnerSave', 'useSaveTwice', 'useDeleteThing']),
    );
  });

  it('fails on a fixture page with an unwrapped mutation', () => {
    expect(unwrappedMutations(FIXTURE_UNWRAPPED_PAGE, fixtureHooks)).toEqual(['useSaveThing']);
    expect(unwrappedMutations('const remove = useDeleteThing();', fixtureHooks)).toEqual(['useDeleteThing']);
  });

  it('passes a fixture page whose mutations go through guard', () => {
    expect(unwrappedMutations(FIXTURE_WRAPPED_PAGE, fixtureHooks)).toEqual([]);
  });

  it('counts a direct useMutation in a page as a mutation', () => {
    const page = `const mutation = useMutation({ mutationFn: save });\nconst ok = access.guard(useMutation({ mutationFn: save }));`;
    expect(unwrappedMutations(page, fixtureHooks)).toEqual(['useMutation']);
  });

  // The scan reads `pages/admin/api` only. That is complete while the
  // generated client has no mutation hook: its writes are plain functions,
  // which a page reaches through `useMutation` (caught above). When orval
  // starts generating mutation hooks, this fails and the scan must read them.
  it('the generated client has no mutation hook a page could call past the scan', () => {
    const generated = walk(GENERATED_DIR, new Set()).filter((path) => !/\.(msw|zod)\.ts$/.test(path));
    expect(generated.length).toBeGreaterThan(20);
    const withMutation = generated.filter((path) => /\buseMutation\b/.test(readFileSync(path, 'utf8')));
    expect(withMutation.map((path) => relative(GENERATED_DIR, path))).toEqual([]);
  });

  it('reads the real API modules, so a renamed directory cannot pass by finding nothing', () => {
    const hooks = mutationHooks(apiSources());
    expect(hooks.has('useSaveProjectBudget')).toBe(true);
    expect(hooks.has('useProjectBudgets')).toBe(false);
    expect(hooks.size).toBeGreaterThan(50);
  });
});

describe('admin pages run their mutations through the read-only guard', () => {
  const findings = pageFindings(mutationHooks(apiSources()));

  it('no page calls a mutation hook outside guard', () => {
    expect(Object.fromEntries(findings)).toEqual({});
  });

  it('names only real mutation hooks as reads, so a stale entry cannot linger', () => {
    const hooks = mutationHooks(apiSources());
    expect([...READ_MUTATIONS].filter((name) => !hooks.has(name))).toEqual([]);
  });
});
