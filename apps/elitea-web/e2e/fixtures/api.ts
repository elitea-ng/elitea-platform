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

/**
 * Create a conversation via API.
 * Returns the new conversation id.
 */
export async function createConversation(
  request: APIRequestContext,
  name: string,
): Promise<string> {
  const url = `${API_BASE}/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}`;
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
): Promise<void> {
  await request.delete(
    `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`,
  );
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
): Promise<void> {
  await request.delete(
    `${API_BASE}/elitea_core/application/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`,
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
): Promise<CreatedAgent> {
  const path = `/elitea_core/applications/prompt_lib/${projectId}`;
  const response = await request.post(`${API_BASE}${path}`, {
    data: {
      name,
      description: `${AUTOTEST_PREFIX}version contract fixture`,
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

/**
 * The project's `X-SECRET` value, resolved from the project's own vault.
 *
 * The expanded version-details read is gated on this header, and the value is
 * per project and generated at boot — so a journey that wanted a literal
 * would need one configured per environment, and would silently start
 * asserting "a wrong header is refused" everywhere the literal went stale.
 * Reading it makes the journey portable and keeps the refusal case honest.
 */
export async function resolveProjectSecretHeader(
  request: APIRequestContext,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<string> {
  const url = `${API_BASE}/secrets/secret/default/${projectId}/secrets_header_value`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(
      `resolveProjectSecretHeader: GET ${url} -> ${response.status()}. The project vault holds ` +
        `no secrets_header_value; cmd/elitea-main backfills one at boot.` +
        `${await describeRefusal(response)}`,
    );
  }
  const body = (await response.json()) as { value?: unknown };
  const value = typeof body.value === 'string' ? body.value : '';
  if (value === '') {
    throw new Error('resolveProjectSecretHeader: the vault answered an empty secrets_header_value');
  }
  return value;
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

/** One row of the project's tag list. */
export interface StoredTag {
  readonly id: string;
  readonly name: string;
}

/**
 * The project's tags, as the tag control reads them.
 *
 * The list is filtered by NAME rather than paged: this reads the whole set
 * because the caller matches its own `autotest_` names out of it, and the
 * route offers no name filter to narrow with.
 */
export async function readTags(
  request: APIRequestContext,
  projectId: string = DEFAULT_PROJECT_ID,
): Promise<readonly StoredTag[]> {
  const url = `${API_BASE}/elitea_core/tags/prompt_lib/${projectId}`;
  const response = await request.get(url);
  if (!response.ok()) {
    throw new Error(
      `readTags: GET ${url} -> ${response.status()}: ${(await response.text()).slice(0, 300)}`,
    );
  }
  const body = (await response.json()) as { rows?: readonly Record<string, unknown>[] };
  return (body.rows ?? []).map((row) => ({
    id: String(row['id'] ?? ''),
    name: String(row['name'] ?? ''),
  }));
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
      message_items?: readonly { item_type?: unknown; item_details?: unknown }[];
    }[];
  };
  return (body.message_groups ?? []).map((group) => ({
    id: String(group.id ?? ''),
    uuid: String(group.uuid ?? ''),
    content: typeof group.content === 'string' ? group.content : '',
    items: (group.message_items ?? []).map((item) => ({
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
 * the deployment composes the Configurations graph. The e2e stack does not set
 * `ELITEA_CONFIGURATIONS_ENABLED`, so a made-up `elitea_title` would be
 * accepted there today — and would start answering 400 on any deployment that
 * turns the graph on. One extra POST buys a fixture that is correct on both.
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

export async function createGithubToolkit(
  request: APIRequestContext,
  projectId: string,
  toolkitName: string,
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
        data: { base_url: GITHUB_PLACEHOLDER_BASE_URL },
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
