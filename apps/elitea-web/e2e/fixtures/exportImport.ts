/**
 * Export/import round-trip helpers for the API journeys under
 * `e2e/journeys/api/`.
 *
 * ## Where these came from
 *
 * They are the port of `automation/tests/api/export_import/`'s module-level
 * helpers in `EliteaAI/elitea-testing-public` — `_parse_exported_md`,
 * `_build_import_payload`, `_build_import_payload_from_export`,
 * `_create_agent_with_fields`, `_create_pipeline_with_nodes`, the pipeline node
 * builders and the two `assert_*_fields_match` comparers. That suite is the
 * only regression net the platform has over the export/import contract, and it
 * caught two real Go defects on its first run against this stack (issues 844
 * and 845). Porting the helpers rather than the runner is what the
 * legacy-suite assessment recommends: the assertions are about the API, the
 * pytest fixtures around them are about `stage.elitea.ai`.
 *
 * ## Why the round trip is worth a journey at all
 *
 * Every step of it is a different subsystem, and each one can lose data while
 * answering 2xx:
 *
 *  - the EXPORT renders one markdown file per version out of five tables;
 *  - the client PARSES that file — the format is the contract, not the code;
 *  - the IMPORT re-derives rows from the parse, through a wizard that reports
 *    per-entity errors in a body it still answers 201 with.
 *
 * A unit test on any one of the three cannot see the loss, because each half
 * is correct about the shape it owns. Only the trip catches it, which is why
 * the legacy suite is organised this way.
 *
 * ## The one thing these helpers must never do
 *
 * They must not translate a value on the way back in. The legacy suite's own
 * `_build_import_payload_from_export` renamed `agent_type: agent` to `openai`
 * before posting, quietly reproducing what the web client does in its file
 * parser — and that rename is exactly what hid issue 845 for as long as it did.
 * `buildImportPayloadFromExport` below posts the frontmatter's own value.
 */
import type { APIRequestContext, APIResponse } from '@playwright/test';

import { dump as dumpYaml, load as loadYaml } from 'js-yaml';

import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, describeRefusal } from './api';

/* ── the shapes these journeys read ──────────────────────────────────────── */

/** One `versions[]` entry / `version_details` of the application detail response. */
export interface StoredVersion {
  readonly id?: string;
  readonly name?: string;
  readonly agent_type?: string;
  readonly instructions?: string;
  readonly welcome_message?: string;
  readonly llm_settings?: Readonly<Record<string, unknown>>;
  readonly conversation_starters?: readonly unknown[];
  readonly variables?: readonly { readonly name: string; readonly value?: string }[];
  readonly meta?: Readonly<Record<string, unknown>>;
}

/** `GET /elitea_core/application/prompt_lib/{project}/{id}`. */
export interface StoredApplication {
  readonly id: string;
  readonly name: string;
  readonly description?: string;
  readonly versions?: readonly StoredVersion[];
  readonly version_details?: StoredVersion;
}

/** `POST /elitea_core/import_wizard/prompt_lib/{project}`. */
export interface ImportWizardResult {
  readonly result?: {
    readonly agents?: readonly { readonly id: string; readonly name?: string }[];
  };
  readonly errors?: {
    readonly agents?: readonly { readonly index: number; readonly name: string; readonly msg: string }[];
  };
}

/** An exported `.agent.md` split into its two halves. */
export interface ExportedMarkdown {
  readonly frontmatter: Readonly<Record<string, unknown>>;
  /** The markdown body — the agent's instructions, and `''` for a pipeline. */
  readonly body: string;
}

/* ── the document format ─────────────────────────────────────────────────── */

/**
 * Split an exported `.agent.md` into parsed frontmatter and body.
 *
 * NOT `text.split('---', 3)`: JavaScript's `split` with a limit DISCARDS the
 * remainder instead of keeping it as the last element the way Python's
 * `maxsplit` does, so the direct transcription of the legacy helper would have
 * returned an empty body for every file and every instructions assertion in
 * these journeys would have compared '' to ''.
 */
export function parseExportedMarkdown(text: string): ExportedMarkdown {
  const opening = '---\n';
  if (!text.startsWith(opening)) {
    throw new Error(`the export does not open with YAML frontmatter: ${text.slice(0, 120)}`);
  }
  const closing = text.indexOf('\n---', opening.length);
  if (closing < 0) {
    throw new Error(`the export has no closing --- delimiter: ${text.slice(0, 200)}`);
  }
  const frontmatter = loadYaml(text.slice(opening.length, closing + 1));
  if (frontmatter === null || typeof frontmatter !== 'object') {
    throw new Error(`the exported frontmatter is not a YAML mapping: ${String(frontmatter)}`);
  }
  return {
    frontmatter: frontmatter as Record<string, unknown>,
    body: text.slice(closing + '\n---'.length).trim(),
  };
}

