/**
 * The four network seams `lib/hooks/useToolkitChat.types.ts` declares as
 * injected params (`createConversation`, `addParticipant`, `modelList`/
 * `defaultModel`, `stopIndexing`) — supplied here, against real routes.
 *
 * CORRECTION (2026-08-09). `useToolkitChat.types.ts`'s own comments call
 * three of these "no generated … endpoint" gaps, and `IndexChat.tsx`/
 * `IndexConfig.tsx` say the components they need "do not exist anywhere in
 * this worktree". Those comments are STALE — every one of them was written
 * before the routes and components landed. Re-measured against the running
 * standalone stack on 2026-08-09 (`deploy/docker-compose.e2e-standalone.yml`,
 * `http://localhost:8082`), all authenticated as the E2E member persona:
 *
 *   POST /api/v2/elitea_core/conversations/prompt_lib/{projectID}
 *        — services/elitea-main/internal/api/router.go:775 (convHandler.Create)
 *   POST /api/v2/elitea_core/participants/prompt_lib/{projectID}/{conversationID}
 *        — router.go:782 (convHandler.AddParticipant)
 *   GET  /api/v2/configurations/models/{projectID}?section=llm&include_shared=true
 *        — answered 200 {"items":[],"total":0} (empty on that stack, route real)
 *   DELETE /api/v2/elitea_core/index_cancel/prompt_lib/{projectID}/{toolkitID}/{indexName}/{taskID}
 *        — router.go:710/735, and this slice ALREADY wraps it in
 *          `indexes/api/indexesApi.ts`'s `useStopIndexingItemMutation`, so
 *          `stopIndexing` needs nothing new here at all (see `IndexesTab.tsx`).
 *
 * The model fetcher below is a near-duplicate of `features/chat-input/api/
 * models.ts` (same route, `section` narrowed there to `'asr' | 'tts'`).
 * `no-sideways-features` forbids importing it, and no `shared`/`entities`
 * model-listing primitive has been promoted yet — the same disclosed
 * duplication that file's own module doc already records for its own copy.
 */
