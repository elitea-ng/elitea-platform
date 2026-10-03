/**
 * The decisions that let the Playwright suites run against a DEPLOYED instance
 * (`playwright.live.config.ts`) as well as the e2e rig, kept free of
 * Playwright, a browser and a stack so each one is unit-tested
 * (`scripts/e2e-live-assumptions.test.mjs`).
 *
 * Every default reproduces the rig (`scripts/e2e-stack.sh`): a run that sets
 * none of the variables below behaves exactly as the suites always did.
 */

/** The `E2E_PUBLISH_AUTHOR_PROJECT_ID` value that says "this deployment has none". */
export const NO_PROJECT = 'none';

/** A variable's trimmed value, or undefined when it is unset or blank. */
export function envValue(env, name) {
  const raw = env[name];
  if (typeof raw !== 'string') return undefined;
  const value = raw.trim();
  return value === '' ? undefined : value;
}

/**
 * The tenancy a run targets.
 *
 *   E2E_PROJECT_ID                 the project the personas work in (rig: 1)
 *   E2E_DEFAULT_PROJECT_NAME       its name in the switcher (rig: Default Project)
 *   E2E_PUBLIC_PROJECT_ID          the FRONTEND-public project (VITE_PUBLIC_PROJECT_ID);
 *                                  rig: resolved by the seeded name `e2e-public`
 *   E2E_CATALOGUE_PROJECT_ID       the backend catalogue (ELITEA_AI_PROJECT_ID);
 *                                  rig: read from platform_settings
 *   E2E_PUBLISH_AUTHOR_PROJECT_ID  the publish journeys' author project, or `none`;
 *                                  rig: resolved by the seeded name `e2e-publish-author`
 */
export function e2eTenancy(env) {
  return {
    projectId: envValue(env, 'E2E_PROJECT_ID') ?? '1',
    projectName: envValue(env, 'E2E_DEFAULT_PROJECT_NAME') ?? 'Default Project',
    publicProjectId: envValue(env, 'E2E_PUBLIC_PROJECT_ID'),
    catalogueProjectId: envValue(env, 'E2E_CATALOGUE_PROJECT_ID'),
    publishAuthorProjectId: envValue(env, 'E2E_PUBLISH_AUTHOR_PROJECT_ID'),
  };
}

/** The run targets a deployed instance (`playwright.live.config.ts` sets it). */
export function isLiveTarget(env) {
  return env['E2E_TARGET'] === 'live';
}

/** The live run re-admits env-dependent cases so their failures can be triaged. */
export function includesEnvDependent(env) {
  return env['LIVE_INCLUDE_ENV_DEPENDENT'] === '1';
}

/**
 * Whether a case whose deployment prerequisite is `absent` is skipped.
 *
 * Only on a live target, only when the prerequisite is really absent, and
 * never while triaging. On the rig the same absence IS the defect, so the case
 * runs and fails on its own assertion.
 */
export function shouldSkipForDeployment(absent, env) {
  return absent === true && isLiveTarget(env) && !includesEnvDependent(env);
}

/**
 * Why a live run must not start, or undefined when it may.
 *
 * Project 1 holds real data on a deployed instance, so the live config refuses
 * to default to it or to the rig's project name; it also needs the https
 * origin (the session cookie is `Secure`) and the persona storage states it
 * cannot mint itself.
 */
export function liveRunRefusal(env) {
  const baseUrl = envValue(env, 'PLAYWRIGHT_BASE_URL') ?? '';
  const missing = [];
  if (!baseUrl.startsWith('https://')) missing.push('PLAYWRIGHT_BASE_URL (https)');
  if (envValue(env, 'E2E_STATE_DIR') === undefined) missing.push('E2E_STATE_DIR');
  const projectId = envValue(env, 'E2E_PROJECT_ID');
  if (projectId === undefined || projectId === '1') {
    missing.push('E2E_PROJECT_ID (the regression project, never 1: project 1 holds real data)');
  }
  // The rig's name would select a project of that name on the deployment, if
  // it has one — the wrong project, chosen silently.
  if (envValue(env, 'E2E_DEFAULT_PROJECT_NAME') === undefined) {
    missing.push('E2E_DEFAULT_PROJECT_NAME (the name the switcher shows for E2E_PROJECT_ID)');
  }
  return missing.length === 0 ? undefined : `playwright.live.config.ts: set ${missing.join(', ')}`;
}

/**
 * The chat-config reader's own defaults (10/150/150/10/3) — what a deployment
 * that sets nothing serves. The file limit is also `useArtifactUpload`'s
 * `DEFAULT_MAX_FILE_SIZE` (150 MB), the limit a client that never read the
 * config enforces.
 */
export const CLIENT_DEFAULT_CHAT_LIMITS = Object.freeze({
  chat_max_upload_count: 10,
  chat_max_upload_size_mb: 150,
  chat_max_file_upload_size_mb: 150,
  chat_max_image_upload_count: 10,
  chat_max_image_upload_size_mb: 3,
});