/**
 * Collapse runs of three or more newlines, and trim.
 *
 * The port of the legacy `_normalize_ws`. It exists for the large-instructions
 * case: the comparison is about whether 5 000 characters SURVIVED, not about
 * whether a blank line moved, and a byte-exact assertion there reports a
 * whitespace difference as data loss.
 */
export function normaliseWhitespace(value: string): string {
  return value.replace(/\n{3,}/g, '\n\n').trim();
}

/** `{name: value}` out of a version's variables list, the shape the comparisons use. */
export function variablesAsRecord(version: StoredVersion | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const variable of version?.variables ?? []) out[variable.name] = variable.value ?? '';
  return out;
}

/** The default version of an application detail: `version_details`, else `versions[0]`. */
export function defaultVersionOf(application: StoredApplication): StoredVersion {
  return application.version_details ?? application.versions?.[0] ?? {};
}

/* ── pipeline node builders (the legacy `_llm_node` family) ──────────────── */

/**
 * The node ids these journeys use carry NO SPACE, unlike the legacy suite's
 * `"LLM 1"`.
 *
 * The Rust pipeline compiler's `valid_graph_id` admits ASCII alphanumerics plus
 * `_ - . :` (`services/elitea-worker-rust/src/agents/graph/yaml.rs`), so a
 * pipeline named the legacy way survives the round trip and cannot run — the
 * exact defect `e2e/fixtures/pipelines.ts`'s `COMPILER_LEGAL_NODE_ID` exists
 * to catch. A fixture that mints an unrunnable pipeline teaches the suite to
 * accept one.
 */
export const LLM_NODE = 'LLM_1';
export const SECOND_LLM_NODE = 'LLM_2';
export const PRINTER_NODE = 'Printer_1';
export const DECISION_NODE = 'Decision_1';
export const ROUTER_NODE = 'Router_1';
export const CODE_NODE = 'Code_1';

/** One node of a pipeline document. */
export type PipelineNode = Record<string, unknown>;

export function llmNode(id: string, transition = 'END'): PipelineNode {
  return {
    id,
    type: 'llm',
    input: [],
    input_mapping: {
      chat_history: { type: 'fixed', value: [] },
      system: { type: 'fixed', value: '' },
      task: { type: 'fixed', value: '' },
    },
    output: [],
    structured_output: false,
    transition,
  };
}

export function printerNode(id: string, transition = 'END'): PipelineNode {
  return {
    id,
    type: 'printer',
    input: [],
    input_mapping: { text: { type: 'fixed', value: 'output' } },
    output: [],
    transition,
  };
}

export function decisionNode(id: string, transitions: Record<string, string>): PipelineNode {
  return {
    id,
    type: 'decision',
    input: [],
    input_mapping: { condition: { type: 'fixed', value: 'True' } },
    output: [],
    transitions,
  };
}

export function routerNode(id: string, routes: readonly Record<string, string>[]): PipelineNode {
  return {
    id,
    type: 'router',
    input: [],
    input_mapping: { task: { type: 'fixed', value: '' } },
    output: [],
    routes,
  };
}

export function codeNode(id: string, transition = 'END'): PipelineNode {
  return {
    id,
    type: 'code',
    input: [],
    input_mapping: { code: { type: 'fixed', value: "result = 'hello'" } },
    output: [],
    transition,
  };
}

/** The YAML document a pipeline version stores in `instructions`. */
export function pipelineInstructions(entryPoint: string, nodes: readonly PipelineNode[]): string {
  return dumpYaml({ entry_point: entryPoint, nodes });
}

/** Parse a stored pipeline document back into a comparable object. */
export function parsePipelineInstructions(instructions: string | undefined): Record<string, unknown> {
  if (instructions === undefined || instructions === '') return {};
  const parsed = loadYaml(instructions);
  return parsed === null || typeof parsed !== 'object' ? {} : (parsed as Record<string, unknown>);
}

/* ── the API calls ───────────────────────────────────────────────────────── */

/** The model these journeys write into every version they create. */
export const FIXTURE_MODEL = 'gpt-4o-mini';

