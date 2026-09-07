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

import { API_BASE } from './api';

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