const MIB = 1024 * 1024;

/**
 * How J20f proves the client ACTS on the server's file limit, from the limits
 * the server actually served.
 *
 * A file one MiB over the served limit must be refused and a file under it
 * accepted. That only distinguishes "the client read the config" from "the
 * client fell back to its default" while the two limits differ; when they are
 * equal (a deployment that leaves the defaults) no upload can tell them apart,
 * and the plan says so instead of passing on the fallback.
 */
export function uploadLimitPlan(config) {
  const keys = Object.keys(CLIENT_DEFAULT_CHAT_LIMITS);
  const malformed = keys.filter((key) => !Number.isInteger(config?.[key]) || config[key] <= 0);
  if (malformed.length > 0) {
    return { ok: false, reason: `chat_config served no positive integer for ${malformed.join(', ')}` };
  }
  const limitMb = config.chat_max_file_upload_size_mb;
  if (limitMb === CLIENT_DEFAULT_CHAT_LIMITS.chat_max_file_upload_size_mb) {
    return {
      ok: true,
      discriminating: false,
      limitMb,
      reason:
        `the served file limit (${limitMb} MB) is the client's own fallback, so no upload can ` +
        'tell a client that read the config from one that did not',
    };
  }
  return {
    ok: true,
    discriminating: true,
    limitMb,
    oversizedBytes: (limitMb + 1) * MIB,
    acceptedBytes: Math.min(512 * 1024, Math.floor((limitMb * MIB) / 2)),
  };
}

/**
 * The settings a toolkit create carries for its type's fillable required
 * fields — the same `autotest-<key>` values the create form is typed with.
 */
export function schemaFilledSettings(fillableKeys) {
  const settings = { selected_tools: [] };
  for (const key of fillableKeys) settings[key] = `autotest-${key}`;
  return settings;
}

/**
 * Whether a refused toolkit create was refused for a CREDENTIAL the type
 * requires — something an empty project cannot supply — rather than for
 * anything this journey sent.
 */
export function refusedForMissingCredential(status, body, credentialKeys) {
  if (status !== 400 || typeof body !== 'string') return false;
  return credentialKeys.some((key) => key !== '' && body.includes(key));
}

/**
 * Whether the app under test has a socket server, from the runner's own
 * `VITE_SOCKET_SERVER` or the page's runtime `/app/config.js` object.
 */
export function socketServerConfigured(env, uiConfig) {
  if (envValue(env, 'VITE_SOCKET_SERVER') !== undefined) return true;
  const fromPage = uiConfig?.['vite_socket_server'] ?? uiConfig?.['VITE_SOCKET_SERVER'];
  return typeof fromPage === 'string' && fromPage.trim() !== '';
}

/* ── which journeys a live run selects ─────────────────────────────────────── */

/**
 * NEVER run on a shared deployed instance: they write platform-wide state,
 * publish to the public catalogue real users see, log the shared session out,
 * suspend or delete users and projects, or invite the hardcoded
 * `e2e-*@autotest.local` accounts (which would CREATE those users there).
 * Paths are relative to `e2e/`; a trailing `/` names a whole directory and a
 * trailing `*` a file-name prefix (`livePathPattern`).
 */
