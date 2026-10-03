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
import { existsSync, readdirSync, statSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import {
  CLIENT_DEFAULT_CHAT_LIMITS,
  LIVE_ADMIN_READONLY,
  LIVE_ENV_DEPENDENT,
  LIVE_SAFETY_EXCLUDED,
  NO_PROJECT,
  livePathPattern,
  liveTestIgnore,
  e2eTenancy,
  envValue,
  includesEnvDependent,
  isLiveTarget,
  liveRunRefusal,
  refusedForMissingCredential,
  schemaFilledSettings,
  shouldSkipForDeployment,
  socketServerConfigured,
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
});

describe('liveRunRefusal', () => {
  const ready = {
    PLAYWRIGHT_BASE_URL: 'https://elitea.example.test',
    E2E_STATE_DIR: '/tmp/state',
    E2E_PROJECT_ID: '8',
    E2E_DEFAULT_PROJECT_NAME: 'regression-2026-10',
  };

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
  });

  it('treats an unset base URL as not https', () => {
    expect(liveRunRefusal({ ...ready, PLAYWRIGHT_BASE_URL: undefined })).toBe(
      'playwright.live.config.ts: set PLAYWRIGHT_BASE_URL (https)',
    );
  });
});

describe('uploadLimitPlan', () => {
  const rig = {
    chat_max_upload_count: 4,
    chat_max_upload_size_mb: 5,
    chat_max_file_upload_size_mb: 1,
    chat_max_image_upload_count: 2,
    chat_max_image_upload_size_mb: 6,
  };

  it('derives both uploads from the served limit (the rig seeds 1 MB)', () => {
    expect(uploadLimitPlan(rig)).toEqual({
      ok: true,
      discriminating: true,
      limitMb: 1,
      oversizedBytes: 2 * 1024 * 1024,
      acceptedBytes: 512 * 1024,
    });
  });

  it('keeps the accepted file small however generous the served limit is', () => {
    // The accepted upload is at most half the limit and never more than
    // 512 KiB, so a large limit does not turn step 3 into a large upload.
    const plan = uploadLimitPlan({ ...rig, chat_max_file_upload_size_mb: 40 });
    expect(plan).toMatchObject({ discriminating: true, oversizedBytes: 41 * 1024 * 1024, acceptedBytes: 512 * 1024 });
  });

  it('says a deployment on the client defaults cannot discriminate (live: 150 MB)', () => {
    const plan = uploadLimitPlan({ ...CLIENT_DEFAULT_CHAT_LIMITS });
    expect(plan.ok).toBe(true);
    expect(plan).toMatchObject({ discriminating: false, limitMb: 150 });
    expect(plan.reason).toMatch(/client's own fallback/);
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