import { useQuery } from '@tanstack/react-query';
import type { UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';

import type { AddParticipantResult, CreateConversationResult } from '../lib/helpers/toolkitConversation.helpers';

async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

const JSON_HEADERS = { 'Content-Type': 'application/json' } as const;

/**
 * `createToolkitConversationWithParticipant`'s `createConversation` seam.
 * That helper puts `projectId` INSIDE the same object as the conversation
 * body (see its own `createConversation({... projectId})` call), so this
 * function splits it back out for the URL and posts the rest.
 */
export async function createToolkitConversation(input: Readonly<Record<string, unknown>>): Promise<CreateConversationResult> {
  const { projectId, ...body } = input;
  const data = await fetchData<NonNullable<CreateConversationResult['data']> | undefined>(
    `/elitea_core/conversations/prompt_lib/${String(projectId)}`,
    { method: 'POST', body: JSON.stringify(body), headers: JSON_HEADERS },
  );
  // `createToolkitConversationWithParticipant` short-circuits on a missing
  // `data` (its own documented `if (!conversationResult.data) return null`),
  // so the key is omitted rather than set to `undefined`
  // (`exactOptionalPropertyTypes`).
  return data === undefined ? {} : { data };
}

/**
 * `createToolkitConversationWithParticipant`'s `addParticipant` seam — `{projectId, id, participants}`.
 *
 * The route takes the participant ARRAY as the whole body (or one bare
 * participant object), the same as legacy `chat.api.js` and
 * `participantApi.addParticipantIntoConversation`. A `{participants: [...]}`
 * wrapper reads as ONE participant with no `entity_meta`, and the server
 * refuses it with 400.
 */
export async function addToolkitConversationParticipant(input: Readonly<Record<string, unknown>>): Promise<AddParticipantResult> {
  const { projectId, id, participants } = input;
  const data = await fetchData<NonNullable<AddParticipantResult['data']> | undefined>(
    `/elitea_core/participants/prompt_lib/${String(projectId)}/${String(id)}`,
    { method: 'POST', body: JSON.stringify(participants), headers: JSON_HEADERS },
  );
  return data === undefined ? {} : { data };
}

/**
 * One LLM model row. Structurally compatible with BOTH
 * `useToolkitChat.types.ts`'s `ToolkitChatModel` and
 * `widgets/llm-model-selector`'s `LLMModel` — the composition root hands the
 * same array to both, so neither may be narrowed here.
 */
export interface ToolkitLlmModel {
  readonly id: string;
  readonly name: string;
  readonly display_name?: string;
  readonly project_id?: string | number;
  readonly shared?: boolean;
  readonly supports_reasoning?: boolean;
  readonly [key: string]: unknown;
}

/** One row exactly as the endpoint answers it — no synthesized `id` yet (see {@link withModelId}). */
interface LlmModelWire {
  readonly name: string;
  readonly display_name?: string;
  readonly project_id?: string | number;
  readonly shared?: boolean;
  readonly supports_reasoning?: boolean;
  readonly [key: string]: unknown;
}

interface LlmModelListWire {
  readonly items?: readonly LlmModelWire[];
  readonly total?: number;
  readonly default_model_name?: string;
  readonly default_model_project_id?: string;
}

export interface ToolkitLlmModelList {
  readonly models: readonly ToolkitLlmModel[];
  readonly defaultModel: ToolkitLlmModel | null;
}

const EMPTY_MODEL_LIST: ToolkitLlmModelList = { models: [], defaultModel: null };

/**
 * Synthesizes `id` exactly the way `features/chat-input/api/models.ts`'s
 * `withModelId` does — bare template-literal coercion, so a row with no
 * `project_id` yields `"undefined_<name>"`. Kept identical deliberately:
 * two independently-synthesized id schemes for the same rows would break
 * any future comparison across the two call sites.
 */
function withModelId(row: LlmModelWire): ToolkitLlmModel {
  return { ...row, name: row.name, id: `${String(row.project_id)}_${row.name}` };
}

function selectDefaultModel(models: readonly ToolkitLlmModel[], wire: LlmModelListWire): ToolkitLlmModel | null {
  const byName = models.find(
    (model) => model.name === wire.default_model_name && String(model.project_id) === String(wire.default_model_project_id),
  );
  return byName ?? models.find((model) => model['default'] === true) ?? null;
}

/**
 * The `section` values this endpoint recognises, for the three model-picker
 * field kinds `ToolBaseProperty` dispatches
 * (`ToolBaseProperty.kinds.ts`'s `CREDENTIAL_LIKE_TYPES`). Baseline:
 * `components/LlmModelSelect.jsx` / `EmbeddingModelSelect.jsx` /
 * `ImageGenerationModelSelect.jsx` each pin their own literal on the same
 * `useListModelsQuery({section})` call.
 */
export type ToolkitModelSection = 'llm' | 'embedding' | 'image_generation';

/**
 * One project's models for a given section. #308 — generalised from the
 * llm-only `useToolkitLlmModels` below (which now delegates here) so the
 * embedding- and image-generation-model form fields have a data source: the
 * response shape is identical across sections, only the query param differs.
 */
export function useToolkitModels(
  projectId: string | undefined,
  section: ToolkitModelSection,
): UseQueryResult<ToolkitLlmModelList> {
  return useQuery({
    queryKey: ['toolkits', 'models', section, projectId],
    queryFn: async ({ signal }): Promise<ToolkitLlmModelList> => {
      const wire = await fetchData<LlmModelListWire>(
        `/configurations/models/${String(projectId)}?section=${section}&include_shared=true`,
        { signal },
      );
      const models = (wire.items ?? []).map(withModelId);
      return { models, defaultModel: selectDefaultModel(models, wire) };
    },
    enabled: projectId !== undefined,
    // A toolkit with no configured LLM must still show its index list; the
    // chat panel simply has no model to pick. Never let this reject the
    // whole tab.
    placeholderData: EMPTY_MODEL_LIST,
  });
}

export function useToolkitLlmModels(projectId: string | undefined): UseQueryResult<ToolkitLlmModelList> {
  return useToolkitModels(projectId, 'llm');
}
