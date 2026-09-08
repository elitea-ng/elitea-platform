/**
 * Pipeline E2E fixtures: read back what the backend actually STORED for a
 * pipeline version, and parse the graph out of it.
 *
 * Why this exists. The pipelines journey used to assert
 * `.react-flow__node[data-id="Agent 1"]` after adding a node — a screen echo.
 * The canvas renders whatever the editor put in its own store, so that
 * assertion passed for as long as the editor was minting `"Agent 1"`, an id
 * the Rust pipeline compiler refuses outright (`valid_graph_id`,
 * `services/elitea-worker-rust/src/agents/graph/yaml.rs:362`, admits ASCII
 * alphanumerics plus `_ - . :` — no space). Every pipeline authored in the
 * visual editor was unloadable, and the journey was green throughout.
 *
 * The only way to catch that class is to read the STORED document back over
 * the API and check it against the runtime's own grammar, which is what
 * these helpers are for.
 */
import type { APIRequestContext } from '@playwright/test';

import { load as loadYaml } from 'js-yaml';

import { API_BASE, DEFAULT_PROJECT_ID } from './api';

/**
 * The node-id characters `valid_graph_id` (worker `yaml.rs:362`) admits,
 * minus `:` — the editor never mints a colon, and leaving it out keeps the
 * assertion strictly tighter than the runtime rather than looser.
 *
 * A stored id that fails this is a pipeline that cannot run.
 */
export const COMPILER_LEGAL_NODE_ID = /^[A-Za-z0-9_.-]+$/;

/** One stored application version, as `GET .../version/prompt_lib/...` returns it. */
export interface StoredPipelineVersion {
  readonly id: string;
  readonly name: string;
  /** The pipeline YAML document. This is what the worker compiles. */
  readonly instructions: string;
  /**
   * `openai` or `pipeline`. Load-bearing on a version CREATE: `insertVersion`
   * substitutes the literal `"openai"` for an empty value
   * (`internal/infra/db/repos/applications.go:29, 493-496`), so a
   * save-as-version that failed to pin it would mint an agent out of a
   * pipeline — same rows, wrong executor, nothing on screen to say so.
   */
  readonly agentType: string;
  /**
   * `pipeline_settings` — the laid-out node/edge geometry. `{}` when the
   * column was never written, which is exactly what a version created by the
   * POST alone looks like: `versionFromBody` reads no such key and
   * `insertVersion`'s INSERT does not name the column.
   */
  readonly pipelineSettings: Readonly<Record<string, unknown>>;
  /** `meta` — carries `step_limit`/`internal_tools`, both of which a clone must not reset. */
  readonly meta: Readonly<Record<string, unknown>>;
  /**
   * `welcome_message` — a version-level column the pipeline editor's
   * configuration form writes. Read here because the form's own inputs keep
   * the typed value whether or not the save carried it, so only the stored
   * row can tell a working save from a discarded one.
   */
  readonly welcomeMessage: string;
  /** `tags` — stored as `application_version_tag_association` rows; the version GET returns the joined names. */
  readonly tagNames: readonly string[];
}

/** The parsed pipeline document — only the fields these journeys assert on. */
export interface StoredPipelineGraph {
  readonly entry_point?: string;
  readonly nodes: readonly Readonly<Record<string, unknown>>[];
  readonly state?: Readonly<Record<string, unknown>>;
}

function describeResponse(status: number, statusText: string, body: string): string {
  return `${status} ${statusText}\n${body.slice(0, 300)}`;
}

/**
 * Resolve the version id the `/app/pipelines/latest/{id}` route opens.
 *
 * The URL segment is the literal word `latest`; the version it means is the
 * one named `base` (`entities/version/model/selectors.ts`'s
 * `LATEST_VERSION_NAME`). The application-detail response carries it as
 * `version_details`, so that is preferred; the `versions[]` summary list is
 * the fallback.
 */
export async function resolveLatestPipelineVersionId(
  request: APIRequestContext,
  projectId: string,
  applicationId: string,
): Promise<string> {
  const url = `${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${applicationId}`;
  const resp = await request.get(url);
  if (!resp.ok()) {
    throw new Error(
      `resolveLatestPipelineVersionId: GET ${url} -> ${describeResponse(resp.status(), resp.statusText(), await resp.text())}\n` +
        'If this is a 401, pass `page.request` (which shares the browser context cookies), not the bare `request` fixture.',
    );
  }
  const body = (await resp.json()) as {
    version_details?: { id?: string };
    versions?: readonly { id?: string; name?: string }[];
  };
  const detailId = body.version_details?.id;
  if (typeof detailId === 'string') return detailId;

  const base = (body.versions ?? []).find(version => version.name === 'base');
  if (typeof base?.id === 'string') return base.id;

  throw new Error(
    `resolveLatestPipelineVersionId: application ${applicationId} carried no version_details.id and no "base" version: ` +
      `${JSON.stringify(body).slice(0, 300)}`,
  );
}

