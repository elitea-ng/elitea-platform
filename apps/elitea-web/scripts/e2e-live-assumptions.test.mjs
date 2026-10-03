/**
 * The decisions that let the Playwright suites target a deployed instance
 * (`scripts/lib/e2e-live-assumptions.mjs`).
 *
 * Two properties matter more than the rest, and neither is visible from inside
 * a journey:
 *
 *  1. Every default is the rig's. A rig run sets none of these variables, and
 *     if a default drifted every journey would quietly change its tenancy.
 *  2. A deployment-absence skip fires ONLY on a declared live target. On the
 *     rig the same absence is the defect, and a skip there would turn a broken
 *     feature into a green run.
 *
 * Runs in the `scripts` vitest project, like every other gate self-test here.
 */
import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import {
  CLIENT_DEFAULT_CHAT_LIMITS,
  LIVE_ADMIN_READONLY,
  LIVE_ENV_DEPENDENT,
  LIVE_SAFETY_EXCLUDED,
  NO_PROJECT,
  PLAYWRIGHT_BUFFER_LIMIT_BYTES,
  RIG_SEEDED_CHAT_LIMITS,
  adminPersonaScope,
  catalogueListsModel,
  discoveryAbsentAnswer,
  livePathPattern,
  liveSelectedSpecs,
  liveSharedStateViolations,
  liveTestIgnore,
  liveTraceMode,
  e2eTenancy,
  envValue,
  includesEnvDependent,
  isLiveTarget,
  liveRunRefusal,
  nonDiscriminatingOutcome,
  refusedForMissingCredential,
  refusedOnLiveTarget,
  rethrowSkipAfterCleanup,
  schemaFilledSettings,
  shouldSkipForDeployment,
  socketServerConfigured,
  toleratesCredentialOnlyCategories,
  uploadLimitPlan,
} from './lib/e2e-live-assumptions.mjs';

const LIVE = { E2E_TARGET: 'live' };

describe('envValue', () => {
  it('trims a set value', () => {
    expect(envValue({ X: '  8 ' }, 'X')).toBe('8');
  });

  it('reads blank and unset alike as absent', () => {
    expect(envValue({ X: '   ' }, 'X')).toBeUndefined();
    expect(envValue({ X: '' }, 'X')).toBeUndefined();
    expect(envValue({}, 'X')).toBeUndefined();
  });
});

describe('e2eTenancy', () => {
  it('is the rig when nothing is set', () => {
    expect(e2eTenancy({})).toEqual({
      projectId: '1',
      projectName: 'Default Project',
      publicProjectId: undefined,
      catalogueProjectId: undefined,
      publishAuthorProjectId: undefined,
    });
  });

  it('takes every value a live run declares', () => {
    expect(
      e2eTenancy({
        E2E_PROJECT_ID: '8',
        E2E_DEFAULT_PROJECT_NAME: 'regression-2026-10',
        E2E_PUBLIC_PROJECT_ID: '1',
        E2E_CATALOGUE_PROJECT_ID: '1',
        E2E_PUBLISH_AUTHOR_PROJECT_ID: NO_PROJECT,
      }),
    ).toEqual({
      projectId: '8',
      projectName: 'regression-2026-10',
      publicProjectId: '1',
      catalogueProjectId: '1',
      publishAuthorProjectId: 'none',
    });
  });
});

describe('the live-target gate', () => {
  it('recognises only the exact declaration', () => {
    expect(isLiveTarget(LIVE)).toBe(true);
    expect(isLiveTarget({ E2E_TARGET: 'LIVE' })).toBe(false);
    expect(isLiveTarget({})).toBe(false);
  });

  it('recognises the triage switch only as 1', () => {
    expect(includesEnvDependent({ LIVE_INCLUDE_ENV_DEPENDENT: '1' })).toBe(true);
    expect(includesEnvDependent({ LIVE_INCLUDE_ENV_DEPENDENT: 'true' })).toBe(false);
  });

  it('never skips on the rig, where an absent feature is the defect', () => {
    expect(shouldSkipForDeployment(true, {})).toBe(false);
  });

  it('skips on a live target only when the prerequisite is absent', () => {
    expect(shouldSkipForDeployment(true, LIVE)).toBe(true);
    expect(shouldSkipForDeployment(false, LIVE)).toBe(false);
  });

  it('does not skip while env-dependent cases are being triaged', () => {
    expect(shouldSkipForDeployment(true, { ...LIVE, LIVE_INCLUDE_ENV_DEPENDENT: '1' })).toBe(false);
  });

  it('refuses shared-state writers on every live run, triage included, and never on the rig', () => {
    expect(refusedOnLiveTarget(LIVE)).toBe(true);
    expect(refusedOnLiveTarget({ ...LIVE, LIVE_INCLUDE_ENV_DEPENDENT: '1' })).toBe(true);
    expect(refusedOnLiveTarget({})).toBe(false);
  });
});

