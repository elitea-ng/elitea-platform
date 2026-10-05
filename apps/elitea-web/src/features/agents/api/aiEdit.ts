/**
 * `features/agents/api/aiEdit.ts` — the two network reads and the one write
 * the "Edit with AI" affordance needs.
 *
 * **WHY A LOCAL COPY.** `features/pipelines/api/aiAssistantConfigurations.ts`
 * and `features/pipelines/api/aiAssistantPredict.ts` already speak these
 * exact endpoints, and `.dependency-cruiser.cjs`'s `no-sideways-features`
 * forbids `features/agents` importing any `features/pipelines` file — even a
 * named symbol through its `index.ts`. Duplicating locally against the SAME
 * verified wire contract is this codebase's established answer (that
 * pipelines file is itself a local copy of `features/credentials`' version,
 * for the same rule). Per R-A5 every call below goes through `eliteaFetch`;
 * `endpoints.manifest.json` gains `features/agents` on the two existing
 * configuration entries and one new `agents.generateContentBlocking` entry.
 *
 * **BLOCKING, NOT STREAMING.** The pipelines panel uses `await_task_timeout:
 * 0` and collects the output over socket.io. This affordance produces ONE
 * rewritten block of text that the user then reviews as a diff — there is no
 * partial state worth showing — so it uses the baseline's other mode,
 * `generateContentBlocking` (`apps/elitea-ui/src/api/llm.js:18-26`), which
 * returns the content in the HTTP response and needs no socket at all.
 *
 * **THE ROUTE.** `POST /elitea_core/predict_llm/prompt_lib/{projectId}` was
 * removed by #126 — the group was gated on a `RouterConfig.Predictor` nothing
 * ever assigned, so it 404'd in every deployment before it was deleted. It is
 * served again: elitea-main now answers the BLOCKING mode by calling the LLM
 * gateway directly. The affordance stays behind
 * `hasBackendCapability('llmPredictBlocking')` as well as its own prompt/model
 * gate (see `../model/useAiEditAvailability.ts`), because a deployment with no
 * `LLM_GATEWAY_URL` still has no LLM to reach.
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';

/** Envelope-unwrap, matching every hand-registered endpoint in this codebase. */
async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

/* ── GET /configurations/available/ ──────────────────────────────────────── */

interface AiEditConfigSchemaNode {
  readonly properties?: {
    readonly data?: {
      readonly properties?: {
        readonly prompt?: {
          readonly default_by_key?: Readonly<Record<string, string>>;
        };
      };
    };
  };
  readonly [key: string]: unknown;
}

/** One credential/service-prompt TYPE descriptor. */
export interface AiEditConfigurationTypeDescriptor {
  readonly type: string;
  readonly config_schema?: AiEditConfigSchemaNode;
  readonly [key: string]: unknown;
}

export async function getServicePromptTypes(signal?: AbortSignal): Promise<AiEditConfigurationTypeDescriptor[]> {
  return fetchData<AiEditConfigurationTypeDescriptor[]>(
    '/configurations/available/?section=service_prompts',
    signal ? { signal } : {},
  );
}

/** The per-key default prompt text a type descriptor carries. */
export function servicePromptDefaultsByKey(
  types: readonly AiEditConfigurationTypeDescriptor[] | undefined,
): Readonly<Record<string, string>> {
  const merged: Record<string, string> = {};
  // Defensive: a deployment that answers this route with something other than
  // a JSON array must degrade to "no prompt resolved" (which hides the
  // affordance), not throw out of a render.
  const list: readonly AiEditConfigurationTypeDescriptor[] = Array.isArray(types) ? types : [];
  for (const descriptor of list) {
    const defaults = descriptor.config_schema?.properties?.data?.properties?.prompt?.default_by_key;
    if (defaults === undefined) continue;
    for (const [key, value] of Object.entries(defaults)) {
      if (typeof value === 'string' && value !== '') merged[key] = value;
    }
  }
  return merged;
}

/* ── GET /configurations/configurations/{projectId} (service prompts) ────── */

interface AiEditConfigurationWire {
  readonly type?: string;
  readonly data?: Readonly<Record<string, unknown>>;
  readonly elitea_title?: string;
  readonly [key: string]: unknown;
}

export interface AiEditConfigurationPageWire {
  readonly items?: readonly AiEditConfigurationWire[];
  readonly total?: number;
  readonly shared?: {
    readonly items?: readonly AiEditConfigurationWire[];
    readonly total?: number;
  };
}

const SERVICE_PROMPT_PAGE_SIZE = 100;

function buildServicePromptsUrl(projectId: string | number): string {
  const search = new URLSearchParams();
  search.append('include_shared', 'true');
  search.append('shared_offset', '0');
  search.append('shared_limit', String(SERVICE_PROMPT_PAGE_SIZE));
  search.append('limit', String(SERVICE_PROMPT_PAGE_SIZE));
  search.append('offset', '0');
  search.append('sort_by', 'created_at');
  search.append('sort_order', 'desc');
  search.append('query', '');
  search.append('section', 'service_prompts');
  return `/configurations/configurations/${String(projectId)}?${search.toString()}`;
}

