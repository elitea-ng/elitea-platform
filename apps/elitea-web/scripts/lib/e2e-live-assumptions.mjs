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
 * Whether a case that WRITES shared state (publishes into the real catalogue,
 * flips a platform flag, invites a user) is refused on this run. Every live
 * run, triage included: `LIVE_INCLUDE_ENV_DEPENDENT=1` re-admits cases that
 * need the rig, never cases that would harm the instance.
 */
export function refusedOnLiveTarget(env) {
  return isLiveTarget(env);
}

/**
 * Run `cleanup`, then rethrow `skipped` — the signal a deployment probe's skip
 * raised — whatever the cleanup does. A cleanup that fails (a transient 5xx on
 * a shared instance) must not replace the skip and turn a deployment-absence
 * skip into a test failure; the autotest_ sweep collects what it leaves.
 */
export async function rethrowSkipAfterCleanup(skipped, cleanup) {
  try {
    await cleanup();
  } catch {
    // Deliberately swallowed: see above.
  }
  throw skipped;
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
  // Specs switch THEMSELVES to `admin.json` (`test.use({ storageState:
  // STORAGE_STATE.admin })`) in every lane, so whatever that persona may do,
  // a live run does. The run has to say which it is, out loud.
  if (adminPersonaScope(env) === undefined) {
    missing.push(
      'LIVE_ADMIN_PERSONA_SCOPE (project|platform: what the admin.json persona may administer — ' +
        'specs switch to it themselves)',
    );
  }
  return missing.length === 0 ? undefined : `playwright.live.config.ts: set ${missing.join(', ')}`;
}

/**
 * What the run declared its `admin.json` persona may administer:
 *
 *   project   an admin of the regression project only — admin-scoped writes
 *             cannot leave E2E_PROJECT_ID. The admin-readonly lane, whose
 *             pages need the platform administration role, is not run.
 *   platform  a holder of the platform administration role — the safety list
 *             is then all that keeps platform-wide writes off the instance.
 */
export function adminPersonaScope(env) {
  const value = envValue(env, 'LIVE_ADMIN_PERSONA_SCOPE');
  return value === 'project' || value === 'platform' ? value : undefined;
}

/**
 * The trace mode of a live run. Off unless the run opts in with
 * `LIVE_TRACE_WITH_SESSION_COOKIES=1`: a Playwright trace records the request
 * `Cookie` header and the context's storage state, so a retained trace holds a
 * working session for the deployed instance — the admin persona's included.
 */
export function liveTraceMode(env) {
  return env['LIVE_TRACE_WITH_SESSION_COOKIES'] === '1' ? 'retain-on-failure' : 'off';
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
 * The limits `scripts/e2e-stack.sh` seeds into the ADMIN vault. Every value
 * differs from `CLIENT_DEFAULT_CHAT_LIMITS`, so a body carrying only the
 * reader's defaults (the admin-regular fallback of `lookupCurrentChatInteger`
 * broken, or the seed lost) cannot equal it. On the rig J20f asserts the
 * served body is exactly this, for all five keys.
 */
export const RIG_SEEDED_CHAT_LIMITS = Object.freeze({
  chat_max_upload_count: 4,
  chat_max_upload_size_mb: 5,
  chat_max_file_upload_size_mb: 1,
  chat_max_image_upload_count: 2,
  chat_max_image_upload_size_mb: 6,
});

/**
 * Playwright refuses a `setInputFiles` BUFFER payload of 50 MiB or more
 * ("Cannot set buffer larger than 50Mb, please write it to a file and pass its
 * path instead." — `filePayloadExceedsSizeLimit` compares with `>=`). A file
 * that large has to be written to disk and passed by path.
 */
export const PLAYWRIGHT_BUFFER_LIMIT_BYTES = 50 * MIB;

/**
 * How J20f proves the client ACTS on the server's file limit, from the limits
 * the server actually served.
 *
 * A file one MiB over the served limit must be refused and a file under it
 * accepted. That only distinguishes "the client read the config" from "the
 * client fell back to its default" while the oversized file is one the
 * fallback would ACCEPT: no larger than its 150 MiB (the client refuses
 * `size > limit`). A served limit of 150 MB or more fails that — at 150 the
 * two limits are equal, and above it the oversized file is over the fallback
 * too, so a client that never read the config refuses it just the same and
 * the step would pass vacuously. The plan says so instead.
 *
 * `oversizedViaFile` is true when the oversized file reaches Playwright's
 * buffer cap, so the caller writes it to disk instead of failing on a harness
 * limit that says nothing about the product.
 */