describe('rethrowSkipAfterCleanup', () => {
  const skipped = new Error('skip signal');

  it('rethrows the skip after a cleanup that succeeds', async () => {
    let cleaned = false;
    await expect(
      rethrowSkipAfterCleanup(skipped, async () => {
        cleaned = true;
      }),
    ).rejects.toBe(skipped);
    expect(cleaned).toBe(true);
  });

  it('rethrows the SKIP, not the cleanup error, when the cleanup fails', async () => {
    await expect(
      rethrowSkipAfterCleanup(skipped, async () => {
        throw new Error('503 from a shared instance');
      }),
    ).rejects.toBe(skipped);
  });
});

describe('liveRunRefusal', () => {
  const ready = {
    PLAYWRIGHT_BASE_URL: 'https://elitea.example.test',
    E2E_STATE_DIR: '/tmp/state',
    E2E_PROJECT_ID: '8',
    E2E_DEFAULT_PROJECT_NAME: 'regression-2026-10',
    LIVE_ADMIN_PERSONA_SCOPE: 'project',
  };

  it('refuses a run that does not say what its admin persona may administer', () => {
    const { LIVE_ADMIN_PERSONA_SCOPE: _scope, ...undeclared } = ready;
    expect(liveRunRefusal(undeclared)).toMatch(/LIVE_ADMIN_PERSONA_SCOPE/);
    expect(liveRunRefusal({ ...ready, LIVE_ADMIN_PERSONA_SCOPE: 'yes' })).toMatch(/LIVE_ADMIN_PERSONA_SCOPE/);
    expect(liveRunRefusal({ ...ready, LIVE_ADMIN_PERSONA_SCOPE: 'platform' })).toBeUndefined();
  });

  it('lets a fully declared run start', () => {
    expect(liveRunRefusal(ready)).toBeUndefined();
  });

  it('refuses project 1, which holds real data', () => {
    expect(liveRunRefusal({ ...ready, E2E_PROJECT_ID: '1' })).toMatch(/never 1/);
  });

  it('names everything that is missing at once', () => {
    const refusal = liveRunRefusal({ PLAYWRIGHT_BASE_URL: 'http://localhost:8082' }) ?? '';
    expect(refusal).toContain('PLAYWRIGHT_BASE_URL (https)');
    expect(refusal).toContain('E2E_STATE_DIR');
    expect(refusal).toContain('E2E_PROJECT_ID');
    expect(refusal).toContain('E2E_DEFAULT_PROJECT_NAME');
    expect(refusal).toContain('LIVE_ADMIN_PERSONA_SCOPE');
  });

  it('treats an unset base URL as not https', () => {
    expect(liveRunRefusal({ ...ready, PLAYWRIGHT_BASE_URL: undefined })).toBe(
      'playwright.live.config.ts: set PLAYWRIGHT_BASE_URL (https)',
    );
  });
});

describe('adminPersonaScope and liveTraceMode', () => {
  it('reads only the two declared scopes', () => {
    expect(adminPersonaScope({ LIVE_ADMIN_PERSONA_SCOPE: ' project ' })).toBe('project');
    expect(adminPersonaScope({ LIVE_ADMIN_PERSONA_SCOPE: 'platform' })).toBe('platform');
    expect(adminPersonaScope({ LIVE_ADMIN_PERSONA_SCOPE: 'admin' })).toBeUndefined();
    expect(adminPersonaScope({})).toBeUndefined();
  });

  it('keeps traces (which hold live session cookies) only on the explicit opt-in', () => {
    expect(liveTraceMode({})).toBe('off');
    expect(liveTraceMode({ LIVE_TRACE_WITH_SESSION_COOKIES: 'true' })).toBe('off');
    expect(liveTraceMode({ LIVE_TRACE_WITH_SESSION_COOKIES: '1' })).toBe('retain-on-failure');
  });
});