export const LIVE_SAFETY_EXCLUDED = Object.freeze([
  // platform-flag writers (withPlatformFlag) — flip global flags for every user
  'journeys/admin/admin.agent-publishing-guardrails.spec.ts',
  'journeys/admin/admin.branding.spec.ts',
  'journeys/admin/admin.features.spec.ts',
  'journeys/admin/admin.guardrails.spec.ts',
  'journeys/admin/admin.resources-help-center.spec.ts',
  'journeys/shell/shell.resources-version-info.spec.ts',
  // admin writes on real platform data
  'journeys/admin/admin.app-requests.spec.ts',
  'journeys/admin/admin.configuration.spec.ts',
  'journeys/admin/admin.projects.spec.ts',
  'journeys/admin/admin.roles.spec.ts',
  'journeys/admin/admin.schedules.spec.ts',
  'journeys/admin/admin.secrets.spec.ts',
  'journeys/admin/admin.service-descriptors.spec.ts',
  'journeys/admin/admin.tasks.spec.ts',
  'journeys/admin/admin.toolkit-types.spec.ts',
  'journeys/admin/admin.users.spec.ts',
  // support assistant: rewires the platform support project / auto-enrols users
  'journeys/support/',
  // destructive autotest_ sweep across projects
  'journeys/api/api.fixture-isolation.spec.ts',
  // public-catalogue publishing, moderation, model grants and forks into the
  // seeded e2e-public / e2e-publish-author projects
  'journeys/api/api.publish-*',
  'journeys/api/api.model-grants.spec.ts',
  'journeys/api/api.import-wizard.spec.ts',
  'journeys/api/api.export-import-fork-gaps.spec.ts',
  'journeys/api/api.tail-fork-subagents.spec.ts',
  'journeys/chat/chat.duplicate.spec.ts',
  'journeys/chat/chat.issues-sharing.spec.ts',
  'journeys/chat/chat.sharing.spec.ts',
  'journeys/chat/chat.legacy-conversation-isolation.spec.ts',
  'journeys/shell/shell.sidebar-public-project.spec.ts',
  // logout (revokes the shared persona session mid-run) / OIDC mock sign-in
  'journeys/shell/shell.redirect.spec.ts',
  'journeys/shell/shell.session.spec.ts',
  'journeys/shell/shell.reauth-popup.spec.ts',
  // scratch projects / user invites with hardcoded e2e-*@autotest.local emails
  'journeys/agents/agents.sweep-editor-deletion.spec.ts',
  'journeys/settings/settings.closed-gaps.spec.ts',
  'journeys/settings/settings.p13-project-context.spec.ts',
  'journeys/settings/settings.project-context.spec.ts',
  'journeys/toolkits/toolkits.tail-aha.spec.ts',
  'journeys/toolkits/toolkits.tail-credential-warning-modal.spec.ts',
  'journeys/settings/settings.users.spec.ts',
  'journeys/settings/settings.model-request.spec.ts',
  'journeys/settings/settings.project-request.spec.ts',
  'journeys/artifacts/artifacts.p13-bucket-permissions.spec.ts',
  // runs the PLATFORM-WIDE pat_expiry_notices job (would notify real users)
  'journeys/settings/settings.pat-expiry-notifications.spec.ts',
  // admin budget PUT on a seeded e2e-budget-<engine> project
  'journeys/settings/settings.budget-warning.spec.ts',
]);

/**
 * Need the rig, not the product — whole files whose every case depends on it.
 * Failures here are environment, not product; `LIVE_INCLUDE_ENV_DEPENDENT=1`
 * re-admits them for triage. Where only SOME cases in a file depend on the
 * rig, those cases skip themselves instead (`e2e/fixtures/deployment.ts`).
 */
export const LIVE_ENV_DEPENDENT = Object.freeze([
  'journeys/agents/agents.issues-import-version-format.spec.ts', // E2E-MOCK-MODEL
  // seeded rig fixtures beyond the project's name (folders, dashboards, resources)
  'journeys/agents/agents.closed-entity-folders-gap.spec.ts',
  'journeys/agents/agents.list.spec.ts',
  'journeys/pipelines/pipelines.dashboard.spec.ts',
  'journeys/shell/shell.resources.spec.ts',
  'journeys/pipelines/pipelines.version-selector.spec.ts', // hardcoded persona email
  'journeys/artifacts/artifacts.bucket-access.spec.ts', // hardcoded persona email
  'journeys/admin/admin.audit-trail.spec.ts', // seeded audit fixture rows
  'journeys/toolkits/toolkits.emptyList.spec.ts', // must run before anything seeds
  // UI-ENV-1: the stand-in Aha host is refused by the deployment egress allowlist
  'journeys/toolkits/toolkits.aha.spec.ts',
  // UI-ENV-2: no Inventory or DeepWiki provider is configured
  // (ELITEA_INVENTORY_BASE_URL, ELITEA_DEEPWIKI_BASE_URL empty), and every
  // DeepWiki journey reads the seeded wiki toolkit
  'journeys/inventory/',
  'journeys/deepwiki/',
]);

/** Admin specs that only READ (navigation, titles, access-denied, closed-gap probes). */
export const LIVE_ADMIN_READONLY = Object.freeze([
  'journeys/admin/admin.access-denied.spec.ts',
  'journeys/admin/admin.navigation.spec.ts',
  'journeys/admin/admin.legacy-bk45-page-title.spec.ts',
  'journeys/admin/admin.closed-budgets-gaps.spec.ts',
  'journeys/admin/admin.closed-config-feature-gaps.spec.ts',
]);

/**
 * A Playwright `testMatch`/`testIgnore` pattern for one `e2e/`-relative entry:
 * a spec file, a directory (trailing `/`), or a file-name prefix (trailing `*`).
 */
export function livePathPattern(entry) {
  const prefix = entry.endsWith('*') ? entry.slice(0, -1) : entry;
  const escaped = prefix.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const open = entry.endsWith('/') || entry.endsWith('*');
  return new RegExp(open ? `${escaped}.*\\.spec\\.ts$` : `${escaped}$`);
}

/** What a live journey lane ignores: always the safety list, the env list unless triaging. */
export function liveTestIgnore(env) {
  const entries = includesEnvDependent(env)
    ? LIVE_SAFETY_EXCLUDED
    : [...LIVE_SAFETY_EXCLUDED, ...LIVE_ENV_DEPENDENT];
  return entries.map(livePathPattern);
}