/**
 * Read one stored pipeline version straight from the API.
 *
 * `GET {API_BASE}/elitea_core/version/prompt_lib/{projectId}/{applicationId}/{versionId}`
 * (`services/elitea-main/api/openapi/v2.yaml`'s
 * `getApplicationVersionDetail`) — the same endpoint the editor itself
 * loads a pipeline from, so what comes back is exactly what a reload, an
 * export, or the worker would see.
 *
 * Status is checked BEFORE parsing: calling `.json()` on a 401 throws a
 * `SyntaxError` that says nothing about the real problem (the same trap
 * `fixtures/api.ts` documents on its own helpers).
 */
export async function readStoredPipelineVersion(
  request: APIRequestContext,
  projectId: string,
  applicationId: string,
  versionId: string,
): Promise<StoredPipelineVersion> {
  const url = `${API_BASE}/elitea_core/version/prompt_lib/${projectId}/${applicationId}/${versionId}`;
  const resp = await request.get(url);
  if (!resp.ok()) {
    throw new Error(
      `readStoredPipelineVersion: GET ${url} -> ${describeResponse(resp.status(), resp.statusText(), await resp.text())}\n` +
        'If this is a 401, pass `page.request` (which shares the browser context cookies), not the bare `request` fixture.',
    );
  }
  const body = (await resp.json()) as {
    id?: string;
    name?: string;
    instructions?: unknown;
    agent_type?: unknown;
    pipeline_settings?: unknown;
    meta?: unknown;
    welcome_message?: unknown;
    tags?: unknown;
  };
  if (typeof body.instructions !== 'string') {
    throw new Error(
      `readStoredPipelineVersion: version ${versionId} stored no \`instructions\` string ` +
        `(got ${typeof body.instructions}). A pipeline whose graph never reached the backend ` +
        `would look exactly like this: ${JSON.stringify(body).slice(0, 300)}`,
    );
  }
  return {
    id: String(body.id ?? versionId),
    name: String(body.name ?? ''),
    instructions: body.instructions,
    agentType: typeof body.agent_type === 'string' ? body.agent_type : '',
    pipelineSettings: asRecord(body.pipeline_settings),
    meta: asRecord(body.meta),
    welcomeMessage: typeof body.welcome_message === 'string' ? body.welcome_message : '',
    tagNames: Array.isArray(body.tags)
      ? body.tags
          .map(tag => (typeof tag === 'object' && tag !== null ? (tag as { name?: unknown }).name : undefined))
          .filter((name): name is string => typeof name === 'string')
      : [],
  };
}

/** A jsonb column as an object, or `{}` — never `null`, which `pipeline_settings`/`meta` can both be on the wire. */
function asRecord(value: unknown): Readonly<Record<string, unknown>> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

/**
 * The version elitea-main currently considers this application's DEFAULT.
 *
 * `GET {API_BASE}/elitea_core/default_version/prompt_lib/{projectId}/{applicationId}`
 * — a route the router really serves (`internal/api/router.go:1780`,
 * `appHandler.GetDefaultVersion`) and that `api/openapi/v2.yaml:7548-7553`
 * deliberately does NOT document, so no generated client exists for it and
 * the UI cannot read it. That asymmetry is exactly why this helper is here:
 * "set as default" is otherwise unassertable from outside. The bar REMEMBERS
 * the id it just set, so reading the screen back would prove only that the
 * component kept its own state — this reads what the server stored.
 */
export async function readDefaultPipelineVersionId(
  request: APIRequestContext,
  projectId: string,
  applicationId: string,
): Promise<string> {
  const url = `${API_BASE}/elitea_core/default_version/prompt_lib/${projectId}/${applicationId}`;
  const resp = await request.get(url);
  if (!resp.ok()) {
    throw new Error(
      `readDefaultPipelineVersionId: GET ${url} -> ${describeResponse(resp.status(), resp.statusText(), await resp.text())}`,
    );
  }
  const body = (await resp.json()) as { id?: unknown };
  if (typeof body.id !== 'string' && typeof body.id !== 'number') {
    throw new Error(`readDefaultPipelineVersionId: no id in ${JSON.stringify(body).slice(0, 300)}`);
  }
  return String(body.id);
}