export async function getServicePrompts(
  projectId: string | number,
  signal?: AbortSignal,
): Promise<AiEditConfigurationPageWire> {
  return fetchData<AiEditConfigurationPageWire>(buildServicePromptsUrl(projectId), signal ? { signal } : {});
}

/**
 * The authored prompt text for one service-prompt key, if a project (or a
 * shared configuration) has one. Matches by `data.key` first and
 * `elitea_title` second, the same two-step lookup the baseline's
 * `useServicePromptByKey` performs.
 */
export function findServicePrompt(page: AiEditConfigurationPageWire | undefined, key: string): string {
  const items: readonly AiEditConfigurationWire[] = Array.isArray(page?.items) ? page.items : [];
  const shared: readonly AiEditConfigurationWire[] = Array.isArray(page?.shared?.items) ? page.shared.items : [];
  const all = [...items, ...shared];
  const found = all.find((item) => item.data?.['key'] === key) ?? all.find((item) => item.elitea_title === key);
  const prompt = found?.data?.['prompt'];
  return typeof prompt === 'string' ? prompt : '';
}

/* ── GET /configurations/models/{projectId} (the project default model) ──── */

/** The part of the LLM model catalogue "Edit with AI" reads. */
interface AiEditModelCatalogueWire {
  readonly default_model_name?: string | null;
  /**
   * True when `default_model_name` is the default an admin configured. False
   * (or absent, on a server that predates the field) when it is only the
   * catalogue's first item.
   */
  readonly default_model_configured?: boolean;
}

/**
 * The project's CURRENT default LLM model, read when "Edit with AI" opens
 * (legacy issue 6872). The agent version's own `model_name` is the default
 * that was current when the agent was created, so it goes stale when an admin
 * changes the project default.
 */
export async function getProjectModelCatalogue(
  projectId: string | number,
  signal?: AbortSignal,
): Promise<AiEditModelCatalogueWire> {
  const params = new URLSearchParams({ section: 'llm', include_shared: 'true' });
  return fetchData<AiEditModelCatalogueWire>(`/configurations/models/${String(projectId)}?${params.toString()}`, signal ? { signal } : {});
}

function catalogueDefaultName(catalogue: AiEditModelCatalogueWire | undefined): string {
  const name = catalogue?.default_model_name;
  return typeof name === 'string' ? name.trim() : '';
}

/**
 * The CONFIGURED default model name in a catalogue answer, else `''`. A
 * project with no configured default still gets a `default_model_name`: the
 * catalogue's first item. That item is no one's choice, so it is not
 * returned here.
 */
function projectDefaultModelName(catalogue: AiEditModelCatalogueWire | undefined): string {
  return catalogue?.default_model_configured === true ? catalogueDefaultName(catalogue) : '';
}

/**
 * The model "Edit with AI" runs on, in this order (legacy issue 6872):
 *
 *  1. the project's CONFIGURED default (an admin chose it, and it is current);
 *  2. the agent version's own model (the agent's author chose it);
 *  3. the catalogue's first-item default (better than no model at all).
 */
export function aiEditModelName(catalogue: AiEditModelCatalogueWire | undefined, versionModel: string): string {
  return projectDefaultModelName(catalogue) || versionModel || catalogueDefaultName(catalogue);
}

/* ── POST /elitea_core/predict_llm/prompt_lib/{projectId} (blocking) ─────── */

export interface AiEditLlmSettings {
  readonly model_name: string;
  readonly integration_uid?: string;
  readonly temperature: number;
  readonly max_tokens: number;
}

export interface GenerateContentBlockingBody {
  readonly user_input: string;
  readonly chat_history: readonly unknown[];
  readonly llm_settings: AiEditLlmSettings;
}

/**
 * The blocking response. The baseline reads the generated text off whichever
 * of these the backend filled in, so all three are optional and the caller
 * picks the first non-empty one (`readGeneratedContent`).
 */
export interface GenerateContentBlockingResult {
  readonly error?: string;
  readonly content?: string;
  readonly response?: string;
  readonly messages?: readonly { readonly content?: string }[];
  readonly [key: string]: unknown;
}

const AWAIT_TASK_TIMEOUT_SECONDS = 60;

export async function generateContentBlocking(
  projectId: string | number,
  body: GenerateContentBlockingBody,
  signal?: AbortSignal,
): Promise<GenerateContentBlockingResult> {
  return fetchData<GenerateContentBlockingResult>(`/elitea_core/predict_llm/prompt_lib/${String(projectId)}`, {
    method: 'POST',
    body: JSON.stringify({ ...body, await_task_timeout: AWAIT_TASK_TIMEOUT_SECONDS }),
    headers: { 'Content-Type': 'application/json' },
    ...(signal ? { signal } : {}),
  });
}

/** First non-empty text the blocking response carries, or `''`. */
export function readGeneratedContent(result: GenerateContentBlockingResult | undefined): string {
  if (result === undefined) return '';
  if (typeof result.content === 'string' && result.content.trim() !== '') return result.content;
  if (typeof result.response === 'string' && result.response.trim() !== '') return result.response;
  for (const message of result.messages ?? []) {
    if (typeof message.content === 'string' && message.content.trim() !== '') return message.content;
  }
  return '';
}
