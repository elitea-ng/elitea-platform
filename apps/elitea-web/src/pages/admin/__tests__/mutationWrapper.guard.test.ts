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
 * through another hook of the same modules), then every admin page source
 * outside `api/` and the tests, and finds each call of one of those hooks or
 * of `useMutation` itself. A call is wrapped when it is the argument of a
 * `.guard(` call: `access.guard(useSaveProjectBudget())`.
 *
 * The pages not converted yet are a checked-in allowlist. It can only shrink:
 * a page that has an unwrapped mutation and is not on the list fails, and a
 * page on the list that no longer has one fails too, so the list is cut in
 * the change that converts the page (R3.4b, issue 2613, empties it).
 */
import { readFileSync, readdirSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

const ADMIN_DIR = fileURLToPath(new URL('..', import.meta.url));
const API_DIR = join(ADMIN_DIR, 'api');

/**
 * The admin pages with a mutation outside the guard, relative to
 * `src/pages/admin`. Remove a line when its page is converted; never add one.
 */
const NOT_CONVERTED: readonly string[] = [
  'AdminBulkInviteDialog.tsx',
  'AdminEmailEditor.tsx',
  'AdminIdentityProvidersEditor.tsx',
  'AdminLlmProxyEditor.tsx',
  'AdminMcpServersEditor.tsx',
  'AdminMicrosoftAppsEditor.tsx',
  'AdminNativeClientsEditor.tsx',
  'AdminProjectCloudExecutorDialog.tsx',
  'AdminScimClientsEditor.tsx',
  'AdminScimGroupBindingsEditor.tsx',
  'AdminUserDevicesDrawer.tsx',
  'ConnectorRequests.tsx',
  'CredentialRevokeDialog.tsx',
  'DelegationReview.tsx',
  'GovernanceGuardrailPresetDialog.tsx',
  'GovernanceRoutingFields.tsx',
  'KillSwitch.tsx',
  'LlmProxyAlertsPanel.tsx',
  'LlmProxyProvidersPanel.tsx',
  'LlmProxyToolPricesPanel.tsx',
  'PermissionTrimming.tsx',
  'PlatformDefaultModelCard.tsx',
  'PlatformModelsPanel.tsx',
  'ProjectMemberDialog.tsx',
  'RetentionLegalHolds.tsx',
  'RetentionPolicies.tsx',
  'ServiceDescriptorDecisionDialog.tsx',
  'ToolkitTypeBulkDialog.tsx',
  'ToolkitTypeDecisionDialog.tsx',
  'ToolkitTypeProjectsDialog.tsx',
  'useAdminAppRequestsPage.ts',
  'useAdminBrandingPage.ts',
  'useAdminConfigurationPage.ts',
  'useAdminProjectProvisioning.ts',
  'useAdminProjectsPage.ts',
  'useAdminRoleDefinitions.ts',
  'useAdminRolesPage.ts',
  'useAdminSchedulesPage.ts',
  'useAdminSecretsPage.ts',
  'useAdminTasksPage.ts',
  'useAdminUsersPage.ts',
  'useAdoptProviderModels.ts',
  'useBrandingPackage.ts',
  'useGatewayGovernancePage.ts',
];

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

/** Each `function useX` of a module, with the text up to the next top-level declaration. */
function hookBodies(source: string): Map<string, string> {
  const bodies = new Map<string, string>();
  const starts = [...source.matchAll(/^(?:export\s+)?(?:async\s+)?function\s+(use[A-Za-z0-9_]*)/gm)];
  const boundaries = [...source.matchAll(/^(?:export\s|function\s|const\s|interface\s|type\s|class\s)/gm)].map(
    (match) => match.index,
  );
  for (const start of starts) {
    const name = start[1];
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
    const calls = unwrappedMutations(readFileSync(path, 'utf8'), hooks);
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
    expect(fixtureHooks).toEqual(new Set(['useMutation', 'useSaveThing', 'useInnerSave', 'useSaveTwice']));
  });

  it('fails on a fixture page with an unwrapped mutation', () => {
    expect(unwrappedMutations(FIXTURE_UNWRAPPED_PAGE, fixtureHooks)).toEqual(['useSaveThing']);
  });

  it('passes a fixture page whose mutations go through guard', () => {
    expect(unwrappedMutations(FIXTURE_WRAPPED_PAGE, fixtureHooks)).toEqual([]);
  });

  it('counts a direct useMutation in a page as a mutation', () => {
    const page = `const mutation = useMutation({ mutationFn: save });\nconst ok = access.guard(useMutation({ mutationFn: save }));`;
    expect(unwrappedMutations(page, fixtureHooks)).toEqual(['useMutation']);
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

  it('no page outside the allowlist calls a mutation hook outside guard', () => {
    const offending = [...findings.keys()].filter((path) => !NOT_CONVERTED.includes(path));
    expect(offending).toEqual([]);
  });

  it('every allowlisted page still has an unwrapped mutation, so the list only shrinks', () => {
    const converted = NOT_CONVERTED.filter((path) => !findings.has(path));
    expect(converted).toEqual([]);
  });

  it('keeps the allowlist sorted and free of duplicates', () => {
    expect([...NOT_CONVERTED].sort()).toEqual(NOT_CONVERTED);
    expect(new Set(NOT_CONVERTED).size).toBe(NOT_CONVERTED.length);
  });

  it('Budgets, the first converted page, is not on the list', () => {
    expect(NOT_CONVERTED.some((path) => /budget/i.test(path))).toBe(false);
    expect(findings.has('useAdminBudgetsPage.ts')).toBe(false);
  });
});