describe('uploadLimitPlan', () => {
  const MIB = 1024 * 1024;
  const rig = { ...RIG_SEEDED_CHAT_LIMITS };
  const withLimit = (mb) => uploadLimitPlan({ ...rig, chat_max_file_upload_size_mb: mb });

  it('seeds every key away from the reader defaults, so a defaults-only body cannot pass on the rig', () => {
    for (const key of Object.keys(CLIENT_DEFAULT_CHAT_LIMITS)) {
      expect(RIG_SEEDED_CHAT_LIMITS[key], key).not.toBe(CLIENT_DEFAULT_CHAT_LIMITS[key]);
    }
  });

  it('derives both uploads from the served limit (the rig seeds 1 MB)', () => {
    expect(uploadLimitPlan(rig)).toEqual({
      ok: true,
      discriminating: true,
      limitMb: 1,
      oversizedBytes: 2 * MIB,
      oversizedViaFile: false,
      acceptedBytes: 512 * 1024,
    });
  });

  it('keeps the accepted file small however generous the served limit is', () => {
    // The accepted upload is at most half the limit and never more than
    // 512 KiB, so a large limit does not turn step 3 into a large upload.
    expect(withLimit(40)).toMatchObject({ discriminating: true, oversizedBytes: 41 * MIB, acceptedBytes: 512 * 1024 });
  });

  it("hands an oversized file at Playwright's 50 MiB buffer cap to disk", () => {
    expect(PLAYWRIGHT_BUFFER_LIMIT_BYTES).toBe(50 * MIB);
    expect(withLimit(48)).toMatchObject({ discriminating: true, oversizedBytes: 49 * MIB, oversizedViaFile: false });
    // 50 MiB exactly is refused as a buffer (`>=`).
    expect(withLimit(49)).toMatchObject({ discriminating: true, oversizedBytes: 50 * MIB, oversizedViaFile: true });
    expect(withLimit(50)).toMatchObject({ discriminating: true, oversizedBytes: 51 * MIB, oversizedViaFile: true });
  });

  it('still discriminates at 149 MB: the 150 MiB file is one the fallback would accept', () => {
    // The client refuses `size > limit`, so 150 MiB passes a 150 MB fallback.
    expect(withLimit(149)).toMatchObject({ discriminating: true, oversizedBytes: 150 * MIB, oversizedViaFile: true });
  });

  it('says a deployment on the client defaults cannot discriminate (live: 150 MB)', () => {
    const plan = uploadLimitPlan({ ...CLIENT_DEFAULT_CHAT_LIMITS });
    expect(plan.ok).toBe(true);
    expect(plan).toMatchObject({ discriminating: false, limitMb: 150 });
    expect(plan.reason).toMatch(/client's own fallback/);
  });

  it('says a limit ABOVE the fallback cannot discriminate either — the fallback refuses the file too', () => {
    for (const mb of [151, 200]) {
      const plan = withLimit(mb);
      expect(plan).toMatchObject({ ok: true, discriminating: false, limitMb: mb });
      expect(plan.reason).toMatch(/above the client's 150 MB fallback/);
    }
  });

  it('refuses a body missing a key, or carrying a non-positive or non-integer one', () => {
    const { chat_max_upload_count: _dropped, ...missing } = rig;
    expect(uploadLimitPlan(missing)).toEqual({
      ok: false,
      reason: 'chat_config served no positive integer for chat_max_upload_count',
    });
    expect(uploadLimitPlan({ ...rig, chat_max_file_upload_size_mb: 0 }).ok).toBe(false);
    expect(uploadLimitPlan({ ...rig, chat_max_file_upload_size_mb: 1.5 }).ok).toBe(false);
    expect(uploadLimitPlan(null).ok).toBe(false);
  });
});

describe('nonDiscriminatingOutcome', () => {
  it('fails on the rig, where a non-discriminating limit means the seed or the fallback broke', () => {
    expect(nonDiscriminatingOutcome({})).toBe('fail');
  });

  it('skips only on a live target outside triage', () => {
    expect(nonDiscriminatingOutcome(LIVE)).toBe('skip');
    expect(nonDiscriminatingOutcome({ ...LIVE, LIVE_INCLUDE_ENV_DEPENDENT: '1' })).toBe('fail');
  });
});

describe('toleratesCredentialOnlyCategories', () => {
  it('never on the rig or in triage, where every representative must be created', () => {
    expect(toleratesCredentialOnlyCategories({})).toBe(false);
    expect(toleratesCredentialOnlyCategories({ ...LIVE, LIVE_INCLUDE_ENV_DEPENDENT: '1' })).toBe(false);
  });

  it('on a live target', () => {
    expect(toleratesCredentialOnlyCategories(LIVE)).toBe(true);
  });
});

describe('discoveryAbsentAnswer', () => {
  it('reads only the not-composed 503 as absent', () => {
    expect(discoveryAbsentAnswer(503, '{"error":"toolkit discovery unavailable"}')).toBe(true);
  });

  it('does not read the settings-resolution 503 (a runtime fault) as absent', () => {
    expect(discoveryAbsentAnswer(503, '{"ok":false,"error":"toolkit settings could not be resolved"}')).toBe(false);
  });

  it('does not read any other status or an unparsable body as absent', () => {
    expect(discoveryAbsentAnswer(200, '{"error":"toolkit discovery unavailable"}')).toBe(false);
    expect(discoveryAbsentAnswer(503, '<html>bad gateway</html>')).toBe(false);
    expect(discoveryAbsentAnswer(503, undefined)).toBe(false);
  });
});

describe('catalogueListsModel', () => {
  const body = JSON.stringify({ items: [{ name: 'a' }, { data: { name: 'e2e-embed' } }] });

  it('finds the model by row name or data.name', () => {
    expect(catalogueListsModel(200, body, 'e2e-embed')).toBe(true);
    expect(catalogueListsModel(200, body, 'a')).toBe(true);
  });

  it('reports a served catalogue without the model as not listed', () => {
    expect(catalogueListsModel(200, body, 'missing')).toBe(false);
    expect(catalogueListsModel(200, '{}', 'missing')).toBe(false);
  });

  it('throws on a 403 or 500 instead of reading the fault as "not listed"', () => {
    expect(() => catalogueListsModel(403, '{"error":"forbidden"}', 'e2e-embed')).toThrow(/failed \(403\)/);
    expect(() => catalogueListsModel(500, 'boom', 'e2e-embed')).toThrow(/failed \(500\)/);
    expect(() => catalogueListsModel(200, 'not json', 'e2e-embed')).toThrow(/non-JSON/);
  });
});

describe('schemaFilledSettings', () => {
  it('fills each fillable required key the way the form is typed', () => {
    expect(schemaFilledSettings(['repository', 'project'])).toEqual({
      selected_tools: [],
      repository: 'autotest-repository',
      project: 'autotest-project',
    });
  });

  it('still sends an empty tool selection for a type with nothing to fill', () => {
    expect(schemaFilledSettings([])).toEqual({ selected_tools: [] });
  });
});

describe('refusedForMissingCredential', () => {
  const github = '{"error":"settings.github_configuration is required for github toolkit"}';

  it('recognises a refusal that names a credential the type requires', () => {
    expect(refusedForMissingCredential(400, github, ['github_configuration'])).toBe(true);
  });

  it('does not excuse a refusal about a field this journey sent', () => {
    expect(
      refusedForMissingCredential(
        400,
        '{"error":"settings.repository is required for github toolkit"}',
        ['github_configuration'],
      ),
    ).toBe(false);
  });

  it('does not excuse an error that lists a credential beside a field this journey sent', () => {
    expect(
      refusedForMissingCredential(
        400,
        '{"error":"settings.repository is required; settings.github_configuration is required for github toolkit"}',
        ['github_configuration'],
      ),
    ).toBe(false);
  });

  it('does not excuse an error that merely mentions the credential key', () => {
    expect(
      refusedForMissingCredential(400, '{"error":"github_configuration must reference a saved credential"}', [
        'github_configuration',
      ]),
    ).toBe(false);
    expect(refusedForMissingCredential(400, 'settings.github_configuration is required for github toolkit', [
      'github_configuration',
    ])).toBe(false);
  });

  it('does not excuse a non-400, a non-text body, or an empty key', () => {
    expect(refusedForMissingCredential(500, github, ['github_configuration'])).toBe(false);
    expect(refusedForMissingCredential(400, undefined, ['github_configuration'])).toBe(false);
    expect(refusedForMissingCredential(400, github, [''])).toBe(false);
  });
});

describe('socketServerConfigured', () => {
  it('is off on the rig, which sets the variable empty', () => {
    expect(socketServerConfigured({ VITE_SOCKET_SERVER: '' }, { vite_socket_server: '' })).toBe(false);
    expect(socketServerConfigured({}, undefined)).toBe(false);
    expect(socketServerConfigured({}, null)).toBe(false);
  });

  it('is on when the runner declares it', () => {
    expect(socketServerConfigured({ VITE_SOCKET_SERVER: 'https://socket.example.test' }, undefined)).toBe(true);
  });

  it('is on when the page serves it, in either casing', () => {
    expect(socketServerConfigured({}, { vite_socket_server: 'https://s.example.test' })).toBe(true);
    expect(socketServerConfigured({}, { VITE_SOCKET_SERVER: 'https://s.example.test' })).toBe(true);
  });

  it('ignores a non-string or blank served value', () => {
    expect(socketServerConfigured({}, { vite_socket_server: 1 })).toBe(false);
    expect(socketServerConfigured({}, { vite_socket_server: '  ' })).toBe(false);
  });
});

/* ── the live selection ──────────────────────────────────────────────────── */

const E2E_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', 'e2e');

/** Every spec under `e2e/`, as an `e2e/`-relative path. */
function allSpecs(dir = E2E_DIR, prefix = '') {
  const found = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    const relative = prefix === '' ? name : `${prefix}/${name}`;
    if (statSync(path).isDirectory()) found.push(...allSpecs(path, relative));
    else if (name.endsWith('.spec.ts')) found.push(relative);
  }
  return found;
}

describe('livePathPattern', () => {
  it('matches a spec file exactly at the end of the path', () => {
    const pattern = livePathPattern('journeys/shell/shell.session.spec.ts');
    expect(pattern.test('/abs/e2e/journeys/shell/shell.session.spec.ts')).toBe(true);
    expect(pattern.test('/abs/e2e/journeys/shell/shellXsession.spec.ts')).toBe(false);
    expect(pattern.test('/abs/e2e/journeys/shell/shell.session.spec.ts.bak')).toBe(false);
  });

  it('matches every spec under a directory entry', () => {
    const pattern = livePathPattern('journeys/support/');
    expect(pattern.test('/abs/e2e/journeys/support/support.widget.spec.ts')).toBe(true);
    expect(pattern.test('/abs/e2e/journeys/supporting/x.spec.ts')).toBe(false);
  });

  it('matches every spec sharing a file-name prefix', () => {
    const pattern = livePathPattern('journeys/api/api.publish-*');
    expect(pattern.test('/abs/e2e/journeys/api/api.publish-lifecycle.spec.ts')).toBe(true);
    expect(pattern.test('/abs/e2e/journeys/api/api.published.spec.ts')).toBe(false);
  });
});

describe('liveTestIgnore', () => {
  it('always ignores the safety list and, outside triage, the env-dependent list', () => {
    const ignored = liveTestIgnore(LIVE);
    expect(ignored).toHaveLength(LIVE_SAFETY_EXCLUDED.length + LIVE_ENV_DEPENDENT.length);
    expect(ignored.some((p) => p.test('/e2e/journeys/toolkits/toolkits.aha.spec.ts'))).toBe(true);
    expect(ignored.some((p) => p.test('/e2e/journeys/deepwiki/deepwiki.route.spec.ts'))).toBe(true);
    expect(ignored.some((p) => p.test('/e2e/journeys/inventory/inventory.graph.spec.ts'))).toBe(true);
  });

  it('re-admits only the env-dependent list for triage, never the safety list', () => {
    const ignored = liveTestIgnore({ ...LIVE, LIVE_INCLUDE_ENV_DEPENDENT: '1' });
    expect(ignored).toHaveLength(LIVE_SAFETY_EXCLUDED.length);
    expect(ignored.some((p) => p.test('/e2e/journeys/toolkits/toolkits.aha.spec.ts'))).toBe(false);
    expect(ignored.some((p) => p.test('/e2e/journeys/shell/shell.session.spec.ts'))).toBe(true);
  });

  /*
   * The lists are paths, and a path that no longer exists still "matches
   * nothing" quietly — a safety exclusion for a renamed platform-flag writer
   * would stop excluding it with every run still green. Each entry must name
   * something on disk, and a directory or prefix entry must still match a spec.
   */
  it('names only specs that exist', () => {
    const specs = allSpecs();
    expect(specs.length).toBeGreaterThan(100);
    for (const entry of [...LIVE_SAFETY_EXCLUDED, ...LIVE_ENV_DEPENDENT, ...LIVE_ADMIN_READONLY]) {
      if (entry.endsWith('/') || entry.endsWith('*')) {
        const pattern = livePathPattern(entry);
        expect(specs.some((spec) => pattern.test(spec)), `${entry} matches no spec`).toBe(true);
      } else {
        expect(existsSync(join(E2E_DIR, entry)), `${entry} does not exist`).toBe(true);
      }
    }
  });
});

/*
 * The other direction: what the live lanes DO select must not write shared
 * state. A list of exclusions only covers the files someone thought of — the
 * catalogue-publishing journeys joined the live run that way, and every real
 * user of the instance saw autotest_ agents in its Catalog. So the selection is
 * computed exactly as the config builds it, in its widest form (triage, which
 * re-admits the env-dependent list, plus the platform-scope admin lane and the
 * streaming lane), and each selected file is scanned.
 */
describe('liveSharedStateViolations', () => {
  const unguarded = [
    "test.describe('d', () => {",
    "  test('publishes', async ({ request }) => {",
    '    await request.post(`${API_BASE}/elitea_core/publish/prompt_lib/${P}/${V}`, { data: {} });',
    '  });',
    '});',
  ].join('\n');

  it('flags a publish in an unguarded case', () => {
    expect(liveSharedStateViolations(unguarded)).toEqual([
      { line: 3, marker: 'catalogue publish', reason: 'in an unguarded case' },
    ]);
  });

  it("accepts the case's own guard, the describe's beforeEach guard, or a live-safe note", () => {
    const inCase = unguarded.replace("async ({ request }) => {", "async ({ request }) => {\n    neverOnLiveTarget('x');");
    expect(liveSharedStateViolations(inCase)).toEqual([]);
    const inDescribe = unguarded.replace(
      "test.describe('d', () => {",
      "test.describe('d', () => {\n  test.beforeEach(() => {\n    neverOnLiveTarget('x');\n  });",
    );
    expect(liveSharedStateViolations(inDescribe)).toEqual([]);
    const noted = unguarded.replace("async ({ request }) => {", 'async ({ request }) => {\n    // live-safe: why');
    expect(liveSharedStateViolations(noted)).toEqual([]);
  });

  it('does not let a guard in one case cover its sibling', () => {
    const siblings = unguarded.replace(
      "test.describe('d', () => {",
      "test.describe('d', () => {\n  test('guarded', () => {\n    neverOnLiveTarget('x');\n  });",
    );
    expect(liveSharedStateViolations(siblings)).toHaveLength(1);
  });

  it('flags a writer in a shared helper outside any test — that file must be excluded whole', () => {
    const helper = 'async function publish(r) {\n  await r.post(`/x/publish/prompt_lib/1/2`);\n}\n';
    expect(liveSharedStateViolations(helper)).toEqual([
      { line: 2, marker: 'catalogue publish', reason: 'outside any test (a shared helper)' },
    ]);
  });

  it('ignores markers in comments and administration-mode READS', () => {
    const benign = [
      '/** e2e-member@autotest.local is the persona; POST /publish/prompt_lib/ is covered elsewhere */',
      "test('reads', async ({ request }) => {",
      '  // await request.post(`/logout`)',
      '  await request.get(`${API_BASE}/admin/users/mode/administration`);',
      "  await page.goto('https://example.test/x');",
      '});',
    ].join('\n');
    expect(liveSharedStateViolations(benign)).toEqual([]);
    const write = benign.replace('request.get(', 'request.post(');
    expect(liveSharedStateViolations(write)).toHaveLength(1);
  });

  it('flags every other writer shape', () => {
    for (const line of [
      "  await withPlatformFlag(request, 'x', true, async () => {});",
      "  await page.getByTestId('agent-publish-menuitem').click();",
      "  await request.post(`${API_BASE}/project_users/${P}`, { data: { emails: ['x@autotest.local'] } });",
      "  await page.goto(BASE_URL + '/app/settings/users?inviteUsers=1');",
      '  await request.post(`${API_BASE}/auth/logout`);',
    ]) {
      expect(liveSharedStateViolations(`test('t', async () => {\n${line}\n});`), line).toHaveLength(1);
    }
  });

  it('lets a persona e-mail that is only LOOKED UP through (env-dependent, not unsafe)', () => {
    const lookup = [
      "const MEMBER_EMAIL = 'e2e-member@autotest.local';",
      '',
      '',
      '',
      "test('t', async ({ request }) => {",
      '  const id = await resolveUserId(request, P, MEMBER_EMAIL);',
      '});',
    ].join('\n');
    expect(liveSharedStateViolations(lookup)).toEqual([]);
  });

  // The regression fixture: agents.publishing as it stood when a live run
  // published into the real Catalog — the same file with its guards removed.
  it('catches agents.publishing.spec.ts without its live guards', () => {
    const source = readFileSync(join(E2E_DIR, 'journeys/agents/agents.publishing.spec.ts'), 'utf8');
    const stripped = source.replace(/^\s*neverOnLiveTarget\([^;]*\);\n/gm, '');
    expect(stripped).not.toBe(source);
    expect(liveSharedStateViolations(stripped).length).toBeGreaterThan(0);
  });
});

describe('the live selection writes no shared state', () => {
  const widest = { ...LIVE, LIVE_INCLUDE_ENV_DEPENDENT: '1', LIVE_ADMIN_PERSONA_SCOPE: 'platform' };

  it('computes the selection the way the config does', () => {
    const selected = liveSelectedSpecs(allSpecs(), widest);
    expect(selected.length).toBeGreaterThan(100);
    // Safety exclusions stay out even in triage; the env list comes back.
    expect(selected).not.toContain('journeys/shell/shell.session.spec.ts');
    expect(selected).not.toContain('journeys/agents/agents.hub-discovery.spec.ts');
    expect(selected).toContain('journeys/toolkits/toolkits.aha.spec.ts');
    expect(selected).toContain('journeys/admin/admin.navigation.spec.ts');
    expect(selected).toContain('streaming/index.lifecycle.spec.ts');
    expect(selected).not.toContain('streaming/chat.admin-providers.spec.ts');
    // A project-scoped admin persona does not get the admin-readonly lane.
    expect(liveSelectedSpecs(allSpecs(), { ...widest, LIVE_ADMIN_PERSONA_SCOPE: 'project' })).not.toContain(
      'journeys/admin/admin.navigation.spec.ts',
    );
  });

  it('selects no spec that reaches shared state outside a live-guarded case', () => {
    const offenders = [];
    for (const spec of liveSelectedSpecs(allSpecs(), widest)) {
      const violations = liveSharedStateViolations(readFileSync(join(E2E_DIR, spec), 'utf8'));
      for (const v of violations) offenders.push(`${spec}:${v.line} ${v.marker} (${v.reason})`);
    }
    expect(offenders, 'add the file to LIVE_SAFETY_EXCLUDED or guard the case with neverOnLiveTarget').toEqual([]);
  });
});