export function uploadLimitPlan(config) {
  const keys = Object.keys(CLIENT_DEFAULT_CHAT_LIMITS);
  const malformed = keys.filter((key) => !Number.isInteger(config?.[key]) || config[key] <= 0);
  if (malformed.length > 0) {
    return { ok: false, reason: `chat_config served no positive integer for ${malformed.join(', ')}` };
  }
  const limitMb = config.chat_max_file_upload_size_mb;
  const fallbackMb = CLIENT_DEFAULT_CHAT_LIMITS.chat_max_file_upload_size_mb;
  const oversizedBytes = (limitMb + 1) * MIB;
  if (oversizedBytes > fallbackMb * MIB) {
    return {
      ok: true,
      discriminating: false,
      limitMb,
      reason:
        limitMb === fallbackMb
          ? `the served file limit (${limitMb} MB) is the client's own fallback, so no upload can ` +
            'tell a client that read the config from one that did not'
          : `the served file limit (${limitMb} MB) is above the client's ${fallbackMb} MB fallback, so ` +
            'a client that never read the config refuses a file over it just the same',
    };
  }
  return {
    ok: true,
    discriminating: true,
    limitMb,
    oversizedBytes,
    oversizedViaFile: oversizedBytes >= PLAYWRIGHT_BUFFER_LIMIT_BYTES,
    acceptedBytes: Math.min(512 * 1024, Math.floor((limitMb * MIB) / 2)),
  };
}

/**
 * What J20f does with a plan that cannot discriminate. On a live target
 * (outside triage) it is a deployment fact and the case is skipped. On the rig
 * it FAILS: the rig seeds 1 MB, so a non-discriminating limit there means the
 * seed or the admin-vault fallback broke — the regression the journey guards.
 */
export function nonDiscriminatingOutcome(env) {
  return shouldSkipForDeployment(true, env) ? 'skip' : 'fail';
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
  let message;
  try {
    message = JSON.parse(body)?.error;
  } catch {
    return false;
  }
  if (typeof message !== 'string') return false;
  // The server's own sentence (`validateToolkitCreate`: "settings.<key> is
  // required for <type> toolkit"), naming ONE field and nothing else. A
  // substring match would also excuse an error that lists a credential key
  // beside a field this journey sent, or one that merely mentions the key.
  return credentialKeys.some((key) => {
    if (key === '') return false;
    const escaped = key.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    return new RegExp(`^settings\\.${escaped} is required for [\\w-]+ toolkit$`).test(message);
  });
}

/**
 * Whether J17C.1 may move past a credential-refused type and count a category
 * as credential-only. Only on a live target, outside triage: the rig creates
 * every representative, and a refusal there is the defect.
 */