/** What `createApplication` was asked to write, so a caller can assert against its own input. */
export interface ApplicationFixture {
  readonly name: string;
  readonly description?: string;
  readonly instructions?: string;
  readonly agentType?: string;
  readonly temperature?: number;
  readonly maxTokens?: number;
  readonly variables?: readonly { readonly name: string; readonly value: string }[];
  readonly welcomeMessage?: string;
  readonly conversationStarters?: readonly string[];
}

function applicationURL(id: string): string {
  return `${API_BASE}/elitea_core/application/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`;
}

async function refuse(what: string, method: string, url: string, resp: APIResponse): Promise<never> {
  throw new Error(
    `${what}: ${method} ${url} -> ${resp.status()} ${resp.statusText()}` +
      `${await describeRefusal(resp)}\n${(await resp.text()).slice(0, 400)}`,
  );
}

/**
 * Create an application (agent or pipeline) through the public API, with full
 * field control — the port of `_create_agent_with_fields` and
 * `_create_pipeline_with_nodes`.
 */
export async function createApplication(
  request: APIRequestContext,
  fixture: ApplicationFixture,
): Promise<string> {
  const url = `${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}`;
  const resp = await request.post(url, {
    data: {
      name: fixture.name,
      description: fixture.description ?? '',
      type: 'agent',
      versions: [
        {
          name: 'base',
          tags: [],
          agent_type: fixture.agentType ?? 'openai',
          instructions: fixture.instructions ?? '',
          welcome_message: fixture.welcomeMessage ?? '',
          variables: fixture.variables ?? [],
          tools: [],
          conversation_starters: fixture.conversationStarters ?? [],
          llm_settings: {
            model_name: FIXTURE_MODEL,
            temperature: fixture.temperature ?? 0.7,
            max_tokens: fixture.maxTokens ?? 1024,
          },
          meta: { step_limit: 25 },
        },
      ],
    },
  });
  if (!resp.ok()) await refuse('createApplication', 'POST', url, resp);
  const body = (await resp.json()) as { id?: unknown };
  if (typeof body.id !== 'string') {
    throw new Error(`createApplication: the create answered no id: ${JSON.stringify(body).slice(0, 300)}`);
  }
  return body.id;
}

/** Read one application back. Throws on any non-2xx, naming the refusal. */
export async function readApplication(
  request: APIRequestContext,
  id: string,
): Promise<StoredApplication> {
  const url = applicationURL(id);
  const resp = await request.get(url);
  if (!resp.ok()) await refuse('readApplication', 'GET', url, resp);
  return (await resp.json()) as StoredApplication;
}

/** `true` when the application is gone — the read the delete polls against. */
export async function applicationIsGone(request: APIRequestContext, id: string): Promise<boolean> {
  const resp = await request.get(applicationURL(id));
  return !resp.ok();
}

/** Export one application as markdown, parsed. Returns the raw text too, because the format IS the contract. */
export async function exportMarkdown(
  request: APIRequestContext,
  id: string,
): Promise<{ readonly text: string; readonly parsed: ExportedMarkdown }> {
  const url =
    `${API_BASE}/elitea_core/export_import/prompt_lib/${DEFAULT_PROJECT_ID}/${id}?format=md`;
  const resp = await request.get(url);
  if (!resp.ok()) await refuse('exportMarkdown', 'GET', url, resp);
  const text = await resp.text();
  return { text, parsed: parseExportedMarkdown(text) };
}

/** Delete an application. Best effort by design — it runs in a `finally`. */
export async function deleteApplication(request: APIRequestContext, id: string): Promise<void> {
  await request.delete(applicationURL(id));
}

/** One entry of an import-wizard body. */
export interface ImportVersionInput {
  readonly instructions: string;
  readonly agentType: unknown;
  readonly modelName?: unknown;
  readonly temperature?: unknown;
  readonly maxTokens?: unknown;
  readonly stepLimit?: unknown;
  readonly variables?: readonly { readonly name: string; readonly value: string }[];
  readonly welcomeMessage?: unknown;
  readonly conversationStarters?: readonly unknown[];
}

/**
 * The import-wizard body, the port of `_build_import_payload`.
 *
 * `entity: "agents"` for both kinds: agents and pipelines share the import
 * handler, and `versions[0].agent_type` is what tells them apart.
 */
