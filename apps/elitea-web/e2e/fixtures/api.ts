/**
 * API helpers for E2E test setup (issue #60).
 *
 * Used in test setup steps to create entities via API rather than clicking
 * through 5 screens — the Playwright `request` fixture approach from
 * qa/elitea-testing-public's per-domain API helpers.
 *
 * All entities created here use the `autotest_` prefix (per qa/ convention)
 * so failed runs' leftovers are identifiable and sweepable.
 */
import type { APIRequestContext, APIResponse, Locator, Page } from '@playwright/test';
import { expect, request as playwrightRequest } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../playwright.config';

export const AUTOTEST_PREFIX = 'autotest_';

/**
 * What a 401 MEANS, in the words the server itself used.
 *
 * ## Why this exists
 *
 * elitea-main used to write ONE body — `{"code":"unauthenticated","message":
 * "missing authorization header"}` — for four different findings: no cookie
 * arrived, a cookie signed by another secret arrived, the cookie had expired,
 * or a server-side session had been revoked. A journey that hit it could not
 * say which, and a CI run keeps nothing but the response. #538 spent a whole
 * investigation on one occurrence for exactly that reason, and #832 spent
 * another on the revoked-session case.
 *
 * `middleware/auth.go` now names the finding in `error.code`
 * (`internal/api/credential_refusal_code_route_test.go` pins one case per
 * code). This turns that code into the sentence the next reader needs, so the
 * failure explains itself in the report instead of in a later investigation.
 *
 * It returns '' for anything that is not a named refusal, so a caller can
 * append it unconditionally.
 */
export async function describeRefusal(response: APIResponse): Promise<string> {
  if (response.status() !== 401) return '';
  let code = '';
  try {
    const body = (await response.json()) as { error?: { code?: unknown } } | null;
    const named = body?.error?.code;
    code = typeof named === 'string' ? named : '';
  } catch {
    return '\nauth refusal: the 401 carried no JSON body.';
  }
  if (code === '') return '';
  const hints: Readonly<Record<string, string>> = {
    no_credential:
      'the request context sent NO cookie of ours. Pass `page.request` (which shares the ' +
      'browser context cookies), not the bare `request` fixture.',
    session_revoked:
      'the session was REVOKED — some journey signed this persona out. The suite shares one ' +
      'session per persona, so a logout journey must own the session it ends ' +
      '(e2e/fixtures/session.ts, rule 3 of scripts/e2e-journey-shape.test.mjs).',
    session_expired: 'the server-side session reached its lifetime. The run outlived the session.',
    session_idle: 'the server-side session was idle past its timeout.',
    session_unknown:
      'the server has no row for this session id. The stack was re-seeded or restarted, and the ' +
      'storage state belongs to the previous one — re-run `auth.setup.ts`.',
    session_cookie_signature_mismatch:
      'the cookie was signed with ANOTHER secret. The stack restarted with a new session secret ' +
      'while this storage state was kept.',
    session_cookie_expired: 'the signed cookie passed its `exp`. The suite outlived the cookie.',
    session_cookie_malformed: 'the cookie value is not the signed form this deployment writes.',
    token_rejected: 'the bearer token or API key was refused. It is not a session problem.',
    unauthenticated:
      'the server refused without naming a finding. On elitea-main that is the deployment-side ' +
      'case (no session secret composed), which is written to the server log only.',
  };
  const hint = hints[code] ?? 'no hint recorded for this code yet.';
  return `\nauth refusal: code=${code} — ${hint}`;
}

/**
 * Wait for the sidebar create button to become enabled, then click it.
 * The button stays disabled until the permission query resolves, which
 * requires the project store to hydrate from localStorage. This can take
 * up to ~3 s on a cold page load in the E2E stack.
 */
export async function clickCreateButton(page: Page): Promise<void> {
  const btn = page.getByTestId('sidebar-create-button');
  await expect(btn).toBeEnabled({ timeout: 15_000 });
  await btn.click();
}

/** API base from env (matches the app's VITE_SERVER_URL). */
export const API_BASE = (process.env['PLAYWRIGHT_BASE_URL'] ?? 'http://localhost:8082') + '/api/v2';

/** Default public project ID (matches compose env VITE_PUBLIC_PROJECT_ID). */
export const DEFAULT_PROJECT_ID = process.env['E2E_PROJECT_ID'] ?? '1';

/* ── the tenancy the publish journeys need ───────────────────────────────── */

/**
 * The project the publish journeys AUTHOR in, by name (`scripts/e2e-stack.sh`).
 *
 * Publishing is cross-project: the clone stays in the author's schema and a
 * twin is written into the public project's, which is the only schema the
 * catalogue reads. Project 1 is the public project on this rig, so a publish
 * issued from it never writes a twin and the two halves cannot be told apart.
 * This project is the author side of that pair.
 *
 * NAMED, NOT NUMBERED, and resolved through the product's own project listing
 * below. The seed picks the id out of a reserved range, and a journey that
 * repeated the literal would keep passing against a project that had moved —
 * or, worse, against whatever row later took that id.
 */
export const PUBLISH_AUTHOR_PROJECT_NAME = 'e2e-publish-author';

/**
 * The seeded model that is NOT shared: it lives in the author project, so
 * `llm_settings.model_project_id` naming it is refused publication
 * (`llm_not_shared`) and flagged critical by the pre-publish check.
 *
 * The name is the `data.name` the catalogue serves, which is the value a
 * version's `llm_settings.model_name` carries.
 */
export const PRIVATE_MODEL_NAME = 'E2E-PRIVATE-MODEL';

/**
 * The route the project switcher itself calls.
 *
 * The trailing `1` is part of the reference URL, not a project id the caller
 * chooses: `internal/api/v2/projects/handler.go` mounts this exact path and
 * uses the segment for the `check_public_role` filter alone. The handler
 * answers the CALLER's projects, whatever stands there.
 */
const PROJECT_LIST_PATH = '/projects/project/default/1';

/** Resolved ids, memoised per worker process. Only successes are cached. */
const resolvedProjectIds = new Map<string, string>();

/**
 * The id of a project the caller belongs to, found by its seeded NAME.
 *
 * Reads through `?search=`, then matches the name EXACTLY: `search` is an
 * `ILIKE '%…%'` on the server, so asking for `e2e-publish-author` would also
 * answer a project someone later names `e2e-publish-author-2`.
 */
export async function resolveProjectIdByName(
  request: APIRequestContext,
  name: string,
): Promise<string> {
  const cached = resolvedProjectIds.get(name);
  if (cached !== undefined) return cached;
  const url = `${API_BASE}${PROJECT_LIST_PATH}?search=${encodeURIComponent(name)}`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(
      `resolveProjectIdByName(${name}): GET ${url} -> ${response.status()}` +
        `${await describeRefusal(response)}\n${(await response.text()).slice(0, 300)}`,
    );
  }
  const rows = (await response.json()) as readonly { readonly id?: unknown; readonly name?: unknown }[];
  const found = (Array.isArray(rows) ? rows : []).find((row) => row.name === name);
  if (found === undefined) {
    throw new Error(
      `resolveProjectIdByName(${name}): the caller is a member of no project of that name. ` +
        `scripts/e2e-stack.sh seeds it and grants both personas the admin role in it; a stack ` +
        `seeded before that change has the row missing. Answer was ` +
        `${JSON.stringify(rows).slice(0, 300)}`,
    );
  }
  const id = String(found.id);
  resolvedProjectIds.set(name, id);
  return id;
}

/** The seeded author project's id — the project a publish is issued FROM. */
export async function resolvePublishAuthorProjectId(
  request: APIRequestContext,
): Promise<string> {
  return resolveProjectIdByName(request, PUBLISH_AUTHOR_PROJECT_NAME);
}

/**
 * The CATALOGUE project's id, as the server itself reports it.
 *
 * The deployment decides this (`ELITEA_AI_PROJECT_ID`, `internal/publicproject`),
 * and the browser learns it from `platform_settings` — so a journey reads it
 * from the same place rather than repeating a default that is only true while
 * nobody sets the variable.
 */
export async function resolveCatalogueProjectId(request: APIRequestContext): Promise<string> {
  const cached = resolvedProjectIds.get('#catalogue');
  if (cached !== undefined) return cached;
  const url = `${API_BASE}/elitea_core/platform_settings/prompt_lib`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(
      `resolveCatalogueProjectId: GET ${url} -> ${response.status()}` +
        `${await describeRefusal(response)}`,
    );
  }
  const body = (await response.json()) as { public_project_id?: unknown };
  const id = body.public_project_id;
  if (typeof id !== 'number' && typeof id !== 'string') {
    throw new Error(
      `resolveCatalogueProjectId: platform_settings named no public_project_id: ` +
        `${JSON.stringify(body).slice(0, 300)}`,
    );
  }
  const value = String(id);
  resolvedProjectIds.set('#catalogue', value);
  return value;
}

/** One row of the model catalogue, as the picker reads it. */
export interface CatalogueModel {
  /** The model NAME a version carries in `llm_settings.model_name`. */
  readonly name: string;
  /** The configuration row's own title, for a message that names the row. */
  readonly title: string;
  /** The project the row lives in — what the publish guard compares. */
  readonly projectId: string;
}

/**
 * The models a project may name, through the route the picker itself calls.
 *
 * TWO SHAPES, one route. When the Configurations plane is composed
 * (`ELITEA_CONFIGURATIONS_ENABLED` + `ELITEA_AI_PROJECT_ID`) the production
 * route answers items whose `name` is already the model name; without it the
 * legacy handler answers the configuration ROW, whose `name` is the row's
 * title and whose `data.name` is the model. The e2e rig runs the second, a
 * full deployment the first, and a helper that read only one of them would
 * resolve a title as a model name on one of the two and fail to match at all.
 */
export interface ReadProjectModelsOptions {
  /**
   * Merge the CATALOGUE's published models into the answer.
   *
   * The route defaults it to false, and the product sends it as
   * `projectId !== publicProjectId` (`configurationsPanel.helpers.ts`). A
   * journey about a PLATFORM model has to ask for it: without it the answer is
   * the project's own rows and a platform model — granted or not — is absent
   * from every one of them, so an assertion made on the default would pass
   * whatever the grant said.
   */
  readonly includeShared?: boolean;
}