export function toleratesCredentialOnlyCategories(env) {
  return shouldSkipForDeployment(true, env);
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

/**
 * The body `Handler.AvailableTools` answers when discovery is not composed
 * (`h.discovery == nil`). The SAME route also answers 503 for a composed
 * discovery whose settings resolution is down (`toolkitrun.WriteError`:
 * "toolkit settings could not be resolved") — a runtime fault, not a
 * deployment choice — so only this exact error counts as "absent".
 */
const TOOLKIT_DISCOVERY_ABSENT_ERROR = 'toolkit discovery unavailable';

/** Whether an `AvailableTools` answer says discovery is not composed — and nothing else. */
export function discoveryAbsentAnswer(status, body) {
  if (status !== 503 || typeof body !== 'string') return false;
  try {
    return JSON.parse(body)?.error === TOOLKIT_DISCOVERY_ABSENT_ERROR;
  } catch {
    return false;
  }
}

/**
 * Whether a `GET /configurations/models/{pid}` answer lists `modelName` (by row
 * name or `data.name`). Throws on a non-2xx status or a body that is not the
 * catalogue: a 403 or 500 there is a fault, and a probe that read it as "not
 * listed" would report a broken catalogue as a deployment that disallows
 * project-own models.
 */
export function catalogueListsModel(status, body, modelName) {
  if (status < 200 || status > 299) {
    throw new Error(
      `the model catalogue read failed (${status}), so whether the project sees ${modelName} cannot be ` +
        `told: ${String(body).slice(0, 300)}`,
    );
  }
  let parsed;
  try {
    parsed = JSON.parse(body);
  } catch {
    throw new Error(`the model catalogue answered ${status} with a non-JSON body: ${String(body).slice(0, 300)}`);
  }
  const items = Array.isArray(parsed?.items) ? parsed.items : [];
  return items.some((row) => row?.name === modelName || row?.data?.name === modelName);
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
  // every case publishes into the deployment's real catalogue (a helper that
  // POSTs /publish/prompt_lib/, outside any one test). Files that publish in
  // SOME cases guard those cases with `neverOnLiveTarget` instead; the unit
  // test's selection scan holds both to it.
  'journeys/agents/agents.hub-discovery.spec.ts',
  'journeys/agents/agents.unpublished-conversation.spec.ts',
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

/* ── the lanes, shared by playwright.live.config.ts and the selection scan ── */

export const LIVE_API_MATCH = /journeys\/api\/.+\.spec\.ts$/;
export const LIVE_JOURNEYS_MATCH = /journeys\/.+\.spec\.ts$/;
/** live-journeys leaves these to their own lanes. */
export const LIVE_JOURNEYS_OWN_IGNORE = Object.freeze([/journeys\/api\//, /journeys\/admin\//]);

/**
 * The OPT-IN streaming lane: only the streaming specs that do NOT read the
 * mock journal or name E2E-MOCK-MODEL; chat.admin-providers is left out (it
 * publishes PLATFORM provider credentials).
 */
export const LIVE_STREAM_ALLOWLIST = Object.freeze([
  /streaming\/chat\.(agent|agent-tools|nested-agent|messageRefusals|project-context-injection)\.spec\.ts$/,
  /streaming\/chat\.pipeline(-authored|-execution|-multinode|-triggers)?\.spec\.ts$/,
  /streaming\/index\.lifecycle\.spec\.ts$/,
]);

/** The admin-readonly lane's entries; the audit trail joins only for triage (seeded rows). */
export function liveAdminReadonly(env) {
  return [
    ...LIVE_ADMIN_READONLY,
    ...(includesEnvDependent(env) ? ['journeys/admin/admin.audit-trail.spec.ts'] : []),
  ];
}

/**
 * Every spec (an `e2e/`-relative path) some live lane selects under `env`,
 * computed the way `playwright.live.config.ts` builds its projects.
 */
export function liveSelectedSpecs(specs, env) {
  const ignored = liveTestIgnore(env);
  const safety = LIVE_SAFETY_EXCLUDED.map(livePathPattern);
  const admin = liveAdminReadonly(env).map(livePathPattern);
  const any = (patterns, spec) => patterns.some((pattern) => pattern.test(spec));
  return specs.filter((spec) => {
    const api = LIVE_API_MATCH.test(spec) && !any(ignored, spec);
    const journeys =
      LIVE_JOURNEYS_MATCH.test(spec) && !any(LIVE_JOURNEYS_OWN_IGNORE, spec) && !any(ignored, spec);
    const adminLane = adminPersonaScope(env) === 'platform' && any(admin, spec) && !any(safety, spec);
    const stream = any(LIVE_STREAM_ALLOWLIST, spec);
    return api || journeys || adminLane || stream;
  });
}

/* ── shared-state writers a live run must never reach ──────────────────────── */

/**
 * Code shapes that write state other users of a shared instance see. A spec a
 * live lane selects may contain one only inside a case guarded by
 * `neverOnLiveTarget(` (in the case, or in its describe's `beforeEach`), or a
 * case that says why it is safe with a `live-safe:` comment.
 */
const LIVE_SHARED_STATE_MARKERS = Object.freeze([
  { name: 'platform flag write', pattern: /withPlatformFlag\(/ },
  { name: 'catalogue publish', pattern: /\/publish\/prompt_lib\// },
  { name: 'publish wizard', pattern: /agent-publish-menuitem/ },
  // A hardcoded persona e-mail only creates a user when it is SENT (an invite
  // body, a typed invite form); looked up by a GET it merely fails on an
  // instance without the rig's personas — env-dependent, not unsafe.
  { name: 'hardcoded autotest.local user sent in a write', pattern: /@autotest\.local/, needsWrite: true },
  { name: 'invite form', pattern: /inviteUsers=1|title="Invite users"/ },
  { name: 'logout', pattern: /\/logout\b/ },
  { name: 'administration-mode write', pattern: /mode\/administration/, needsWrite: true },
]);

const GUARD = /\bneverOnLiveTarget\(/;
const LIVE_SAFE = /\/\/\s*live-safe:|\*\s*live-safe:/;
const WRITE_CALL = /\.(post|put|patch|delete|fill)\(/;
/** Lines either side of a `needsWrite` marker in which a write call makes it one. */
const WRITE_WINDOW = 3;

/** The source with comments blanked out, line structure kept. */
function stripComments(source) {
  const blanked = source.replace(/\/\*[\s\S]*?\*\//g, (block) => block.replace(/[^\n]/g, ' '));
  // A `//` comment starts the line or follows whitespace; `https://` in a
  // string follows a colon and is kept.
  return blanked.replace(/(^|[\s;{}(),])\/\/.*$/gm, '$1');
}

/** [start, end] line ranges of blocks opened by `opener` and closed by `closer`. */
function blocks(lines, opener, closer) {
  const found = [];
  for (let start = 0; start < lines.length; start += 1) {
    if (!opener.test(lines[start])) continue;
    let end = start;
    while (end + 1 < lines.length && !closer.test(lines[end])) end += 1;
    found.push([start, end]);
    start = end;
  }
  return found;
}

/**
 * Where a spec's source reaches shared state outside a guarded case — one
 * `{ line, marker, reason }` per unguarded occurrence (line is 1-based).
 */
export function liveSharedStateViolations(source) {
  const raw = source.split('\n');
  const code = stripComments(source).split('\n');
  // Closed by the formatter's own `});` at the opener's indent — not by the
  // `}) => {` that ends a multi-line parameter list.
  const topLevel = blocks(code, /^test(\.describe)?(\.(serial|parallel|only|fixme|skip))*\(/, /^\}\);\s*$/);
  const nested = blocks(code, /^ {2}test(\.(only|fixme|skip|fail))?\(/, /^ {2}\}\);\s*$/);
  const has = (pattern, [start, end], lines) => lines.slice(start, end + 1).some((line) => pattern.test(line));
  const violations = [];
  code.forEach((line, index) => {
    for (const marker of LIVE_SHARED_STATE_MARKERS) {
      if (!marker.pattern.test(line)) continue;
      const window = code.slice(Math.max(0, index - WRITE_WINDOW), index + WRITE_WINDOW + 1);
      if (marker.needsWrite && !window.some((l) => WRITE_CALL.test(l))) continue;
      const outer = topLevel.find(([start, end]) => start <= index && index <= end);
      if (outer === undefined) {
        violations.push({ line: index + 1, marker: marker.name, reason: 'outside any test (a shared helper)' });
        continue;
      }
      const isDescribe = code[outer[0]].startsWith('test.describe');
      const inner = nested.find(([start, end]) => outer[0] <= start && start <= index && index <= end);
      if (isDescribe) {
        const firstCase = nested.find(([start]) => outer[0] <= start && start <= outer[1]);
        const preamble = [outer[0], (firstCase?.[0] ?? outer[1]) - 1];
        if (has(GUARD, preamble, code)) continue;
        if (inner !== undefined && (has(GUARD, inner, code) || has(LIVE_SAFE, inner, raw))) continue;
        violations.push({
          line: index + 1,
          marker: marker.name,
          reason: inner === undefined ? 'in a describe helper, describe not guarded' : 'in an unguarded case',
        });
        continue;
      }
      if (has(GUARD, outer, code) || has(LIVE_SAFE, outer, raw)) continue;
      violations.push({ line: index + 1, marker: marker.name, reason: 'in an unguarded case' });
    }
  });
  return violations;
}