/**
 * Parse a stored pipeline document (`instructions`) into its graph.
 *
 * Refuses anything that is not a mapping with a `nodes` array rather than
 * returning an empty shape — an empty graph is precisely what a broken save
 * produces, and a helper that quietly returns `{ nodes: [] }` would let
 * every assertion built on it vacuously pass.
 */
export function parseStoredGraph(instructions: string): StoredPipelineGraph {
  if (instructions.trim().length === 0) {
    throw new Error('parseStoredGraph: `instructions` is empty — the pipeline graph was never persisted.');
  }
  let parsed: unknown;
  try {
    parsed = loadYaml(instructions);
  } catch (caught) {
    const message = caught instanceof Error ? caught.message : String(caught);
    throw new Error(`parseStoredGraph: stored instructions are not valid YAML: ${message}\n---\n${instructions.slice(0, 500)}`);
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    throw new Error(`parseStoredGraph: stored instructions are not a YAML mapping:\n---\n${instructions.slice(0, 500)}`);
  }
  const doc = parsed as Record<string, unknown>;
  const nodes = doc['nodes'];
  if (!Array.isArray(nodes)) {
    throw new Error(`parseStoredGraph: stored instructions carry no \`nodes\` array:\n---\n${instructions.slice(0, 500)}`);
  }
  const graph: StoredPipelineGraph = {
    nodes: nodes as readonly Readonly<Record<string, unknown>>[],
    ...(typeof doc['entry_point'] === 'string' ? { entry_point: doc['entry_point'] } : {}),
    ...(typeof doc['state'] === 'object' && doc['state'] !== null
      ? { state: doc['state'] as Readonly<Record<string, unknown>> }
      : {}),
  };
  return graph;
}

/** Every `nodes[].id` in a stored graph, as strings, in document order. */
export function storedNodeIds(graph: StoredPipelineGraph): readonly string[] {
  return graph.nodes.map(node => String(node['id'] ?? ''));
}

/* ────────────────────────────────────────────────────────────────────────────
 * Creating a pipeline WITHOUT the canvas
 *
 * The journeys ported from the legacy public suite's
 * `tests/ui/pipelines/test_pipeline_management.py` are about the DASHBOARD,
 * the detail screen and the editor's own controls — not about authoring a
 * graph. Driving `/pipelines/create` for each of them would spend a form
 * round trip and a canvas mount on a precondition, which is the "create
 * through the API before the test" rule `e2e/fixtures/api.ts` already states
 * for agents and conversations.
 *
 * `createAgent` cannot stand in. A pipeline is an `application` row whose
 * VERSION carries `agent_type: 'pipeline'`
 * (`entities/pipeline/model/types.ts`), and that one field decides three
 * things at once: whether the row appears under `agents_type=pipeline` on the
 * pipelines list at all, which editor route can open it, and which assembler
 * the worker picks for a turn. An agent created by `createAgent` satisfies
 * none of them.
 * ──────────────────────────────────────────────────────────────────────────── */

/**
 * The document a pipeline created through `/pipelines/create` is stored with.
 *
 * A COPY of `src/shared/lib/pipelineStarterTemplate.ts`, kept here because
 * nothing under `e2e/` imports from `src/` (the app's `@/*` path alias is
 * declared in a tsconfig whose `include` does not cover this directory, and
 * Playwright transpiles these files on its own). The copy is load-bearing in
 * the same way the original is, and for the reasons its doc comment records:
 * every `state:` entry is a MAPPING rather than a bare type name (the SDK
 * worker's `set_defaults` writes into each entry and raises `TypeError` on a
 * string), and `input_mapping` carries BOTH `system` and `task` (the SDK
 * selects its pipeline branch on `'system' in func_args`). A pipeline seeded
 * with anything less is admitted, saved, and then fails its first turn.
 *
 * A journey that only needs "a pipeline exists" is free to ignore all of
 * that; `chat.pipeline-execution.spec.ts` is the one that runs it, and it is
 * the reason this is the starter document rather than a hand-cut minimal one.
 */
export const PIPELINE_STARTER_TEMPLATE = `state:
  input:
    type: str
  messages:
    type: list
entry_point: LLM_1
nodes:
  - id: LLM_1
    type: llm
    input:
      - input
    input_mapping:
      system:
        type: fstring
        value: You are a helpful assistant.
      task:
        type: variable
        value: input
    output:
      - messages
    transition: END
`;