export async function readProjectModels(
  request: APIRequestContext,
  projectId: string,
  options: ReadProjectModelsOptions = {},
): Promise<readonly CatalogueModel[]> {
  const query = options.includeShared === true ? '?include_shared=true' : '';
  const url = `${API_BASE}/configurations/models/${projectId}${query}`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readProjectModels(${projectId}): GET ${url} -> ${response.status()}` +
        `${await describeRefusal(response)}\n${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as { items?: readonly Record<string, unknown>[] };
  return (body.items ?? []).map((row) => {
    const title = String(row['name'] ?? '');
    const data = (row['data'] as Record<string, unknown> | undefined) ?? {};
    const modelName = typeof data['name'] === 'string' && data['name'] !== '' ? data['name'] : title;
    return { name: modelName, title, projectId: String(row['project_id'] ?? '') };
  });
}

/**
 * The seeded NON-shared model: its name and the project that owns it.
 *
 * Resolved rather than assumed. The refusal it exists for is decided on
 * `llm_settings.model_project_id` against the ONE public project, so a journey
 * that invented an id would prove the guard refuses an id naming nothing —
 * which a guard comparing against "any project the caller can see" also passes.
 * This one names a model that really exists, in a project the caller really
 * belongs to, and is private for the only reason that matters here: its project
 * is not the catalogue's.
 */
export async function resolvePrivateModel(
  request: APIRequestContext,
): Promise<{ readonly modelName: string; readonly projectId: string }> {
  const projectId = await resolvePublishAuthorProjectId(request);
  const catalogueProjectId = await resolveCatalogueProjectId(request);
  if (projectId === catalogueProjectId) {
    throw new Error(
      `resolvePrivateModel: the author project IS the catalogue project (${projectId}), so no ` +
        `model in it can be private. The deployment resolved a different public project than ` +
        `the seed assumes.`,
    );
  }
  const models = await readProjectModels(request, projectId);
  const found = models.find((model) => model.name === PRIVATE_MODEL_NAME);
  if (found === undefined) {
    throw new Error(
      `resolvePrivateModel: project ${projectId} serves no model named ${PRIVATE_MODEL_NAME}. ` +
        `scripts/e2e-stack.sh seeds it with shared = false; it answered ` +
        `${JSON.stringify(models).slice(0, 300)}`,
    );
  }
  return { modelName: found.name, projectId };
}

/**
 * Create a conversation via API.
 * Returns the new conversation id.
 */
export async function createConversation(
  request: APIRequestContext,
  name: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<string> {
  const url = `${API_BASE}/elitea_core/conversations/prompt_lib/${projectId}`;
  const resp = await request.post(url, { data: { name } });
  // Check the status BEFORE parsing. Calling `.json()` on a 401 (whose body is
  // not JSON) produced `SyntaxError: Unexpected non-whitespace character after
  // JSON at position 4` — an error that says nothing about the real problem,
  // which was an unauthenticated request context. Fail with the status and body
  // instead, so the next caller diagnoses it in one read.
  if (!resp.ok()) {
    throw new Error(
      `createConversation: POST ${url} -> ${resp.status()} ${resp.statusText()}` +
      `${await describeRefusal(resp)}\n` +
      `${(await resp.text()).slice(0, 300)}`,
    );
  }
  const body = (await resp.json()) as { id?: string };
  if (body.id === undefined) {
    throw new Error(`createConversation: response carried no id: ${JSON.stringify(body).slice(0, 200)}`);
  }
  return body.id;
}

/**
 * Delete a conversation (cleanup helper).
 */
export async function deleteConversation(
  request: APIRequestContext,
  id: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<void> {
  await request.delete(`${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${id}`);
}

/** An agent created through the API, with the initial version it owns. */
export interface CreatedAgent {
  /** `applications.id` — the SERIAL key every route addresses the agent by. */
  readonly id: string;
  /** `application_versions.id` of the initial version created alongside it. */
  readonly versionId: string;
}

/**
 * Create an agent (application) via API.
 *
 * Sends a `versions` array, exactly as the create-agent form does
 * (`entities/application-form/model/mutations.ts` `toVersionWriteRequest`).
 * An application with no version row is a degenerate entity: `List` INNER
 * JOINs `application_versions`, so it never appears in the agents list, and
 * there is no version for a deep link to open. A fixture that creates one
 * sets its callers up to assert against a shape the product never produces.
 *
 * Throws on a non-2xx response. It previously read `body.id` off whatever
 * came back, so when `POST /elitea_core/applications/...` was returning 404
 * (issue #115) the `.json()` parse threw and every caller's `.catch()`
 * quietly took a degraded branch — the journeys passed while the endpoint
 * they exist to exercise was entirely absent.
 */
export async function createAgent(
  request: APIRequestContext,
  name: string,
): Promise<CreatedAgent> {
  const path = `/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}`;
  const resp = await request.post(`${API_BASE}${path}`, {
    data: {
      name,
      description: `${AUTOTEST_PREFIX}e2e test agent`,
      type: 'agent',
      versions: [
        {
          name: 'base',
          agent_type: 'openai',
          instructions: 'You are a helpful assistant.',
          conversation_starters: [],
        },
      ],
    },
  });
  if (!resp.ok()) {
    throw new Error(
      `createAgent: POST ${path} returned ${resp.status()}: ${(await resp.text()).slice(0, 300)}`,
    );
  }
  const body = await resp.json();
  const id: unknown = body?.id;
  const versionId: unknown = body?.version_details?.id;
  if (typeof id !== 'string' || typeof versionId !== 'string') {
    throw new Error(
      `createAgent: POST ${path} returned 201 without an id/version_details.id: ${JSON.stringify(body).slice(0, 300)}`,
    );
  }
  return { id, versionId };
}

/**
 * Delete an agent (cleanup helper).
 */
export async function deleteAgent(
  request: APIRequestContext,
  id: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<void> {
  await request.delete(
    `${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${id}`,
  );
}


/* ────────────────────────────────────────────────────────────────────────────
 * THE AGENT-VERSION WRITE CONTRACT
 *
 * `createAgent` above is the smallest agent that exists. The helpers below are
 * for the journeys that are ABOUT the version write itself, and they exist
 * because that body has three properties a hand-rolled literal keeps getting
 * wrong:
 *
 *  1. PRESENCE IS THE CONTRACT. `name`, `instructions`, `meta`, `variables`
 *     and `tags` each mean something different when the key is ABSENT than
 *     when it is present and empty — an absent key leaves the stored value
 *     alone, an empty one clears it. A builder that always emits every key
 *     cannot express half the cases, and one that emits `undefined` sends
 *     `null` over the wire, which is a third meaning again. `agentVersionBody`
 *     therefore omits what the caller did not name.
 *  2. `variables` AND `meta` ARE THE SAME COLUMN. Variables have no column of
 *     their own; the server folds them into `meta`. So a body that sends
 *     `variables` and no `meta` is the exact shape that used to destroy the
 *     rest of that column.
 *  3. `llm_settings.model_project_id` NAMES A PROJECT, not a model. It is the
 *     project the model is published in, and the create path refuses one that
 *     does not exist.
 * ──────────────────────────────────────────────────────────────────────────── */

/** One version variable, in the shape both the write and the read use. */
export interface VersionVariableInput {
  readonly name: string;
  readonly value: string;
}

/** One topical tag. `data` is the free-form blob pylon's tag model carries. */
export interface VersionTagInput {
  readonly name: string;
  readonly data?: Readonly<Record<string, unknown>>;
}

/** The model reference a version carries, as `llm_settings`. */
export interface VersionModelInput {
  readonly modelName: string;
  /** The PROJECT the model is published in — not the model's own id. */
  readonly modelProjectId?: string;
  readonly temperature?: number;
  readonly maxTokens?: number;
}

/**
 * Everything a version write can carry. Every field is optional on purpose:
 * an omitted one is omitted from the body, which is the only way to write the
 * "leave the stored value alone" half of the contract.
 */
export interface AgentVersionInput {
  readonly name?: string;
  readonly agentType?: string;
  readonly instructions?: string;
  readonly welcomeMessage?: string;
  readonly conversationStarters?: readonly string[];
  readonly variables?: readonly VersionVariableInput[];
  readonly tags?: readonly VersionTagInput[];
  /** A PATCH of the version's meta bag — the server merges it, key by key. */
  readonly meta?: Readonly<Record<string, unknown>>;
  readonly model?: VersionModelInput;
  /** Ids the id-guard cases send. Only a refusal journey needs them. */
  readonly id?: string;
  readonly applicationId?: string;
}


/** One row of `GET /elitea_core/application/prompt_lib/{project}/{id}`. */
export interface StoredApplicationVersion {
  readonly id: string;
  readonly name: string;
  readonly status: string;
}

/**
 * Every version an agent owns, as the Published tab and the version selector
 * read them.
 *
 * This is the read that tells a publish from a MOVE: publishing clones the
 * source version and leaves the original alone, so after one publish this
 * answers two rows — the untouched draft and the published clone. A handler
 * that flipped the source row's status instead would answer one, and the
 * publish response alone cannot tell the difference.
 */
export async function readApplicationVersions(
  request: APIRequestContext,
  applicationId: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<readonly StoredApplicationVersion[]> {
  const url = `${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${applicationId}`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readApplicationVersions: GET ${url} -> ${response.status()}` +
        `${await describeRefusal(response)}\n${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as { versions?: readonly Record<string, unknown>[] };
  return (body.versions ?? []).map((row) => ({
    id: String(row['id'] ?? ''),
    name: String(row['name'] ?? ''),
    status: String(row['status'] ?? ''),
  }));
}

/** One row of the public catalogue. */
export interface CatalogueRow extends Record<string, unknown> {
  readonly name?: string;
  readonly meta?: Record<string, unknown>;
}

/**
 * ELITEA Catalog, through the route the catalogue page itself calls.
 *
 * Read WITHOUT a page size: the catalogue offers no filter narrow enough to
 * name one agent, and every caller here asks "is this name in it", which a
 * first page cannot answer once the table outgrows it. The route's own default
 * page is what the page renders, so this is the same set a reader sees.
 *
 * `category` narrows it to one tab of the Catalog page, which is the only
 * filter the route offers that a journey can state an expectation about.
 */
export async function readCatalogue(
  request: APIRequestContext,
  category?: string,
): Promise<readonly CatalogueRow[]> {
  const url = `${API_BASE}/elitea_core/public_applications/prompt_lib`;
  // `category` is the ONE filter the catalogue offers that a journey can name
  // its own row through, and it is the subject of its own cases: `Other` is a
  // catch-all that also matches a row carrying no category at all, so "is my
  // agent in this bucket" is a different question from "is my agent in the
  // catalogue" and both are asked here.
  const response = await request.get(
    url,
    category === undefined ? {} : { params: { category } },
  );
  if (!response.ok()) {
    throw new Error(
      `readCatalogue: GET ${url} -> ${response.status()}` +
        `${await describeRefusal(response)}\n${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as { rows?: readonly CatalogueRow[] };
  return body.rows ?? [];
}

/** The version write body, carrying only the keys the caller named. */
export function agentVersionBody(version: AgentVersionInput): Record<string, unknown> {
  const body: Record<string, unknown> = {};
  if (version.id !== undefined) body['id'] = version.id;
  if (version.applicationId !== undefined) body['application_id'] = version.applicationId;
  if (version.name !== undefined) body['name'] = version.name;
  if (version.agentType !== undefined) body['agent_type'] = version.agentType;
  if (version.instructions !== undefined) body['instructions'] = version.instructions;
  if (version.welcomeMessage !== undefined) body['welcome_message'] = version.welcomeMessage;
  if (version.conversationStarters !== undefined) {
    body['conversation_starters'] = [...version.conversationStarters];
  }
  if (version.variables !== undefined) {
    body['variables'] = version.variables.map((variable) => ({
      name: variable.name,
      value: variable.value,
    }));
  }
  if (version.tags !== undefined) {
    body['tags'] = version.tags.map((tag) => ({ name: tag.name, data: tag.data ?? {} }));
  }
  if (version.meta !== undefined) body['meta'] = { ...version.meta };
  if (version.model !== undefined) {
    const llm: Record<string, unknown> = { model_name: version.model.modelName };
    if (version.model.modelProjectId !== undefined) {
      llm['model_project_id'] = version.model.modelProjectId;
    }
    if (version.model.temperature !== undefined) llm['temperature'] = version.model.temperature;
    if (version.model.maxTokens !== undefined) llm['max_tokens'] = version.model.maxTokens;
    body['llm_settings'] = llm;
  }
  return body;
}

/**
 * Create an agent whose first version is exactly what the caller asked for.
 *
 * `createAgent` above hard-codes that version, which is right for the twenty
 * journeys that only need an agent to exist. This one is for the journeys
 * whose subject IS the version — the ones that seed a full `meta` and then
 * assert what an ordinary save did to it.
 */
export async function createAgentWithVersion(
  request: APIRequestContext,
  name: string,
  version: AgentVersionInput,
  projectId: string = DEFAULT_PROJECT_ID,
  description?: string,
): Promise<CreatedAgent> {
  const path = `/elitea_core/applications/prompt_lib/${projectId}`;
  const response = await request.post(`${API_BASE}${path}`, {
    data: {
      name,
      // The DESCRIPTION is a parameter because the pre-publish check reads it:
      // a description under 20 characters raises a warning attributed to the
      // agent it belongs to, and a fixture that always sent a long one could
      // not build the sub-agent a quality journey needs to be warned about.
      description: description ?? `${AUTOTEST_PREFIX}version contract fixture`,
      type: 'agent',
      versions: [agentVersionBody({ name: 'base', agentType: 'openai', ...version })],
    },
  });
  if (!response.ok()) {
    throw new Error(
      `createAgentWithVersion: POST ${path} -> ${response.status()}: ` +
        `${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as {
    id?: unknown;
    version_details?: { id?: unknown };
  };
  const id: unknown = body.id;
  const versionId: unknown = body.version_details?.id;
  if (typeof id !== 'string' || typeof versionId !== 'string') {
    throw new Error(
      `createAgentWithVersion: the create answered no id/version_details.id: ` +
        `${JSON.stringify(body).slice(0, 300)}`,
    );
  }
  return { id, versionId };
}

/** One version as `GET /version/...` serves it — the editor's own reload. */
export interface StoredVersionDetails {
  readonly id: string;
  readonly applicationId: string;
  readonly name: string;
  readonly status: string;
  readonly agentType: string;
  readonly instructions: string;
  readonly welcomeMessage: string;
  readonly llmSettings: Record<string, unknown>;
  /** The version's key bag: step_limit, icon_meta, variables, fork provenance. */
  readonly meta: Record<string, unknown>;
  readonly variables: readonly VersionVariableInput[];
  readonly tags: readonly { readonly id: string; readonly name: string }[];
  readonly tools: readonly Record<string, unknown>[];
  readonly authorId: string;
}

function versionDetailsFrom(body: Record<string, unknown>): StoredVersionDetails {
  const llm = (body['llm_settings'] as Record<string, unknown> | null) ?? {};
  const meta = (body['meta'] as Record<string, unknown> | null) ?? {};
  const variables = (body['variables'] as readonly Record<string, unknown>[] | null) ?? [];
  const tags = (body['tags'] as readonly Record<string, unknown>[] | null) ?? [];
  const tools = (body['tools'] as readonly Record<string, unknown>[] | null) ?? [];
  return {
    id: String(body['id'] ?? ''),
    applicationId: String(body['application_id'] ?? ''),
    name: String(body['name'] ?? ''),
    status: String(body['status'] ?? ''),
    agentType: String(body['agent_type'] ?? ''),
    instructions: String(body['instructions'] ?? ''),
    welcomeMessage: String(body['welcome_message'] ?? ''),
    llmSettings: llm,
    meta,
    variables: variables.map((row) => ({
      name: String(row['name'] ?? ''),
      value: String(row['value'] ?? ''),
    })),
    tags: tags.map((row) => ({ id: String(row['id'] ?? ''), name: String(row['name'] ?? '') })),
    tools,
    authorId: String(body['author_id'] ?? ''),
  };
}

/**
 * Read one version back the way the agent editor reloads it.
 *
 * Asserting on THIS and not on the write's own 201 echo is the point: the
 * echo is built in Go from the value the handler decided to send, so a write
 * that never reached the column answers a correct-looking echo. Only the read
 * goes back to the row.
 */
export async function readVersion(
  request: APIRequestContext,
  applicationId: string,
  versionId: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<StoredVersionDetails> {
  const url =
    `${API_BASE}/elitea_core/version/prompt_lib/${projectId}/${applicationId}/${versionId}`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readVersion: GET ${url} -> ${response.status()}${await describeRefusal(response)}\n` +
        `${(await response.text()).slice(0, 300)}`,
    );
  }
  return versionDetailsFrom((await response.json()) as Record<string, unknown>);
}

/** The vault key the expanded version-details route compares `X-SECRET` against. */
export const SECRETS_HEADER_NAME = 'secrets_header_value';

/**
 * The project's `secrets_header_value` as the vault holds it — or `undefined`
 * when it holds none. It NEVER writes.
 *
 * The read and the repair are separated because they answer different
 * questions, and one caller wants only the first. `auth.setup.ts` asserts that
 * the STACK gave the seeded project its value (`scripts/e2e-stack.sh seed`
 * restarts elitea-main so its backfill writes one); a read that quietly minted
 * the value on the way would make that assertion unable to fail, and the
 * absence it exists to catch would go on being repaired by whichever caller
 * happened to run first.
 *
 * An EMPTY value is not "absent": it is a vault that answered, with something
 * no caller can authenticate with. It throws rather than inviting a mint over
 * the top of it.
 */
export async function readProjectSecretHeader(
  request: APIRequestContext,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<string | undefined> {
  const url = `${API_BASE}/secrets/secret/default/${projectId}/${SECRETS_HEADER_NAME}`;
  const response = await request.get(url);
  if (response.status() === 404) return undefined;
  if (!response.ok()) {
    throw new Error(
      `readProjectSecretHeader: GET ${url} -> ${response.status()}${await describeRefusal(response)}`,
    );
  }
  const body = (await response.json()) as { value?: unknown };
  const value = typeof body.value === 'string' ? body.value : '';
  if (value === '') {
    throw new Error(`readProjectSecretHeader: the vault answered an empty ${SECRETS_HEADER_NAME}`);
  }
  return value;
}

/**
 * The project's `X-SECRET` value, resolved from the project's own vault —
 * minted through the API first if the project has none.
 *
 * The expanded version-details read is gated on this header, and the value is
 * per project and random — so a journey that wanted a literal would need one
 * configured per environment, and would silently start asserting "a wrong
 * header is refused" everywhere the literal went stale. Reading it makes the
 * journey portable and keeps the refusal case honest.
 *
 * WHY IT STILL MINTS ONE. elitea-main gives a project its value in exactly two
 * places: the project PROVISIONER, and a boot pass over `centry.project` that
 * creates the vault it needs. The journeys stack is reached by the second one
 * only because `scripts/e2e-stack.sh seed` RESTARTS elitea-main after writing
 * its project rows — the pass elitea-main did at `up` ran against a database
 * that had no `centry` schema yet. That is now the guarantee this suite runs
 * on, and `auth.setup.ts` asserts it.
 *
 * The mint is kept as the fallback for the deployments this fixture is also
 * pointed at and does not seed: a hand-built stack, a branch image whose seed
 * predates that restart, a project created after the pass. It is a repair, no
 * longer the ordinary path, and on the journeys stack it never fires.
 *
 * A 404 is therefore the ONLY status that mints. Anything else is reported as
 * itself: a 403 is a missing grant on the persona, and a 500 is a vault this
 * deployment can no longer open — neither is repaired by writing a new secret
 * over it.
 */
export async function resolveProjectSecretHeader(
  request: APIRequestContext,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<string> {
  const existing = await readProjectSecretHeader(request, projectId);
  if (existing !== undefined) return existing;

  await mintProjectSecretHeader(request, projectId);
  const minted = await readProjectSecretHeader(request, projectId);
  if (minted === undefined) {
    throw new Error(
      `resolveProjectSecretHeader: project ${projectId} holds no ${SECRETS_HEADER_NAME} and one ` +
        `could not be minted; elitea-main writes it when a project is provisioned and in a boot ` +
        `pass over centry.project, and this stack's seed restarts elitea-main to run that pass.`,
    );
  }
  return minted;
}

/**
 * Writes a `secrets_header_value` into the project's vault.
 *
 * The project-mode create route, which is the only one that serves this: the
 * per-name POST is administration-only and answers 405 here. A 400 is accepted
 * silently because it is what a concurrent worker's own mint looks like from
 * this side ("Secret … already exists"), and the caller re-reads either way.
 */
async function mintProjectSecretHeader(
  request: APIRequestContext,
  projectId: string,
): Promise<void> {
  const url = `${API_BASE}/secrets/secrets/default/${projectId}`;
  const response = await request.post(url, {
    // Never a fixed literal: this is the value the runtime authenticates with
    // for the rest of the run, and a constant checked into a public repository
    // would be one every deployment that ever ran this suite shared.
    data: { name: SECRETS_HEADER_NAME, value: `autotest${Date.now()}${Math.random().toString(36).slice(2, 10)}` },
  });
  if (response.status() === 201 || response.status() === 400) return;
  throw new Error(
    `resolveProjectSecretHeader: POST ${url} -> ${response.status()} while minting a ` +
      `${SECRETS_HEADER_NAME} for the project${await describeRefusal(response)}`,
  );
}

/**
 * The EXPANDED version details — the read the runtime and the SDK make.
 *
 * It is a PATCH on the version path, not a GET (the route is a read that
 * carries a header, and pylon shaped it that way), and it is gated on the
 * project's `X-SECRET`. Reading a version through it as well as through
 * `readVersion` is what says a stored value reaches the RUNTIME, not only the
 * editor: they are two different projections of the same row.
 */
export async function readVersionExpanded(
  request: APIRequestContext,
  applicationId: string,
  versionId: string,
  secretHeader: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<Record<string, unknown>> {
  const url =
    `${API_BASE}/elitea_core/version/prompt_lib/${projectId}/${applicationId}/${versionId}`;
  const response = await request.patch(url, { headers: { 'X-SECRET': secretHeader } });
  if (!response.ok()) {
    throw new Error(
      `readVersionExpanded: PATCH ${url} -> ${response.status()}: ` +
        `${(await response.text()).slice(0, 300)}`,
    );
  }
  return (await response.json()) as Record<string, unknown>;
}

/* ── sub-agent references ─────────────────────────────────────────────────── */

/**
 * The route that links one agent version into another as a SUB-AGENT.
 *
 * The URL names the CHILD and the body names the PARENT version — an order no
 * reader guesses, and the reason this is a helper rather than a line repeated
 * in each case: `internal/api/v2/eliteacore/application_relation.go` reads the
 * child's application and version out of the path and takes only
 * `version_id` from the body. `application_id` is sent because the route
 * refuses a body without it, and it is the CHILD's id there too.
 *
 * The response is returned rather than asserted, because the refusal cases need
 * it: a published parent refuses the change, and a pair already linked refuses
 * a second copy.
 */
export function attachSubAgent(
  request: APIRequestContext,
  parentVersionId: string,
  child: { readonly applicationId: string; readonly versionId: string },
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<APIResponse> {
  const url =
    `${API_BASE}/elitea_core/application_relation/prompt_lib/${projectId}/` +
    `${child.applicationId}/${child.versionId}`;
  return request.patch(url, {
    data: {
      application_id: Number(child.applicationId),
      version_id: Number(parentVersionId),
      has_relation: true,
    },
  });
}

/** The same route with `has_relation: false` — the detach half. */
export function detachSubAgent(
  request: APIRequestContext,
  parentVersionId: string,
  child: { readonly applicationId: string; readonly versionId: string },
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<APIResponse> {
  const url =
    `${API_BASE}/elitea_core/application_relation/prompt_lib/${projectId}/` +
    `${child.applicationId}/${child.versionId}`;
  return request.patch(url, {
    data: {
      application_id: Number(child.applicationId),
      version_id: Number(parentVersionId),
      has_relation: false,
    },
  });
}

/** One sub-agent entry as a version read serves it back. */
export interface SubAgentToolRow {
  /** The CHILD agent this entry points at. */
  readonly applicationId: string;
  /** The exact child VERSION this entry points at. */
  readonly versionId: string;
  readonly name: string;
}

/**
 * The sub-agent entries of a `tools` array, whichever projection served it.
 *
 * Three routes serve the same reference under three shapes — the editor read
 * puts the stored blob under `config`, the expanded read and the public detail
 * put it under `settings` — so a caller that read one key would silently find
 * no sub-agents on the other two. Non-application tools are dropped: every
 * caller here is asking "which agents does this version delegate to".
 */
export function subAgentToolsOf(tools: readonly unknown[]): readonly SubAgentToolRow[] {
  return tools
    .map((entry) => (entry ?? {}) as Record<string, unknown>)
    .filter((tool) => tool['type'] === 'application')
    .map((tool) => {
      const settings =
        ((tool['settings'] ?? tool['config']) as Record<string, unknown> | undefined) ?? {};
      return {
        applicationId: String(settings['application_id'] ?? ''),
        versionId: String(settings['application_version_id'] ?? settings['version_id'] ?? ''),
        name: String(tool['name'] ?? ''),
      };
    });
}

/** One row of the project's tag list. */
export interface StoredTag {
  readonly id: string;
  readonly name: string;
  /** The free-form blob the tag carries — the colour the rail draws it in. */
  readonly data: unknown;
}

/**
 * Which entities' tags to read.
 *
 * A project's `tags` table is shared by everything that can be tagged, so
 * "the tags in this project" and "the tags I can filter agents by" are
 * different sets. `all` — the default — is every row, including one that
 * nothing carries yet, which is the only coverage a freshly CREATED tag
 * appears in.
 */
export type TagCoverage = 'all' | 'application' | 'pipeline' | 'skill';

/** How to read the project's tags. */
export interface ReadTagsOptions {
  readonly coverage?: TagCoverage;
  readonly projectId?: string;
}

/**
 * The project's tags, as the tag control reads them.
 *
 * The list is filtered by NAME by the caller rather than paged: this reads
 * the whole set because the caller matches its own `autotest_` names out of
 * it, and the route offers no name filter to narrow with. It does offer
 * `entity_coverage`, which narrows by the KIND of entity carrying the tag,
 * and that is what `options.coverage` sends.
 */
export async function readTags(
  request: APIRequestContext,
  options: ReadTagsOptions = {},
): Promise<readonly StoredTag[]> {
  const projectId = options.projectId ?? DEFAULT_PROJECT_ID;
  const url = `${API_BASE}/elitea_core/tags/prompt_lib/${projectId}`;
  const response = await request.get(url, {
    params: options.coverage === undefined ? {} : { entity_coverage: options.coverage },
  });
  if (!response.ok()) {
    throw new Error(
      `readTags: GET ${url} -> ${response.status()}: ${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as {
    rows?: readonly Record<string, unknown>[];
    total?: unknown;
  };
  return (body.rows ?? []).map((row) => ({
    id: String(row['id'] ?? ''),
    name: String(row['name'] ?? ''),
    data: row['data'] ?? null,
  }));
}

/** The whole tag-list response, for the journey whose subject is the envelope. */
export async function readTagsEnvelope(
  request: APIRequestContext,
  options: ReadTagsOptions = {},
): Promise<Record<string, unknown>> {
  const projectId = options.projectId ?? DEFAULT_PROJECT_ID;
  const url = `${API_BASE}/elitea_core/tags/prompt_lib/${projectId}`;
  const response = await request.get(url, {
    params: options.coverage === undefined ? {} : { entity_coverage: options.coverage },
  });
  if (!response.ok()) {
    throw new Error(
      `readTagsEnvelope: GET ${url} -> ${response.status()}: ` +
        `${(await response.text()).slice(0, 300)}`,
    );
  }
  return (await response.json()) as Record<string, unknown>;
}

/**
 * Create a tag through the tag write API and answer the STORED row.
 *
 * Idempotent on the name, which is what the server is: one name is one row
 * per project, shared by every version that carries it.
 */
export async function createTag(
  request: APIRequestContext,
  name: string,
  data?: Readonly<Record<string, unknown>>,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<StoredTag> {
  const url = `${API_BASE}/elitea_core/tags/prompt_lib/${projectId}`;
  const response = await request.post(url, {
    data: data === undefined ? { name } : { name, data },
  });
  if (!response.ok()) {
    throw new Error(
      `createTag: POST ${url} -> ${response.status()}: ${(await response.text()).slice(0, 300)}`,
    );
  }
  const row = (await response.json()) as Record<string, unknown>;
  return {
    id: String(row['id'] ?? ''),
    name: String(row['name'] ?? ''),
    data: row['data'] ?? null,
  };
}

/**
 * Delete a tag by id. Answers the status, so a cleanup can stay quiet and a
 * journey about the delete can assert on it.
 */
export async function deleteTag(
  request: APIRequestContext,
  tagId: string,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<number> {
  const response = await request.delete(
    `${API_BASE}/elitea_core/tags/prompt_lib/${projectId}/${tagId}`,
  );
  return response.status();
}

/**
 * Remove every tag the project holds whose name starts with `autotest_`.
 *
 * A tag row outlives the agent that carried it — deleting an agent takes the
 * ASSOCIATION and leaves the row — so a journey that saves a tag has to
 * remove the row itself or leave one behind on every run.
 */
export async function deleteAutotestTags(
  request: APIRequestContext,
  names: readonly string[],
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<void> {
  const wanted = new Set(names);
  for (const tag of await readTags(request, { projectId })) {
    if (wanted.has(tag.name) && tag.id !== '') {
      await deleteTag(request, tag.id, projectId);
    }
  }
}

/** One row of the agents list, as `GET /applications/...` serves it. */
export interface ListedAgent extends Record<string, unknown> {
  readonly id?: string;
  readonly name?: string;
}

/** The agents-list envelope: `{rows, total, page, page_size, total_pages}`. */
export interface AgentListEnvelope {
  readonly rows: readonly ListedAgent[];
  readonly body: Record<string, unknown>;
}

/**
 * The agents list, NAMING what it wants.
 *
 * `query` is the list's own name filter, so a journey finds its own agents
 * instead of asking for the first page of everything and hoping they are on
 * it — the shape rule 2 of scripts/e2e-journey-shape.test.mjs states, and the
 * one a suite that creates rows in parallel needs anyway.
 */
export async function readAgentList(
  request: APIRequestContext,
  options: {
    readonly query?: string;
    readonly limit?: number;
    readonly agentsType?: string;
    readonly projectId?: string;
  } = {},
): Promise<AgentListEnvelope> {
  const projectId = options.projectId ?? DEFAULT_PROJECT_ID;
  const url = `${API_BASE}/elitea_core/applications/prompt_lib/${projectId}`;
  const params: Record<string, string | number> = { limit: options.limit ?? 20 };
  if (options.query !== undefined) params['query'] = options.query;
  if (options.agentsType !== undefined) params['agents_type'] = options.agentsType;
  const response = await request.get(url, { params });
  if (!response.ok()) {
    throw new Error(
      `readAgentList: GET ${url} -> ${response.status()}` +
        `${await describeRefusal(response)}\n${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as Record<string, unknown>;
  const rows = (body['rows'] as readonly ListedAgent[] | undefined) ?? [];
  return { rows, body };
}

/**
 * The author profile: identity plus the counters the profile page shows.
 *
 * `GET /elitea_core/author/prompt_lib/{author}` is a different route from
 * `/social/author`, which answers the CALLER. This one answers any author by
 * id, and counts the agents, pipelines and toolkits they own across every
 * project they belong to.
 */
export async function readAuthorProfile(
  request: APIRequestContext,
  authorId: string,
): Promise<Record<string, unknown>> {
  const url = `${API_BASE}/elitea_core/author/prompt_lib/${authorId}`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readAuthorProfile: GET ${url} -> ${response.status()}` +
        `${await describeRefusal(response)}\n${(await response.text()).slice(0, 300)}`,
    );
  }
  return (await response.json()) as Record<string, unknown>;
}

/** The signed-in caller, as `/social/author` answers it. */
export async function readCallerIdentity(
  request: APIRequestContext,
): Promise<{ readonly id: string; readonly email: string }> {
  const url = `${API_BASE}/social/author/`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readCallerIdentity: GET ${url} -> ${response.status()}${await describeRefusal(response)}`,
    );
  }
  const body = (await response.json()) as { id?: unknown; email?: unknown };
  return { id: String(body.id ?? ''), email: String(body.email ?? '') };
}

/**
 * The caller's OWN project — `project_user_<uid>` — as `/social/author` names it.
 *
 * Wanted by the journeys whose subject is a rule that DISCRIMINATES between the
 * public project and any other one. The server's public project is resolved
 * from `ELITEA_AI_PROJECT_ID` and defaults to 1, which is also the project
 * every seeded persona works in, so a rule of the form "only entities that live
 * in the public project may do X" is satisfied by everything a journey creates
 * in the ordinary way and its refusing half is unreachable. The caller's own
 * personal project is a second REAL project, owned by the same persona, and it
 * is the one place a journey can put an entity that the rule must refuse.
 *
 * `''` is answered rather than thrown when the server names the seeded project:
 * `resolvePersonalProjectID` falls back to the lowest-id project the caller
 * holds any role in, so project 1 comes back for a persona whose own project
 * was never provisioned. That answer is an absence, not an id — see
 * `e2e/auth.setup.ts`, which waits for a real one before any journey runs — and
 * a caller that treated it as one would create its "not published" fixture in
 * the very project the rule calls published.
 */
export async function readCallerPersonalProjectId(request: APIRequestContext): Promise<string> {
  const url = `${API_BASE}/social/author/`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readCallerPersonalProjectId: GET ${url} -> ${response.status()}${await describeRefusal(response)}`,
    );
  }
  const body = (await response.json()) as { personal_project_id?: unknown };
  const id = String(body.personal_project_id ?? '');
  return id === DEFAULT_PROJECT_ID ? '' : id;
}

/** What one sweep actually did, so a caller can assert on the work and not on the silence. */
export interface AutotestSweepReport {
  /** Rows deleted, per kind. */
  readonly conversations: number;
  readonly agents: number;
  readonly pipelines: number;
  /**
   * Reads and deletes that failed, as one line each.
   *
   * A sweep used to swallow every failure and return `void`, so a LIST that
   * answered 500 and a project that was already clean were the same
   * observation. They are not: the first leaves every row behind and reports
   * success. The per-row deletes stay best effort — a teardown must not fail
   * the run over one stubborn row — but the failure is now countable, and
   * `api.fixture-isolation.spec.ts` asserts it is empty.
   */
  readonly failures: readonly string[];
}

/** One page of a `prompt_lib` list route, whatever envelope it uses. */
async function listPage(
  request: APIRequestContext,
  url: string,
): Promise<readonly { id?: string; name?: string }[]> {
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(`GET ${url} -> ${response.status()} ${response.statusText()}`);
  }
  const body = (await response.json()) as {
    rows?: readonly { id?: string; name?: string }[];
    items?: readonly { id?: string; name?: string }[];
  };
  return body.rows ?? body.items ?? [];
}

/**
 * Every row of a list route, PAGED.
 *
 * The applications route clamps `limit` to 100 and silently falls back to its
 * default of 20 for anything outside that range
 * (`internal/api/v2/applications/handler.go`), so `?limit=200` asks for two
 * hundred rows and receives twenty — a sweep written that way stops after the
 * first page and reports success. That is the same shape as #544, where a
 * paged read walked off the end of a list and the missing row read as lost
 * work.
 */
async function listAll(
  request: APIRequestContext,
  base: string,
): Promise<readonly { id?: string; name?: string }[]> {
  const pageSize = 100;
  const all: { id?: string; name?: string }[] = [];
  for (let offset = 0; offset < 5_000; offset += pageSize) {
    const separator = base.includes('?') ? '&' : '?';
    const page = await listPage(request, `${base}${separator}limit=${pageSize}&offset=${offset}`);
    all.push(...page);
    if (page.length < pageSize) break;
  }
  return all;
}

/**
 * Sweep every leftover `autotest_*` entity out of the shared project.
 *
 * Scoped by NAME, and only by name: a sweep that ignored the prefix would
 * destroy the seeded rows every journey depends on.
 *
 * ## The two things it used to miss, silently
 *
 *  1. **Pipelines.** `GET .../applications/prompt_lib/{project}` INNER JOINs
 *     `application_versions` on `agent_type != 'pipeline'` unless the caller
 *     sends `agents_type=pipeline` (`internal/infra/db/repos/applications.go`),
 *     so the classic listing NEVER carries a pipeline. Every
 *     `autotest_`-named pipeline survived every sweep, and the sweep said it
 *     was done.
 *  2. **Everything past row twenty**, per the `limit` clamp `listAll` above
 *     documents.
 *
 * Both are the class of defect this whole file's callers keep paying for:
 * absence read as correctness. The report it now returns is what makes the
 * difference assertable.
 */
export async function sweepAutotestEntities(
  request: APIRequestContext,
): Promise<AutotestSweepReport> {
  const failures: string[] = [];
  let conversations = 0;
  let agents = 0;
  let pipelines = 0;

  const named = (rows: readonly { id?: string; name?: string }[]) =>
    rows.filter((row) => (row.name ?? '').startsWith(AUTOTEST_PREFIX));

  /*
   * The DELETE, with its status read.
   *
   * `deleteAgent`/`deleteConversation` above are deliberately best effort and
   * ignore the status — an `afterEach` cleaning up after a failing test must
   * not fail it a second time. A SWEEP is the opposite case: its whole claim
   * is that the rows are gone, so a 403 or a 500 has to be counted or the
   * report says "swept 0 failures" about a project it never touched. 404 is
   * not a failure: another journey's own teardown may have taken the row
   * between the list and the delete.
   */
  const removeRow = async (kind: string, path: string, id: string): Promise<boolean> => {
    const response = await request.delete(`${API_BASE}/${path}/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`);
    if (response.ok() || response.status() === 404) return response.ok();
    failures.push(`${kind} ${id}: ${response.status()} ${(await response.text()).slice(0, 120)}`);
    return false;
  };

  try {
    const rows = named(
      await listAll(request, `${API_BASE}/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}`),
    );
    for (const row of rows) {
      if (row.id === undefined) continue;
      if (await removeRow('conversation', 'elitea_core/conversation', row.id)) conversations += 1;
    }
  } catch (error) {
    failures.push(`list conversations: ${String(error)}`);
  }

  // Classic agents and pipelines are two different listings of one table, and
  // the sweep needs both. See this function's own note.
  for (const [kind, query] of [
    ['agent', ''],
    ['pipeline', '?agents_type=pipeline'],
  ] as const) {
    try {
      const rows = named(
        await listAll(
          request,
          `${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}${query}`,
        ),
      );
      for (const row of rows) {
        if (row.id === undefined) continue;
        if (!(await removeRow(kind, 'elitea_core/application', row.id))) continue;
        if (kind === 'agent') agents += 1;
        else pipelines += 1;
      }
    } catch (error) {
      failures.push(`list ${kind}s: ${String(error)}`);
    }
  }

  return { conversations, agents, pipelines, failures };
}

/* ────────────────────────────────────────────────────────────────────────────
 * The streaming journeys' shared steps
 *
 * `e2e/streaming/chat.*.spec.ts` each drive a full agent turn against the full
 * standalone stack, and each used to hand-roll the same two blocks. They are
 * here, once, because both are load-bearing in ways their call sites cannot
 * see: the form flow encodes what the FORM seeds (the whole reason those
 * journeys exist), and the stored-answer poll encodes the one observation that
 * can tell an answer from a refusal. A copy that drifts out of one spec is a
 * spec that quietly stops discriminating.
 * ──────────────────────────────────────────────────────────────────────────── */

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const APPLICATIONS_RE = /\/elitea_core\/applications\/prompt_lib\/(\d+)$/;

/**
 * The longest agent name that survives the create form.
 *
 * `CreateAgentForm` caps the name input at 32 characters
 * (`features/agents/lib/helpers/agentDraftValidation.helpers.ts`), and
 * `fill()` respects `maxLength` — so a longer name is silently truncated and
 * every later lookup by that name finds nothing, with a failure that reads
 * like the agent was never created. `createAgentThroughForm` asserts it rather
 * than assuming it, because the failure mode is invisible at the call site.
 */
const MAX_AGENT_NAME = 32;

/** What the create-agent FORM actually produced, read back off its own response. */
export interface AgentCreatedThroughForm {
  /** The project the form wrote into — read from the request, never assumed to be 1. */
  readonly projectId: string;
  /** `applications.id` — the key every later route addresses the agent by. */
  readonly agentId: string;
  /** `application_versions.id` — what the agent resolver joins the participant on. */
  readonly versionId: string;
  /** `version_details.meta.internal_tools` exactly as the SERVER stored it. */
  readonly internalTools: readonly string[];
}

/**
 * Author an agent by filling in the create form, and hand back what the server
 * stored.
 *
 * Through the FORM, not through `createAgent()` above, because the defects
 * these journeys exist for were in what the form SEEDS — an
 * `internal_tools: ['internal_mcp']` the agent resolver refuses, so the join
 * produced no row and every send answered 422 about an "execution path". A
 * version created by the API fixture carries whatever that fixture types; only
 * the form carries what a user actually gets.
 *
 * Saved with NO instructions, deliberately. The form does not require the
 * field, so this is the agent a user gets by filling in only what is marked
 * required — and it is the shape the native runtime used to refuse, answering
 * "The execution input is invalid."
 * (`services/elitea-worker-rust/src/agents/assembly.rs::bounded_instruction`).
 * Typing into the instructions editor would also couple these journeys to
 * which code editor the form embeds, which is not what they assert.
 *
 * Leaves the browser on the agent's edit page, which is where the save lands
 * and where the Tools panel and the Chat button live.
 */
export async function createAgentThroughForm(
  page: Page,
  name: string,
): Promise<AgentCreatedThroughForm> {
  expect(name.length, 'the agent name must survive the form’s 32-char cap').toBeLessThanOrEqual(
    MAX_AGENT_NAME,
  );

  // Armed BEFORE the navigation: the response is what carries the project id
  // and the stored version, so the assertions below read what the server
  // actually wrote rather than what the test constructed.
  const created = page.waitForResponse(
    (r) => APPLICATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );

  await page.goto(`${BASE_URL}/app/agents/create`);
  await expect(page.getByTestId('agent-name-input')).toBeVisible({ timeout: 30_000 });
  await page.getByTestId('agent-name-input').fill(name);
  await page.getByTestId('agent-description-input').fill(`${name} description`);
  await expect(page.getByTestId('agent-save-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('agent-save-button').click();

  const response = await created;
  expect(
    response.status(),
    `the agent must be created: ${(await response.text()).slice(0, 300)}`,
  ).toBe(201);

  const projectId = APPLICATIONS_RE.exec(new URL(response.url()).pathname)?.[1] ?? '';
  expect(projectId, 'the agent must belong to a project').not.toBe('');

  const body = (await response.json()) as {
    id?: string;
    version_details?: { id?: string; meta?: { internal_tools?: readonly string[] } };
  };
  const agentId = body.id ?? '';
  expect(agentId, 'the created agent must carry an id').toMatch(/^\d+$/);
  const versionId = String(body.version_details?.id ?? '');
  expect(versionId, 'the created agent must carry a version, or the resolver joins nothing').not.toBe('');

  return {
    projectId,
    agentId,
    versionId,
    internalTools: body.version_details?.meta?.internal_tools ?? [],
  };
}

export interface StoredAssistantAnswerOptions {
  /** How long the store is given to finalise the row. Default 60s. */
  readonly timeout?: number;
  /** The failure line — say what a miss MEANS for this journey. */
  readonly message?: string;
  /**
   * Text the stored answer must contain, checked INSIDE the poll.
   *
   * The offline mock echoes the prompt, which is what proves the answer
   * belongs to this run rather than to a cached or misrouted one. A real model
   * echoes nothing, so a journey driven against one leaves this unset and
   * settles for "the turn produced text".
   */
  readonly contains?: string;
}

/**
 * Require a real assistant answer in the STORE for this conversation.
 *
 * Polled, not read once: the stream runs ahead of the store. Measured — a read
 * issued the moment the UI settled returned the assistant row with
 * `content: ""`, and the same conversation carried the full text moments
 * later, so a single read failed against a backend that was merely still
 * writing.
 *
 * And polled until the row is FINISHED, not merely non-empty. The same lag
 * that makes a single read too early makes "some text is stored" too early in
 * the other direction: the row grows for the whole run, so a poll that stops
 * at the first byte returns while the turn is still being written and leaves
 * it running behind the spec that started it. The `is_error` PRESENCE test
 * inside the poll is what ends the turn here; its note carries the
 * measurements.
 *
 * The STORED reply, not the on-screen bubble, and that is the discriminating
 * part: a refused turn renders its failure AS an assistant card, so "some text
 * appeared" cannot tell an answer from a refusal — one of these specs' own
 * first drafts went green against exactly that card. Only a turn the runtime
 * actually completed finalizes a non-empty assistant row that is not flagged
 * `metadata.is_error`. The `IS_ERROR:` prefix below carries that flag through
 * the poll's string value so the failure names the refusal instead of
 * reporting an empty answer.
 */
export async function expectStoredAssistantAnswer(
  page: Page,
  projectId: string,
  conversationId: string | number,
  options: StoredAssistantAnswerOptions = {},
): Promise<void> {
  const {
    timeout = 60_000,
    message = 'the assistant reply was streamed but never stored',
    contains,
  } = options;

  const pattern =
    contains === undefined
      ? /^(?!IS_ERROR:)[\s\S]+$/
      : new RegExp(`^(?!IS_ERROR:)[\\s\\S]*${escapeForRegExp(contains)}[\\s\\S]*$`);

  await expect
    .poll(
      async () => {
        const stored = await page.request.get(
          `${BASE_URL}/api/v2/elitea_core/messages/prompt_lib/${projectId}/${String(conversationId)}`,
        );
        if (!stored.ok()) return '';
        const body = (await stored.json()) as {
          items?: readonly { role?: string; content?: string; metadata?: { is_error?: boolean } }[];
        };
        const assistant = body.items?.find((item) => item.role === 'assistant');
        if (assistant?.metadata?.is_error === true) return `IS_ERROR:${assistant.content ?? ''}`;
        // NOT YET FINISHED. `is_error` is written ONLY by a terminal
        // projection — FinalizeCurrentAgentFullMessage and its HITL and
        // authorization twins each set it explicitly
        // (services/elitea-main/internal/db/queries/agent_chat.sql) — while a
        // row that is still streaming carries only `execution_generation`. An
        // ABSENT key therefore means "the run has not landed", and reading it
        // as "not an error" is what let this helper return mid-turn.
        //
        // WHAT THAT COST, measured on the standalone stack rather than
        // reasoned about. The stream runs ahead of the store, so content
        // appears within a second or two and this poll settled there: in one
        // run chat.agent-tools passed in 2.3s while ITS OWN turn kept writing
        // for another 37s, and chat.nested-agent passed in 1.9s while its turn
        // ran 12s more. Every spec then started its turn on top of the
        // previous specs' unfinished ones, against a worker that admits TWO
        // invocations at a time (`delivery_max_concurrency` in
        // deploy/runtime/worker-runtime.json). Later turns queued: one
        // chat.pipeline turn took 116s and its spec failed after 60s of
        // polling for the graph's trace steps, on a run where four turns were
        // in flight at once. Nothing about that failure is a statement about
        // pipelines.
        //
        // Requiring the flag to be PRESENT is what makes this helper's own
        // sentence above true — "only a turn the runtime actually completed
        // finalizes a non-empty assistant row that is not flagged is_error".
        // It is deliberately not "poll until the text stops growing": an agent
        // that pauses between tool calls looks settled to that rule, and this
        // one cannot be fooled by a gap in the tokens.
        //
        // `readStoredAssistantAnswer` keeps the old, weaker read on purpose —
        // chat.stop MUST be able to sample a half-written row.
        if (assistant?.metadata?.is_error !== false) return '';
        return assistant.content ?? '';
      },
      { timeout, message },
    )
    .toMatch(pattern);
}

/** Escape a literal so `contains` above can be embedded in a RegExp unchanged. */
function escapeForRegExp(literal: string): string {
  return literal.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

export interface StoredTurnRefusalOptions {
  /** How long the store is given to finalise the row. Default 60s. */
  readonly timeout?: number;
  /** The failure line — say what a miss MEANS for this journey. */
  readonly message?: string;
}

/**
 * The negative twin of `expectStoredAssistantAnswer`: require the STORED
 * assistant row for this turn to be flagged `metadata.is_error === true`.
 *
 * For a turn the admission gates forward but the runtime cannot execute — a
 * profile it does not support, not one it finds malformed — the START POST
 * still answers 200 and a stream still opens; the refusal is decided later
 * and lands as an assistant row carrying `metadata.is_error: true` rather
 * than as any status code a Playwright `waitForResponse` could see. That is
 * exactly the shape `chat.agent-tools.spec.ts`'s own header comment measured
 * for an unsupported internal tool, and it is the shape
 * `chat.attachments.spec.ts` measures for an unserved attachment — see those
 * files' headers for the runtime source each one pins.
 *
 * (`chat.variables.spec.ts` used to be the other example here. It is not any
 * more: the native runtime SUBSTITUTES a populated `meta.variables` now, so
 * that journey asserts a served answer and reads the substituted prompt back
 * out of the mock's model journal.)
 *
 * Polled, not read once, for the same reason `expectStoredAssistantAnswer`
 * is: the stream settles in the browser before the store finalises the row,
 * so a single read can land between the row's creation and the write that
 * flags it, and report "not an error" about a row that is still being
 * written rather than about the turn's real outcome.
 *
 * A caller that wants the refused row's own text (usually empty — see the
 * `chat.agent-tools.spec.ts` note above) should read it separately with
 * `readStoredTranscript`; this helper only answers the yes/no question its
 * name asks.
 */
export async function expectStoredTurnRefusal(
  page: Page,
  projectId: string,
  conversationId: string | number,
  options: StoredTurnRefusalOptions = {},
): Promise<void> {
  const {
    timeout = 60_000,
    message = 'the turn was admitted but the runtime never stored a refusal for it',
  } = options;

  await expect
    .poll(
      async () => {
        const stored = await page.request.get(
          `${BASE_URL}/api/v2/elitea_core/messages/prompt_lib/${projectId}/${String(conversationId)}`,
        );
        if (!stored.ok()) return false;
        const body = (await stored.json()) as {
          items?: readonly { role?: string; metadata?: { is_error?: boolean } }[];
        };
        const assistant = body.items?.find((item) => item.role === 'assistant');
        return assistant?.metadata?.is_error === true;
      },
      { timeout, message },
    )
    .toBe(true);
}

/** One persisted `chat_message_group`, as the transcript route serves it. */
export interface StoredTranscriptRow {
  /** `chat_message_group.id` — the numeric key. */
  readonly id: string;
  /** `chat_message_group.uuid` — the identity a regeneration REUSES (see `chat.regenerate.spec.ts`). */
  readonly uid: string;
  /** `user` for a question, `assistant` for every other author (the route maps `entity_name` this way). */
  readonly role: string;
  /** `string_agg` of the group's `text_message` items, in `order_index` order. */
  readonly content: string;
  /** `metadata.is_error` — a refused turn is STORED as an assistant row, so this is what tells an answer from a refusal. */
  readonly isError: boolean;
  /**
   * `chat_message_group.meta`, verbatim.
   *
   * Carried because one of its keys is the only witness a regeneration leaves:
   * `execution_generation` is rewritten to the run's own id by
   * `ResetCurrentAgentResponse`, while the row keeps its `id`, `uid` and
   * `created_at`. Row identity alone cannot tell "re-ran in place" from "left
   * untouched"; this can.
   */
  readonly metadata: Readonly<Record<string, unknown>>;
}

/**
 * The whole stored transcript of one conversation, oldest row first.
 *
 * `expectStoredAssistantAnswer` above answers "did the newest answer land";
 * this answers "what is the conversation now", which is the only way to state
 * that a second turn ACCUMULATED (`chat.multiturn.spec.ts`) or that a
 * regeneration REPLACED rather than appended (`chat.regenerate.spec.ts`).
 *
 * `sort_order=asc` is not cosmetic. The route's documented default is
 * `created_at DESC` (`parseMessagesQuery`, #603), so a caller that omits it
 * gets the transcript BACKWARDS and an "in order" assertion written against it
 * passes on a reversed conversation. `limit` is explicit for the same class of
 * reason: the default window is 50 groups, which is silently a filter.
 *
 * A non-2xx THROWS, naming the status and body. Returning `[]` would make a
 * broken route and an empty conversation indistinguishable — the exact shape
 * defect #599 took inside this very endpoint, where a failing query answered a
 * successful empty transcript and nobody saw a failure to investigate.
 */
export async function readStoredTranscript(
  page: Page,
  projectId: string,
  conversationId: string | number,
): Promise<readonly StoredTranscriptRow[]> {
  const url =
    `${BASE_URL}/api/v2/elitea_core/messages/prompt_lib/${projectId}/${String(conversationId)}` +
    '?sort_order=asc&limit=100';
  const response = await page.request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readStoredTranscript: GET ${url} -> ${response.status()} ${response.statusText()}` +
      `${await describeRefusal(response)}\n` +
      `${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as {
    items?: readonly {
      id?: string;
      uid?: string;
      role?: string;
      content?: string;
      metadata?: Record<string, unknown>;
    }[];
  };
  return (body.items ?? []).map((item) => ({
    id: String(item.id ?? ''),
    uid: String(item.uid ?? ''),
    role: item.role ?? '',
    content: item.content ?? '',
    isError: item.metadata?.['is_error'] === true,
    metadata: item.metadata ?? {},
  }));
}

/** The newest stored assistant row, as `chat.stop.spec.ts` measures it. */
export interface StoredAssistantAnswer {
  /** `true` when the conversation holds an assistant row at all. */
  readonly found: boolean;
  /** The row's concatenated text. Empty while the store is still filling. */
  readonly content: string;
  /** `metadata.is_error` — a refused turn is stored AS an assistant row. */
  readonly isError: boolean;
}

/**
 * READ the newest stored assistant row. Does not assert anything about it.
 *
 * `expectStoredAssistantAnswer` above answers a yes/no question and swallows
 * the value inside its own poll, which is exactly what a growth measurement
 * cannot use: proving a cancelled turn STOPPED writing needs the same field
 * read twice, a gap apart, and the two lengths compared. A second
 * `expect.poll` cannot express that — a poll that waits for two equal reads
 * also passes on a stream that has merely paused between chunks, and one that
 * waits for a stable value passes trivially the moment the turn finishes on
 * its own.
 *
 * The newest row, not the first in document order: the route's documented
 * default sort is `created_at DESC` (#603), so `items[0]` of role `assistant`
 * is the reply to the most recent question. `expectStoredAssistantAnswer`
 * reads the same route the same way, so the two cannot disagree about which
 * row they are talking about.
 *
 * A non-2xx returns `found: false` rather than throwing: the caller polls this
 * while a turn is still being admitted, and the transcript route can answer
 * before the response row exists. The CALLER's poll message says what a
 * permanent miss means for its own journey.
 */
export async function readStoredAssistantAnswer(
  page: Page,
  projectId: string,
  conversationId: string | number,
): Promise<StoredAssistantAnswer> {
  const response = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/messages/prompt_lib/${projectId}/${String(conversationId)}`,
  );
  if (!response.ok()) return { found: false, content: '', isError: false };
  const body = (await response.json()) as {
    items?: readonly { role?: string; content?: string; metadata?: { is_error?: boolean } }[];
  };
  const assistant = (body.items ?? []).find((item) => item.role === 'assistant');
  if (assistant === undefined) return { found: false, content: '', isError: false };
  return {
    found: true,
    content: assistant.content ?? '',
    isError: assistant.metadata?.is_error === true,
  };
}

/** One `chat_message_items` row as the conversation-details route serves it. */
export interface StoredMessageItem {
  /**
   * `chat_message_items.id` — the numeric key.
   *
   * Carried because it is the only handle on ONE item of a message: the canvas
   * create route takes `message_item_id` and splits exactly that row, and a
   * caller that could only name the group would have to guess which of its
   * items it meant.
   */
  readonly id: string;
  /** `chat_message_items.item_type` — `text_message`, `attachment_message`, … */
  readonly itemType: string;
  /**
   * The item's type-specific payload, verbatim.
   *
   * `{content}` for a text item; for an `attachment_message` the seven keys
   * `attachmentItemDetails` emits (#606) — `id`, `item_type`, `name`, `bucket`,
   * `filepath`, `attachment_type`, `content` — where `content` is a DECODED
   * array of LangChain content chunks, never a string.
   */
  readonly details: Readonly<Record<string, unknown>>;
}

/** One `chat_message_group` with its items, as the details route embeds it. */
export interface StoredMessageGroup {
  readonly id: string;
  readonly uuid: string;
  /** `string_agg` of the group's `text_message` items ALONE — attachments are not spliced in. */
  readonly content: string;
  readonly items: readonly StoredMessageItem[];
  /**
   * `chat_message_group.author_participant_id` — WHO wrote this group.
   *
   * The only field that says which participant answered, and the reason it is
   * carried: in a conversation holding more than one addressable participant
   * every reply looks alike from its text (the offline mock echoes the
   * question), so "the participant this turn addressed is the one that
   * replied" cannot be stated any other way. The admission writes it from the
   * question's `sent_to_id` in the same statement
   * (`InsertCurrentAdhocTurn` / `InsertCurrentApplicationTurn`), so a turn
   * routed to the wrong participant differs HERE and nowhere else.
   */
  readonly authorParticipantId: string;
  /** `sent_to_id` — the participant a QUESTION group was addressed to. */
  readonly sentToId: string;
  /** `reply_to_id` — the question group an ANSWER group belongs to. */
  readonly replyToId: string;
}

/**
 * The conversation's message GROUPS, with every item's `item_details`.
 *
 * A different route and a different projection from `readStoredTranscript`
 * above, and that difference is the whole reason this exists. The transcript
 * route (`/elitea_core/messages/...`) collapses each group to ONE `content`
 * string built from its `text_message` items only — deliberately, so that an
 * attachment's payload cannot be spliced into a message's rendered text (see
 * the `#606` note above the backward-compat loop in
 * `services/elitea-main/internal/infra/db/repos/conversations.go`). An
 * attachment is therefore structurally invisible there, and a spec asserting
 * "this turn carried a file" against that route would be asserting against a
 * projection that cannot answer the question either way.
 *
 * The items live on `GET /elitea_core/conversation/prompt_lib/{p}/{c}`
 * instead, and ONLY when `messages_limit` is supplied: the handler embeds
 * `message_groups` exactly then (`api/v2/conversations/handler.go`), so
 * omitting the parameter yields a well-formed 200 carrying no groups at all —
 * which reads as "this conversation has no messages" rather than as a caller
 * mistake. It is always sent here and never left to a default.
 *
 * `sort_order=asc` for the reason `readStoredTranscript` sends it: the
 * repository interpolates the order into `ORDER BY mg.created_at` and the
 * default is DESC, so an "in order" reading of the result would be backwards.
 *
 * A non-2xx THROWS, naming the status and body. `ListMessageGroups` already
 * answers its OWN query failures with an empty slice rather than an error, so
 * a broken query reaches a client as a successful empty transcript; swallowing
 * a transport failure here as well would leave a caller unable to tell a
 * missing attachment from a route that never ran.
 */
export async function readStoredMessageGroups(
  page: Page,
  projectId: string,
  conversationId: string | number,
  limit = 50,
): Promise<readonly StoredMessageGroup[]> {
  const url =
    `${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${String(conversationId)}` +
    `?messages_limit=${String(limit)}&sort_order=asc`;
  const response = await page.request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readStoredMessageGroups: GET ${url} -> ${response.status()} ${response.statusText()}` +
      `${await describeRefusal(response)}\n` +
      `${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as {
    message_groups?: readonly {
      id?: unknown;
      uuid?: unknown;
      content?: unknown;
      author_participant_id?: unknown;
      sent_to_id?: unknown;
      reply_to_id?: unknown;
      message_items?: readonly { id?: unknown; item_type?: unknown; item_details?: unknown }[];
    }[];
  };
  // An absent id becomes `''`, never the string `'undefined'`: `reply_to_id` is
  // omitted for a question group and `sent_to_id` for a group nobody addressed,
  // and a caller comparing ids must be able to tell "no such link" from a link
  // that happens to be spelled oddly.
  const optionalId = (value: unknown): string =>
    value === undefined || value === null ? '' : String(value);
  return (body.message_groups ?? []).map((group) => ({
    id: String(group.id ?? ''),
    uuid: String(group.uuid ?? ''),
    content: typeof group.content === 'string' ? group.content : '',
    authorParticipantId: optionalId(group.author_participant_id),
    sentToId: optionalId(group.sent_to_id),
    replyToId: optionalId(group.reply_to_id),
    items: (group.message_items ?? []).map((item) => ({
      id: item.id === undefined || item.id === null ? '' : String(item.id),
      itemType: typeof item.item_type === 'string' ? item.item_type : '',
      details:
        typeof item.item_details === 'object' && item.item_details !== null
          ? (item.item_details as Record<string, unknown>)
          : {},
    })),
  }));
}

/** Every `attachment_message` item in a conversation, flattened across its groups. */
export function attachmentItemsOf(
  groups: readonly StoredMessageGroup[],
): readonly StoredMessageItem[] {
  return groups.flatMap((group) =>
    group.items.filter((item) => item.itemType === 'attachment_message'),
  );
}

/** One MCP connection created through the toolkits API. */
export interface CreatedMcpConnection {
  /** `elitea_tools.id` — what `entity_tool_mapping` and the attach route address. */
  readonly id: string;
  /** The row's stored `settings`, read back from the response rather than echoed from the request. */
  readonly settings: Record<string, unknown>;
}

/** What a caller may put in an MCP connection's `settings`. */
export interface McpConnectionSettings {
  /**
   * The MCP server endpoint. HTTPS ONLY on the native runtime: `parse_endpoint`
   * (`services/elitea-worker-rust/src/toolkits/mcp.rs:592-609`) refuses every
   * other scheme, refuses userinfo, a query string and a fragment, and the
   * client that follows is built `https_only` with no verification switch.
   * Main's own proxy validator is LOOSER — it also allows `http` on a loopback
   * host (`internal/api/v2/eliteacore/handler.go:3521-3530`) — so a URL the
   * platform stores happily can still be one the native runtime refuses.
   */
  readonly url?: string;
  /** The tool names to admit from the server's catalogue. Empty/absent means "whatever it publishes". */
  readonly selected_tools?: readonly string[];
  /** Anything else the toolkit row should carry. */
  readonly [key: string]: unknown;
}

/**
 * Create an MCP connection (an `elitea_tools` row of type `mcp`).
 *
 * WHY THIS IS AN API HELPER AND NOT A FORM DRIVER. `/app/mcps/create` renders
 * a schema-driven form whose type tiles come from the project's toolkit-type
 * catalogue, filtered to the mcp-flavoured entries
 * (`src/features/toolkits/lib/hooks/useGetCurrentMCPSchemas.hooks.ts:54-56`).
 * The catalogue now publishes ONE such entry. `GET /elitea_core/toolkits/
 * prompt_lib/{project}` serves every type the pinned SDK snapshot holds, and
 * `mcp` — "Remote MCP" — is one of them; `mcp_config` is served hidden, because
 * it is the container the pre-built servers are declared in and the reference
 * deployment shows no tile for it. So the MCP selector renders a Remote tile,
 * and the LOCAL group is still empty, which keeps the page's "Still no local
 * MCP available" state and the shot `e2e/visual/routes.visual.spec.ts` takes
 * of it.
 *
 * A remote-MCP connection needs a URL and an authorization flow that this
 * helper's callers do not want to drive, so setup still goes through this
 * route, as `e2e/journeys/mcps/mcps.oauth.spec.ts` also does inline. A spec
 * that wants to prove the FORM should now drive the Remote MCP tile.
 *
 * `settings` is returned as the SERVER stored it, not as the caller sent it, so
 * a test asserting on the endpoint asserts on the value the runtime will read.
 */
export async function createMcpConnection(
  page: Page,
  projectId: string,
  name: string,
  settings: McpConnectionSettings = {},
): Promise<CreatedMcpConnection> {
  const path = `/elitea_core/tools/prompt_lib/${projectId}`;
  const response = await page.request.post(`${BASE_URL}/api/v2${path}`, {
    data: {
      name,
      type: 'mcp',
      description: `${AUTOTEST_PREFIX}e2e mcp connection`,
      settings,
    },
  });
  if (!response.ok()) {
    throw new Error(
      `createMcpConnection: POST ${path} returned ${response.status()}: ${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as { id?: string | number; settings?: Record<string, unknown> };
  const id = String(body.id ?? '');
  if (!/^\d+$/.test(id)) {
    throw new Error(
      `createMcpConnection: POST ${path} answered without a numeric id: ${JSON.stringify(body).slice(0, 300)}`,
    );
  }
  return { id, settings: body.settings ?? {} };
}

/**
 * A GitHub toolkit instance, and the credential row it has to reference.
 *
 * ## The contract this encodes, and where it is written down
 *
 * `github` is the ONE toolkit type the create route validates a settings shape
 * for: `validateToolkitCreate`
 * (`services/elitea-main/internal/api/v2/toolkits/handler.go:987-1004`) answers
 * 400 unless `settings.repository` is a non-empty string AND
 * `settings.github_configuration` is present. That is not a Go invention —
 * the SDK's own settings schema declares `required: ["github_configuration",
 * "repository"]` for the type
 * (`services/elitea-main/internal/runtimecomposition/
 * current_toolkit_catalogue_snapshot.json`), and the create FORM renders both
 * as required fields.
 *
 * Three journeys need a github toolkit for reasons that have nothing to do
 * with credentials — the guardrail that blocks its type, the attach and the
 * detach — and all three used to post `settings: { selected_tools: [] }` and
 * take the 400. They share this helper so the contract is stated once.
 *
 * ## Why a REAL credential row, rather than a plausible-looking reference
 *
 * `github_configuration` is a configuration REFERENCE, resolved by
 * `refuseUnresolvableToolkitSettings` (`settings_validation.go:183`) whenever
 * the deployment composes the Configurations graph. The e2e stack composes it
 * — `deploy/docker-compose.e2e-standalone.yml` sets
 * `ELITEA_CONFIGURATIONS_ENABLED` — so a made-up `elitea_title` is answered
 * 400 here, exactly as it is on a shipped stack. This helper made the real row
 * before that was true, which is why turning the graph on cost its callers
 * nothing.
 *
 * NEVER a real credential: `data` carries only the placeholder base URL below,
 * and nothing here contacts api.github.com.
 */
export interface GithubToolkitFixture {
  readonly toolkitId: string;
  readonly toolkitName: string;
  readonly credentialId: string;
  readonly credentialTitle: string;
}

/** The placeholder GitHub endpoint. Deliberately unroutable. */
const GITHUB_PLACEHOLDER_BASE_URL = 'https://autotest.invalid/api';

/**
 * A token-shaped value to store on the credential the toolkit points at.
 *
 * It exists for the two journeys whose subject IS the sealing: the export
 * journey, which asserts that a sealed value never reaches the export
 * document, and the expanded-version journey, which asserts that the runtime
 * read resolves it back. Every other caller leaves it off and gets the
 * base-URL-only row this fixture has always made. The value is a fixture
 * string, never a real token — the credential
 * write seals it into the project vault and stores a `{{secret.<uuid>}}`
 * reference in its place, so the plain value exists nowhere the API can serve
 * it back.
 */
export interface GithubToolkitOptions {
  /** Extra `data` keys for the credential row, merged over the base URL. */
  readonly credentialData?: Readonly<Record<string, unknown>>;
}

export async function createGithubToolkit(
  request: APIRequestContext,
  projectId: string,
  toolkitName: string,
  options: GithubToolkitOptions = {},
): Promise<GithubToolkitFixture> {
  const credentialTitle = `${toolkitName}_cred`;
  const credential = await request.post(
    `${BASE_URL}/api/v2/configurations/configurations/${projectId}`,
    {
      data: {
        type: 'github',
        elitea_title: credentialTitle,
        label: credentialTitle,
        shared: false,
        data: { base_url: GITHUB_PLACEHOLDER_BASE_URL, ...options.credentialData },
      },
    },
  );
  if (!credential.ok()) {
    throw new Error(
      `createGithubToolkit: the credential POST returned ${credential.status()}: ${(await credential.text()).slice(0, 300)}`,
    );
  }
  const credentialId = String(((await credential.json()) as { id?: string | number }).id ?? '');

  const created = await request.post(
    `${BASE_URL}/api/v2/elitea_core/tools/prompt_lib/${projectId}`,
    {
      data: {
        name: toolkitName,
        type: 'github',
        settings: {
          // Both required fields, in the shape the stored row keeps: an OBJECT
          // reference, never the bare string that `CredentialFormFields.tsx`
          // records as a real defect class here.
          repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
          github_configuration: { elitea_title: credentialTitle, private: false },
          selected_tools: [],
        },
      },
    },
  );
  if (created.status() !== 201) {
    throw new Error(
      `createGithubToolkit: the toolkit POST returned ${created.status()}: ${(await created.text()).slice(0, 300)}`,
    );
  }
  const toolkitId = String(((await created.json()) as { id?: string | number }).id ?? '');
  if (toolkitId === '') {
    throw new Error('createGithubToolkit: the created toolkit carries no id');
  }
  return { toolkitId, toolkitName, credentialId, credentialTitle };
}

/** Remove what `createGithubToolkit` made, in dependency order. Never throws. */
export async function deleteGithubToolkit(
  request: APIRequestContext,
  projectId: string,
  fixture: GithubToolkitFixture | undefined,
): Promise<void> {
  if (fixture === undefined) return;
  await request
    .delete(`${BASE_URL}/api/v2/elitea_core/tool/prompt_lib/${projectId}/${fixture.toolkitId}`)
    .catch(() => {});
  if (fixture.credentialId !== '') {
    await request
      .delete(`${BASE_URL}/api/v2/configurations/configuration/${projectId}/${fixture.credentialId}`)
      .catch(() => {});
  }
}

/** Delete a toolkit/MCP connection (cleanup helper). Never throws. */
export async function deleteToolkit(
  page: Page,
  projectId: string,
  toolkitId: string,
): Promise<void> {
  await page.request.delete(
    `${BASE_URL}/api/v2/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`,
  );
}

/** One toolkit row as an agent version serves it back under `version_details.tools`. */
export interface AttachedToolkitRow {
  /** `elitea_tools.id`. The attach route addresses this, NOT the mapping's own id. */
  readonly toolId: string;
  /** The toolkit family — `mcp` for an MCP connection, and what `is_mcp_type` keys on. */
  readonly type: string;
  readonly name: string;
  /** The settings the runtime will read. Served straight from the stored row. */
  readonly settings: Record<string, unknown>;
  /** The tool names the mapping admits, after the version-level intersection. */
  readonly selectedTools: readonly string[];
}

/**
 * Read back the toolkits attached to one agent version, as the server serves
 * them — the projection `services/elitea-main/internal/db/queries/agent_chat.sql:27-110`
 * builds and the worker's frozen tool snapshot parses
 * (`services/elitea-worker-rust/src/toolkits/snapshot.rs:327-353`).
 *
 * Asserting on THIS rather than on the attach response is the point: the
 * agent-as-tool attach once answered 200 and wrote nothing at all (see
 * `chat.agent-tools.spec.ts`'s defect class 2), and an attach that writes no
 * row is invisible from the mutation's own status code.
 */
export async function readAttachedToolkits(
  page: Page,
  projectId: string,
  agentId: string,
): Promise<readonly AttachedToolkitRow[]> {
  const url = `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`;
  const response = await page.request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readAttachedToolkits: GET ${url} -> ${response.status()} ${response.statusText()}` +
      `${await describeRefusal(response)}\n` +
      `${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as {
    version_details?: {
      tools?: readonly {
        tool_id?: string | number;
        id?: string | number;
        type?: string;
        name?: string;
        config?: Record<string, unknown>;
        settings?: Record<string, unknown>;
        selected_tools?: readonly string[];
      }[];
    };
  };
  return (body.version_details?.tools ?? []).map((tool) => ({
    toolId: String(tool.tool_id ?? tool.id ?? ''),
    type: tool.type ?? '',
    name: tool.name ?? '',
    // The agent read serves the stored settings under `config`; the chat
    // resolver serves the same bytes under `settings`. Take whichever is
    // present so a caller asserts on the endpoint either way.
    settings: tool.config ?? tool.settings ?? {},
    selectedTools: tool.selected_tools ?? [],
  }));
}

/* ────────────────────────────────────────────────────────────────────────────
 * The TOOLKIT-INVOCATION journeys' shared steps
 *
 * `chat.toolkit.spec.ts` and `chat.toolkit-hitl.spec.ts` drive the SAME
 * `openapi` toolkit — one through a read-only call, one through a
 * sensitive-action pause — and the pieces below are the ones both need and
 * neither can weaken alone: the wire contract with `deploy/mock-llm/server.py`,
 * the form flow that authors the toolkit, and the platform guardrail that
 * decides whether a call pauses.
 * ──────────────────────────────────────────────────────────────────────────── */

/**
 * The mock's host address. `deploy/docker-compose.standalone-full.yml`
 * publishes `STANDALONE_MOCK_PORT` for exactly this, and
 * `e2e/streaming/index.streaming.spec.ts` already reads the model journal the
 * same way.
 */
const MOCK_HOST = `http://localhost:${process.env['STANDALONE_MOCK_PORT'] ?? '8090'}`;

/**
 * The `[[mock:call_tool …]]` contract, restated.
 *
 * These names are the wire contract with `deploy/mock-llm/server.py`
 * (`TOOL_STATUS_OPERATION`, `TOOL_CREATE_OPERATION`, `TOOL_*_SENTINEL` and
 * `CALL_TOOL_MARKER_PREFIX`). They are restated rather than imported because
 * the mock is a Python file in another service's tree and there is nothing to
 * import — so the pairing is ASSERTED instead: `fetchMockToolSpec` below reads
 * the document the mock actually serves and fails if it does not carry these
 * operation ids.
 */
export const MOCK_TOOL_READ_OPERATION = 'mock_tool_status';
export const MOCK_TOOL_EFFECTFUL_OPERATION = 'mock_tool_create_item';
/**
 * The sentinel the EFFECTFUL route puts in its own reply (`TOOL_CREATE_SENTINEL`).
 *
 * Only the effectful one is exported, because only it has an unconditional
 * assertion to serve: a DECLINED call must never produce it. The read route's
 * sentinel has no such use — whether it reaches the transcript depends on
 * whether the worker can reach the tool at all (see `MockToolSpec.reachable`),
 * so asserting on it would state a stack property, not a contract.
 */
export const MOCK_TOOL_CREATE_SENTINEL = 'MOCKTOOLCREATED';
/**
 * The last word of the mock's continuation reply (`CALL_TOOL_SENTINEL`).
 *
 * The SETTLE SIGNAL for every assertion about a tool result, and it has to be
 * this rather than anything nearer the front. The stored assistant row is
 * readable while it is still being written, and a tool result is long — the
 * runtime's blocked-call payload is ~770 bytes with its `type` field LAST. A
 * poll on the denial comment (which appears early) returned a 370-byte prefix
 * and the next assertion then reported `sensitive_tool_blocked` missing from a
 * row that carried it (measured). Polling on the sentinel cannot settle early.
 */
export const MOCK_CALL_TOOL_SENTINEL = 'MOCKCALLTOOLEND';

/** The prompt that makes the mock answer with a call to `operation`. */
export function callToolPrompt(operation: string, tail: string): string {
  return `[[mock:call_tool ${operation}]] ${tail}`;
}

/**
 * The same marker, WITH arguments — the form a tool with a required field needs.
 *
 * `callToolPrompt` above sends the empty object, which is right for the mock's
 * two `/tool` operations (neither declares a required parameter) and refused
 * for a nested Elitea agent: the runtime presents a saved agent to the model as
 * a tool whose schema requires a non-empty `task`, and a call without one is an
 * invalid configuration that kills the turn before the child ever runs. So a
 * delegation journey names the tool AND the task it delegates.
 *
 * The arguments travel inside the marker, which the mock closes on the `]]`
 * that BALANCES its opening `[[` (`_marker_body`), and that nesting rule is
 * what a two-level delegation needs: the task a parent hands its child is
 * itself a whole marker, so the child's `]]` closes before the parent's.
 *
 * So a `]]` is refused here only when it would close the marker EARLY — an
 * unbalanced one. The check mirrors the mock's own scan rather than banning the
 * sequence, because banning it made the nested form unwritable while the mock
 * has parsed it since the day it was written. An unclosed `[[` is refused for
 * the same reason from the other side: it would swallow the marker's own `]]`
 * and the arguments would then be truncated at the tail.
 */
export function callToolWithArgumentsPrompt(
  toolName: string,
  args: Readonly<Record<string, unknown>>,
  tail: string,
): string {
  const encoded = JSON.stringify(args);
  expect(
    markerNestingDepth(encoded),
    'the marker closes on the `]]` that balances its `[[`, so a scripted argument may ' +
      'not carry an unbalanced one — it would close the marker early or swallow its close',
  ).toBe(0);
  return `[[mock:call_tool ${toolName} ${encoded}]] ${tail}`;
}

/**
 * The nesting depth `text` leaves behind, or `-1` if it ever closes past zero.
 *
 * The same two-character scan `_marker_body` makes in `deploy/mock-llm/server.py`:
 * `[[` opens, `]]` closes, and everything else is one character of content. A
 * result of `0` is the only value that leaves the enclosing marker intact.
 */
function markerNestingDepth(text: string): number {
  let depth = 0;
  let index = 0;
  while (index < text.length - 1) {
    const pair = text.slice(index, index + 2);
    if (pair === '[[') {
      depth += 1;
      index += 2;
      continue;
    }
    if (pair === ']]') {
      depth -= 1;
      if (depth < 0) return -1;
      index += 2;
      continue;
    }
    index += 1;
  }
  return depth;
}

/**
 * The function name a SAVED AGENT is offered to the model under, when it is
 * attached to another agent or sitting in a conversation as a participant.
 *
 * The two runtimes name it differently and both names are derived from the
 * SAME stored reference, so the leg decides which one a scripted call must
 * use:
 *
 *  - the native Rust runtime names it `elitea_agent_<applicationId>_v_<versionId>`
 *    (`application_tool_name`, services/elitea-worker-rust/src/agents/application_tools.rs);
 *  - the SDK worker names it after the agent itself (`ApplicationToolkit.get_toolkit`
 *    passes `app_details['name']`), which reaches the model unchanged for a
 *    name in the `autotest_` alphabet.
 *
 * `E2E_WORKER` is set by `scripts/chat-stream-e2e.sh`, the one entry point that
 * knows which runtime is answering. Every caller asserts the offered tool list
 * actually CONTAINS what this returns, so a naming change fails with the list
 * the runtime really sent rather than with a turn that quietly did nothing.
 */
export function agentAsToolName(agent: {
  readonly agentId: string;
  readonly versionId: string;
  readonly name: string;
}): string {
  const worker = process.env['E2E_WORKER'] ?? 'rust';
  return worker === 'rust' ? `elitea_agent_${agent.agentId}_v_${agent.versionId}` : agent.name;
}

/** One entry of the mock's TOOL journal — see `_record_tool` in the mock. */
export interface MockToolJournalEntry {
  readonly method: string;
  readonly path: string;
  readonly operation: string;
  readonly body?: string;
}

/** One entry of the mock's MODEL journal, with the fields these journeys read. */
export interface MockLlmJournalEntry {
  readonly path: string;
  readonly mode: string | null;
  /** The function names this request offered the model. */
  readonly tools: readonly string[];
  /**
   * The SYSTEM prompt the request carried (`_system_text` in the mock).
   *
   * The only observable for anything the runtime does to an agent's
   * instructions — agent-variable substitution above all — because the reply
   * is an echo of the last USER message and shows the system prompt never.
   * Read by `chat.variables.spec.ts`.
   */
  readonly instructions: string;
}

async function readMockJournal<T>(page: Page, url: string): Promise<readonly T[]> {
  const response = await page.request.get(url, { timeout: 15_000 });
  expect(
    response.ok(),
    `the mock's journal is not readable at ${url}. The llm-mock service must publish ` +
      'STANDALONE_MOCK_PORT — see deploy/docker-compose.standalone-full.yml.',
  ).toBe(true);
  const body = (await response.json()) as { data?: readonly T[] };
  return body.data ?? [];
}

async function clearMockJournal(page: Page, url: string): Promise<void> {
  const cleared = await page.request.delete(url, { timeout: 15_000 });
  expect(cleared.ok(), `the mock's journal is not clearable at ${url}`).toBe(true);
}

/**
 * The TOOL calls the mock has served, newest last.
 *
 * SEPARATE from the model journal on purpose, and that separation is the whole
 * point of the endpoint: "the agent ran this tool" is a fact about traffic
 * between the WORKER and the tool's own host, which never reaches the model
 * hop and therefore leaves no trace a model-side assertion could read.
 */
export async function readMockToolJournal(page: Page): Promise<readonly MockToolJournalEntry[]> {
  return readMockJournal<MockToolJournalEntry>(page, `${MOCK_HOST}/tool/__journal`);
}

/** Empty the TOOL journal, so a journey can bound the window it asserts over. */
export async function clearMockToolJournal(page: Page): Promise<void> {
  await clearMockJournal(page, `${MOCK_HOST}/tool/__journal`);
}

/** The MODEL requests the mock has served, newest last. */
export async function readMockLlmJournal(page: Page): Promise<readonly MockLlmJournalEntry[]> {
  return readMockJournal<MockLlmJournalEntry>(page, `${MOCK_HOST}/__journal`);
}

/** Empty the MODEL journal. */
export async function clearMockLlmJournal(page: Page): Promise<void> {
  await clearMockJournal(page, `${MOCK_HOST}/__journal`);
}

/** The OpenAPI document the mock serves, as text and as the base URL it declares. */
export interface MockToolSpec {
  /** The document verbatim — what a toolkit's `settings.spec` must hold. */
  readonly text: string;
  /** `servers[0].url`. */
  readonly baseUrl: string;
  /**
   * Whether the native Rust worker can actually REACH that base URL.
   *
   * Its OpenAPI client is `https_only()` (`families/openapi/client.rs`), and
   * the certificate has to chain to a public root — the worker image carries
   * the Debian bundle and nothing else. So a `https://llm-mock:…` base URL
   * materializes, is offered to the model and is DISPATCHED, and the request
   * then fails in the transport. A journey asserts the tool-side journal only
   * where this is true; everywhere else it asserts the dispatch instead.
   */
  readonly reachable: boolean;
}

/**
 * Read the OpenAPI document the mock serves, and assert it is the one these
 * journeys were written against.
 *
 * Fetched rather than written out here so the toolkit a journey authors and
 * the routes the mock answers cannot disagree: one document, generated from
 * the mock's own constants, is both the thing pasted into the form and the
 * thing served at the base URL it names.
 */
export async function fetchMockToolSpec(page: Page): Promise<MockToolSpec> {
  const url = `${MOCK_HOST}/tool/openapi.json`;
  const response = await page.request.get(url, { timeout: 15_000 });
  expect(
    response.ok(),
    `the mock does not serve a tool specification at ${url} — is the llm-mock image current? ` +
      'Rebuild it with `deploy/scripts/standalone-stack.sh build llm-mock`.',
  ).toBe(true);
  const text = await response.text();
  const document = JSON.parse(text) as {
    servers?: readonly { url?: string }[];
    paths?: Record<string, Record<string, { operationId?: string }>>;
  };
  const operations = Object.values(document.paths ?? {}).flatMap((item) =>
    Object.values(item).map((operation) => operation.operationId),
  );
  expect(
    operations,
    'the mock serves a tool specification that does not carry the read operation these journeys call',
  ).toContain(MOCK_TOOL_READ_OPERATION);
  expect(
    operations,
    'the mock serves a tool specification that does not carry the effectful operation these journeys call',
  ).toContain(MOCK_TOOL_EFFECTFUL_OPERATION);
  const baseUrl = document.servers?.[0]?.url ?? '';
  expect(baseUrl, 'the tool specification must declare an absolute server URL').not.toBe('');
  return { text, baseUrl, reachable: baseUrl.startsWith('http://') };
}

/** An `openapi` toolkit created through the toolkit form. */
export interface OpenApiToolkitCreatedThroughForm {
  /** The project the form wrote into — read from the request, never assumed. */
  readonly projectId: string;
  /** `elitea_tools.id` — the key the agent attach and every later route uses. */
  readonly toolkitId: string;
  /** The toolkit's name, which is also how the agent's "+ Toolkit" menu lists it. */
  readonly name: string;
}

/** Matched WITHOUT a project id: these journeys work inside the driver's personal project (#290). */
const TOOLKITS_RE = /\/elitea_core\/tools\/prompt_lib\/(\d+)$/;

/**
 * Author an `openapi` toolkit by filling in the toolkit form, and hand back
 * what the server stored.
 *
 * THROUGH THE FORM, deliberately. The type grid, the schema editor and the
 * operation list it derives are the surface these journeys exist to exercise,
 * and none of it is reachable from the API fixture: `settings.selected_tools`
 * is populated by the FORM as a side effect of the schema parsing
 * (`mergeSelectedToolsWithNewSchema`), so a toolkit created by POST carries
 * whatever the caller typed rather than what a user gets.
 *
 * The form carries NO `data-testid` anywhere — measured, the whole
 * toolkit-creation surface is addressed by role and label — so the selectors
 * below are roles and accessible names, matching
 * `e2e/journeys/toolkits/toolkits.lifecycle.spec.ts`.
 *
 * Leaves the browser on the created toolkit's detail page, which is where the
 * form navigates on success.
 */
export async function createOpenApiToolkitThroughForm(
  page: Page,
  name: string,
  specText: string,
): Promise<OpenApiToolkitCreatedThroughForm> {
  // Armed BEFORE the save: the response carries the project id and the stored
  // settings, so the assertions read what the server wrote.
  const created = page.waitForResponse(
    (r) => TOOLKITS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );

  await page.goto(`${BASE_URL}/app/toolkits/create`);
  await expect(page.getByPlaceholder('Search toolkits')).toBeVisible({ timeout: 30_000 });
  await page.getByRole('button', { name: 'OpenAPI', exact: true }).click();

  const nameInput = page.getByRole('textbox', { name: 'Toolkit Name' });
  await expect(nameInput, 'the openapi form must offer a name field').toBeVisible({ timeout: 20_000 });
  await nameInput.fill(name);

  // The schema editor is a CodeMirror instance with no testid and no
  // accessible name (`OpenAPISchemaInput` does not pass one), so `.cm-content`
  // is the only handle — and `fill()` on it is a TRAP, measured: CodeMirror 6
  // reconciles its own document from transactions, ignores the plain `input`
  // event Playwright's fill dispatches, and the form then saves
  // `settings.spec: ""` while the editor looks full on screen and on the
  // failure screenshot. `keyboard.insertText` goes in as a real text-insertion
  // event, which CodeMirror's DOM-change handler reads, and — unlike typing
  // character by character — is not mangled by the editor's bracket and quote
  // auto-closing.
  const editor = page.locator('.cm-content').first();
  await expect(editor, 'the openapi form must offer a schema editor').toBeVisible({ timeout: 20_000 });
  await editor.click();
  await page.keyboard.insertText(specText);

  // The PARSED operation table is what proves the editor's content reached the
  // form's parser. Not the operation NAME as text: the specification is itself
  // visible inside the editor, so a name match passes against an editor whose
  // content never left it — which is exactly the failure above. The table
  // renders only once `openAPIExtract` has returned at least one operation.
  await expect(
    page.getByRole('table', { name: 'tools actions table' }),
    'the form never parsed the pasted specification — no operation table appeared',
  ).toBeVisible({ timeout: 30_000 });

  const save = page.getByRole('button', { name: 'Save', exact: true });
  await expect(save).toBeEnabled({ timeout: 15_000 });
  await save.click();

  const response = await created;
  expect(
    response.status(),
    `the toolkit must be created: ${(await response.text()).slice(0, 300)}`,
  ).toBe(201);
  const projectId = TOOLKITS_RE.exec(new URL(response.url()).pathname)?.[1] ?? '';
  expect(projectId, 'the toolkit must belong to a project').not.toBe('');
  const body = (await response.json()) as {
    id?: string;
    settings?: { spec?: string; selected_tools?: readonly string[] };
  };
  const toolkitId = String(body.id ?? '');
  expect(toolkitId, 'the created toolkit must carry an id').toMatch(/^\d+$/);
  expect(
    body.settings?.selected_tools ?? [],
    'the form must have selected the operations it parsed, or the agent exposes no tool',
  ).toContain(MOCK_TOOL_READ_OPERATION);

  return { projectId, toolkitId, name };
}

/**
 * Attach a toolkit to the agent open in the edit page, through the Tools
 * panel's own "+ Toolkit" picker.
 *
 * Through the PICKER rather than the PATCH it emits, because the picker is the
 * unvalidated half: the button is disabled while the agent is unsaved, the
 * menu lists instances from a paged request and filters them client-side, and
 * the attach it sends omits `selected_tools` entirely — a presence-sensitive
 * distinction (#248) that a hand-written PATCH would not reproduce.
 */
export async function attachToolkitThroughPicker(page: Page, toolkitName: string): Promise<void> {
  const attached = page.waitForResponse(
    (r) => /\/elitea_core\/tool\/prompt_lib\/\d+\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'PATCH',
    { timeout: 45_000 },
  );
  const addButton = page.getByTestId('agent-add-toolkit-button');
  await expect(
    addButton,
    'the Tools panel must offer the toolkit picker — it stays disabled while the agent is unsaved',
  ).toBeEnabled({ timeout: 30_000 });
  await addButton.click();

  // PAGED IN, not searched for. The dropdown fetches 20 instances at a time
  // (`GET …/tools/prompt_lib/{project}?limit=20`, `ORDER BY name`) and filters
  // what it already has CLIENT-side, so a toolkit past the first page cannot
  // be reached by typing its name — the filtered list is empty, there is
  // nothing to scroll, and the next page is never requested. Measured on a
  // stack carrying 31 toolkits: a freshly created `autotest_tk-…` sorts after
  // 20 `autotest_child-…` rows and the search found nothing.
  //
  // So the menu's own infinite scroll is driven instead: its scroller is the
  // Menu PAPER (`slotProps.paper.onScroll`), and it asks for another 20 once
  // it is within 48px of the bottom.
  const item = page.getByRole('menuitem', { name: new RegExp(toolkitName) });
  const menuPaper = page.locator('.MuiMenu-paper').first();
  await expect(menuPaper, 'the toolkit picker did not open').toBeVisible({ timeout: 15_000 });
  await expect
    .poll(
      async () => {
        const found = await item.count();
        if (found === 0) {
          await menuPaper.evaluate((element) => {
            element.scrollTop = element.scrollHeight;
          });
        }
        return found;
      },
      {
        timeout: 60_000,
        message: `the picker never listed ${toolkitName}, even after paging to the end of the list`,
      },
    )
    .toBeGreaterThan(0);
  await item.first().click();

  const response = await attached;
  expect(
    response.status(),
    `the toolkit attach was refused: ${(await response.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
}

/** The platform-wide toolkit security policy, as the admin route serves it. */
export interface ToolkitGuardrailValues {
  readonly blocked_toolkits: readonly string[];
  readonly blocked_tools: Record<string, readonly string[]>;
  readonly sensitive_tools: Record<string, readonly string[]>;
  readonly sensitive_action_company_name: string;
  readonly sensitive_action_message_template: string;
}

const GUARDRAILS_PATH = '/api/v2/admin/plugin_config_values/administration/guardrails';

/**
 * Write the platform-wide `toolkit_security` policy, as the ADMIN persona.
 *
 * This is GLOBAL state and there is no narrower place to put it: the runtime
 * reads `sensitive_tools` from the agent execution input, which
 * `resolveToolkitGuardrails` fills from `centry.platform_config` alone — no
 * per-agent or per-project override exists. The chat driver cannot write it
 * (measured: 403), so a second, admin-authenticated request context is used
 * for this one call and closed again.
 *
 * A caller that sets it MUST restore it, and must do so from a hook that runs
 * on failure too — every other spec in this project shares the stack, and a
 * leaked `sensitive_tools` entry turns their tool calls into pauses nothing
 * answers.
 */
export async function setToolkitGuardrails(values: ToolkitGuardrailValues): Promise<void> {
  const admin = await playwrightRequest.newContext({
    baseURL: BASE_URL,
    storageState: STORAGE_STATE.admin,
  });
  try {
    const saved = await admin.put(GUARDRAILS_PATH, { data: { values } });
    expect(
      saved.status(),
      `the admin persona must be able to write the guardrails policy: ${(await saved.text()).slice(0, 300)}`,
    ).toBe(200);
    // Read back rather than trust the 200: the value this journey depends on
    // is the one a LATER turn will be frozen against, and a write that landed
    // in a different section would answer 200 all the same.
    const readBack = await admin.get(GUARDRAILS_PATH);
    expect(readBack.ok(), 'the guardrails policy must be readable after the write').toBe(true);
    const stored = (await readBack.json()) as { values?: { sensitive_tools?: Record<string, unknown> } };
    expect(
      Object.keys(stored.values?.sensitive_tools ?? {}),
      'the sensitive-tool policy the server stored is not the one that was written',
    ).toEqual(Object.keys(values.sensitive_tools));
  } finally {
    await admin.dispose();
  }
}

/** The empty policy — what a stack starts with, and what a journey restores. */
export const EMPTY_TOOLKIT_GUARDRAILS: ToolkitGuardrailValues = {
  blocked_toolkits: [],
  blocked_tools: {},
  sensitive_tools: {},
  sensitive_action_company_name: '',
  sensitive_action_message_template: '',
};

/** The stored assistant row's `metadata.hitl_interrupt`, as these journeys read it. */
export interface StoredHitlInterrupt {
  readonly guardrail_type?: string;
  readonly available_actions?: readonly string[];
  readonly tool_call_id?: string;
  readonly tool_name?: string;
  readonly toolkit_name?: string;
  readonly toolkit_type?: string;
  readonly interrupt_id?: string;
}

/**
 * Poll until the newest stored assistant row carries a HITL interrupt, and
 * return it.
 *
 * The STORE rather than the card, for the same reason
 * `expectStoredAssistantAnswer` reads the store: the interrupt's identity —
 * `tool_call_id` and `interrupt_id` — is what a later assertion has to bind
 * the decision to, and none of it is rendered.
 */
export async function readStoredHitlInterrupt(
  page: Page,
  projectId: string,
  conversationId: string | number,
  timeout = 150_000,
): Promise<StoredHitlInterrupt> {
  let interrupt: StoredHitlInterrupt | undefined;
  await expect
    .poll(
      async () => {
        const stored = await page.request.get(
          `${BASE_URL}/api/v2/elitea_core/messages/prompt_lib/${projectId}/${String(conversationId)}`,
        );
        if (!stored.ok()) return false;
        const body = (await stored.json()) as {
          items?: readonly {
            role?: string;
            metadata?: { hitl_interrupt?: StoredHitlInterrupt; is_error?: boolean };
          }[];
        };
        const assistant = body.items?.find((item) => item.role === 'assistant');
        // A refused turn is stored as an assistant row too, and it never grows
        // an interrupt — so failing fast on it turns a 150s timeout into a
        // message that names the refusal.
        expect(
          assistant?.metadata?.is_error,
          'the turn was refused instead of pausing — read the worker log for the assembly error code',
        ).not.toBe(true);
        interrupt = assistant?.metadata?.hitl_interrupt;
        return interrupt !== undefined;
      },
      {
        timeout,
        message: 'the turn never parked on a sensitive-tool pause',
      },
    )
    .toBe(true);
  return interrupt ?? {};
}

/**
 * Put `prompt` in the chat composer and hand back its Send control, ready to
 * click.
 *
 * WHY THE FILL IS RETRIED RATHER THAN DONE ONCE. `chat-send-button` is
 * rendered only while the composer holds text, and a chat pane that is still
 * resolving its conversation re-renders after the composer first becomes
 * editable. A fill that lands inside that window is discarded, and a spec that
 * clicks straight afterwards waits out its whole budget for a control that
 * will never appear — `locator.click: Test ended` under `waiting for
 * getByTestId('chat-send-button')`, which is what run 33812152063 recorded for
 * `chat.pipeline.spec.ts`. Waiting for the button instead of retrying the fill
 * does not help: the text is already gone, so no amount of waiting brings the
 * control back.
 *
 * Callers must arm any `waitForResponse` AFTER this resolves. Armed before it,
 * the budget covers the composer settling as well as the request, so a slow
 * stack reports "the start was never admitted" about a turn that was never
 * sent — which is how the cause above stayed hidden through run 33810201650.
 *
 * `chat.streaming.spec.ts` deliberately does NOT use this: it asserts the Send
 * control is ABSENT before the fill, which is a claim about the app this
 * helper's retry loop would swallow.
 */
export async function fillComposer(scope: Page | Locator, prompt: string) {
  const input = scope.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  const sendButton = scope.getByTestId('chat-send-button');
  await expect(async () => {
    await input.fill(prompt);
    await expect(sendButton).toBeEnabled({ timeout: 2_000 });
  }).toPass({ timeout: 30_000 });
  return sendButton;
}