export function buildImportPayload(
  name: string,
  description: string,
  version: ImportVersionInput,
): unknown[] {
  const unique = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
  return [
    {
      name,
      description,
      original_exported: true,
      import_uuid: `${AUTOTEST_PREFIX}${unique}`,
      entity: 'agents',
      versions: [
        {
          name: 'base',
          import_version_uuid: `${AUTOTEST_PREFIX}${unique}-base`,
          instructions: version.instructions,
          agent_type: version.agentType,
          llm_settings: {
            model_name: version.modelName ?? FIXTURE_MODEL,
            temperature: version.temperature ?? 0.7,
            max_tokens: version.maxTokens ?? 1024,
          },
          meta: { step_limit: version.stepLimit ?? 25, internal_tools: [] },
          tools: [],
          variables: version.variables ?? [],
          conversation_starters: version.conversationStarters ?? [],
          welcome_message: version.welcomeMessage ?? '',
          tags: [],
        },
      ],
    },
  ];
}

/**
 * The import-wizard body derived from an exported file, WITHOUT translating a
 * single value — see this module's header for why that matters.
 *
 * A pipeline's document lives in the frontmatter (`entry_point`, `nodes`) and
 * its body is empty, so the instructions are rebuilt from those two keys; an
 * agent's instructions ARE the body.
 */
export function buildImportPayloadFromExport(exported: ExportedMarkdown): unknown[] {
  const front = exported.frontmatter;
  const nodes = front['nodes'];
  const entryPoint = front['entry_point'];
  const instructions =
    entryPoint === undefined && nodes === undefined
      ? exported.body
      : pipelineInstructions(String(entryPoint ?? ''), (nodes ?? []) as PipelineNode[]);

  const exportedVariables = (front['variables'] ?? []) as readonly { name: string; value?: string }[];
  return buildImportPayload(String(front['name'] ?? ''), String(front['description'] ?? ''), {
    instructions,
    // Verbatim. The exporter writes `agent` for a stored `openai` agent, and
    // the whole point of this journey is that the API accepts its own output.
    agentType: front['agent_type'],
    modelName: front['model'],
    temperature: front['temperature'],
    maxTokens: front['max_tokens'],
    stepLimit: front['step_limit'],
    variables: exportedVariables.map((variable) => ({ name: variable.name, value: variable.value ?? '' })),
    // The legacy helper carried NEITHER of these two back, and then asserted
    // them against the application detail's TOP level, where neither key
    // exists — so both comparisons read `'' === ''` and `[] === []` and
    // proved nothing. They are carried, and asserted, here.
    welcomeMessage: front['welcome_message'] ?? '',
    conversationStarters: (front['conversation_starters'] ?? []) as readonly unknown[],
  });
}

/** POST an import-wizard body. Returns the decoded answer whatever the status, because the errors live IN it. */
export async function importWizard(
  request: APIRequestContext,
  payload: unknown[],
): Promise<{ readonly status: number; readonly body: ImportWizardResult; readonly text: string }> {
  const url = `${API_BASE}/elitea_core/import_wizard/prompt_lib/${DEFAULT_PROJECT_ID}`;
  const resp = await request.post(url, { data: payload });
  const text = await resp.text();
  let body: ImportWizardResult = {};
  try {
    body = JSON.parse(text) as ImportWizardResult;
  } catch {
    throw new Error(`importWizard: POST ${url} -> ${resp.status()} answered non-JSON: ${text.slice(0, 300)}`);
  }
  return { status: resp.status(), body, text };
}

/**
 * The id of the single agent an import created, with the wizard's own error
 * channel read first.
 *
 * The wizard answers 201 with a per-entity `errors.agents` list, so a caller
 * that only checked the status would report "imported" for an import that
 * wrote nothing. That is the failure mode the legacy suite guards on every one
 * of its round trips.
 */
export function importedAgentId(answer: {
  readonly status: number;
  readonly body: ImportWizardResult;
  readonly text: string;
}): string {
  const errors = answer.body.errors?.agents ?? [];
  if (errors.length > 0) {
    throw new Error(`the import reported errors: ${JSON.stringify(errors)}`);
  }
  const agents = answer.body.result?.agents ?? [];
  if (agents.length !== 1) {
    throw new Error(
      `the import created ${String(agents.length)} agents, want 1: ${answer.text.slice(0, 400)}`,
    );
  }
  const id = agents[0]?.id;
  if (typeof id !== 'string') {
    throw new Error(`the import answered an agent with no id: ${answer.text.slice(0, 300)}`);
  }
  return id;
}