/** Node id of the single LLM node {@link PIPELINE_STARTER_TEMPLATE} ships — `entry_point` names it. */
export const PIPELINE_STARTER_ENTRY_NODE_ID = 'LLM_1';

/** A pipeline created by {@link createPipelineThroughApi}, addressed by everything a stored read needs. */
export interface CreatedPipeline {
  /** `applications.id` — the SERIAL key every pipeline route addresses it by. */
  readonly id: string;
  /** `application_versions.id` of the `base` version created alongside it. */
  readonly versionId: string;
  /** The project the row was created in — echoed back so a caller never has to assume `1`. */
  readonly projectId: string;
}

/** Options for {@link createPipelineThroughApi}. Every key has a working default. */
export interface CreatePipelineOptions {
  /** Defaults to `${name} description`. The create form requires a non-blank one, so this helper always sends one too. */
  readonly description?: string;
  /** The pipeline document. Defaults to {@link PIPELINE_STARTER_TEMPLATE}. */
  readonly instructions?: string;
  /** Defaults to `DEFAULT_PROJECT_ID` (the seeded shared project the journey personas hold a role on). */
  readonly projectId?: string;
}

/**
 * Create a pipeline over the API and return the ids a stored read needs.
 *
 * Sends the body `entities/application-form/model/mutations.ts` builds for
 * `/pipelines/create` — `type: 'interface'` at the application level and one
 * `versions[]` entry carrying `agent_type: 'pipeline'`. An application with no
 * version row is degenerate (`List` INNER JOINs `application_versions`, so it
 * never appears on any list and no deep link can open it), which is why the
 * version travels with the create rather than following it.
 *
 * `pipeline_settings` is deliberately NOT sent: `versionFromBody` reads no such
 * key and `insertVersion`'s INSERT does not name the column
 * (`internal/api/v2/applications/handler.go`, measured by
 * `pipelines.versioning.spec.ts`'s own probe), so a caller sending one would be
 * shown an empty object back and could reasonably read that as a defect here.
 *
 * Throws on a non-2xx, naming the status and body. Returning a partial object
 * instead would push the failure into whichever assertion first used a blank
 * id, which says nothing about the create.
 */
export async function createPipelineThroughApi(
  request: APIRequestContext,
  name: string,
  options: CreatePipelineOptions = {},
): Promise<CreatedPipeline> {
  const projectId = options.projectId ?? DEFAULT_PROJECT_ID;
  const url = `${API_BASE}/elitea_core/applications/prompt_lib/${projectId}`;
  const resp = await request.post(url, {
    data: {
      name,
      description: options.description ?? `${name} description`,
      type: 'interface',
      versions: [
        {
          name: 'base',
          agent_type: 'pipeline',
          instructions: options.instructions ?? PIPELINE_STARTER_TEMPLATE,
          conversation_starters: [],
          variables: [],
          meta: { step_limit: 25, internal_tools: [] },
        },
      ],
    },
  });
  if (!resp.ok()) {
    throw new Error(
      `createPipelineThroughApi: POST ${url} -> ${describeResponse(resp.status(), resp.statusText(), await resp.text())}`,
    );
  }
  const body = (await resp.json()) as { id?: unknown; version_details?: { id?: unknown; agent_type?: unknown } };
  const id = typeof body.id === 'string' || typeof body.id === 'number' ? String(body.id) : '';
  const versionId =
    typeof body.version_details?.id === 'string' || typeof body.version_details?.id === 'number'
      ? String(body.version_details.id)
      : '';
  if (id === '' || versionId === '') {
    throw new Error(
      `createPipelineThroughApi: 201 without an id/version_details.id: ${JSON.stringify(body).slice(0, 300)}`,
    );
  }
  // The discriminator, checked HERE rather than in each caller: a row stored
  // as an agent looks identical until a list filtered by `agents_type` comes
  // back without it, three steps later, in a journey about a search box.
  if (body.version_details?.agent_type !== 'pipeline') {
    throw new Error(
      `createPipelineThroughApi: the created version is not a pipeline (agent_type=${String(
        body.version_details?.agent_type,
      )}) — every pipelines route filters on that field`,
    );
  }
  return { id, versionId, projectId };
}

/** Delete a pipeline (cleanup). Best effort by design — a caller in `afterEach` must not fail the test it is cleaning up after. */
export async function deletePipeline(
  request: APIRequestContext,
  pipeline: Pick<CreatedPipeline, 'id' | 'projectId'>,
): Promise<void> {
  await request.delete(`${API_BASE}/elitea_core/application/prompt_lib/${pipeline.projectId}/${pipeline.id}`);
}
