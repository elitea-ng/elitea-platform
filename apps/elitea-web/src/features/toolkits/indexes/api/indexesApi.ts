/**
 * TanStack Query port of `apps/elitea-ui/src/[fsd]/features/toolkits/
 * indexes/api/indexesApi.js` (unit A4a's RTK Query endpoints). Query-key
 * based caching replaces `providesTags`; explicit `invalidateQueries` calls
 * replace `invalidatesTags` — this app has no Redux/RTK Query anywhere
 * (spec §2.3: "TanStack Query + zustand"), matching the established pattern
 * in e.g. `features/credentials/api/useConfigurations.ts`.
 *
 * `updateIndexSchedule`/`getIndexSchedule`/`deleteIndexItem` additionally
 * write into `../model/indexesStore.ts`'s `toolkitScheduler` map on
 * success — the TanStack-era replacement for the baseline slice's
 * `getIndexSchedule.matchFulfilled`/`deleteIndexItem.matchFulfilled`
 * `extraReducers` matchers (see that file's own doc comment).
 *
 * Also carries `getIndexHistoryConversationDetails` — a LOCAL, indexes-
 * scoped duplicate of `entities/run-history`'s `getRunHistoryDetails`
 * endpoint (`apps/elitea-ui/src/[fsd]/entities/run-history/api/
 * runHistoryApi.js:38-46`). `entities/run-history` does not exist anywhere
 * in this app yet (confirmed: no `src/entities/run-history` directory in
 * this worktree, and it is not named as an A4a dependency in this unit's
 * brief — only `useToolkitChat`/`ToolkitChatModesEnum` (A4b) and `features/
 * mcps` (A5) are). It is also not indexes-specific (agents/pipelines run
 * history need the identical endpoint), so — same class of decision as
 * `useSelectedProjectId.ts` — this is a scoped-down local duplicate (one
 * endpoint of the baseline entity's three) rather than an invented
 * cross-domain promotion this sub-unit has no mandate to make.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { UseMutationResult, UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import type { MessageGroupWire } from '@/entities/message';

import type { ConversationParticipantWire } from '../lib/helpers/conversationHistory.local';
import type { IndexRow, ScheduleEntry } from '../model/indexesStore';
import { useIndexesStore } from '../model/indexesStore';

/**
 * `fetchData<T>` resolves to orval's enveloped shape (`{data: T, status,
 * headers}`) — matching `features/credentials/api/configurations.ts`'s own
 * documented reason for this one-place unwrap.
 */
async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

const INDEXES_QUERY_ROOT = ['toolkits', 'indexes'] as const;
const INDEX_SCHEDULE_QUERY_ROOT = ['toolkits', 'indexSchedule'] as const;

/* ── getIndexesList — GET elitea_core/index_meta/prompt_lib/{projectId}/{toolkitId} ── */

export interface GetIndexesListParams {
  readonly toolkitId: string | undefined;
  readonly projectId: string | number | undefined;
}

export async function getIndexesList(params: GetIndexesListParams, signal?: AbortSignal): Promise<IndexRow[]> {
  const { toolkitId, projectId } = params;
  const payload = await fetchData<unknown>(`/elitea_core/index_meta/prompt_lib/${String(projectId)}/${String(toolkitId)}`, signal ? { signal } : {});
  /*
   * Shape guard, added 2026-08-09 when mounting this slice (#149) took down
   * the whole toolkit page with `e.map is not a function`. This endpoint's
   * success path writes a bare JSON array
   * (`services/elitea-main/internal/api/v2/toolkits/handler.go`'s `IndexMeta`),
   * but its error path used to write `{"items": [], "total": 0}` at HTTP 200 —
   * a different shape, indistinguishable from success. That handler is fixed
   * in the same change (it now answers 500), so this is no longer load-
   * bearing for that specific bug.
   *
   * It stays because the failure mode it prevents is disproportionate: every
   * downstream consumer (`mergeIndexesOverlay`, `IndexesList`) assumes an
   * array, and a single non-array payload from ANY future shape drift takes
   * out the entire page through the error boundary rather than degrading to
   * an empty list. Narrow on purpose — it coerces only "not an array" and
   * does not inspect or repair row contents.
   */
  return Array.isArray(payload) ? (payload as IndexRow[]) : [];
}

export function useIndexesListQuery(params: GetIndexesListParams): UseQueryResult<IndexRow[]> {
  const { toolkitId, projectId } = params;
  return useQuery({
    queryKey: [...INDEXES_QUERY_ROOT, 'list', toolkitId, projectId],
    queryFn: ({ signal }) => getIndexesList(params, signal),
    enabled: toolkitId !== undefined && projectId !== undefined,
  });
}

/* ── deleteIndexItem — DELETE elitea_core/index_meta/prompt_lib/{projectId}/{toolkitId}/{indexId} ── */

export interface DeleteIndexItemParams {
  readonly projectId: string | number;
  readonly toolkitId: string;
  readonly indexId: string;
  readonly indexName: string;
}

export async function deleteIndexItem(params: DeleteIndexItemParams): Promise<unknown> {
  const { projectId, toolkitId, indexId } = params;
  return fetchData<unknown>(`/elitea_core/index_meta/prompt_lib/${String(projectId)}/${toolkitId}/${indexId}`, {
    method: 'DELETE',
    body: JSON.stringify({ is_hidden: true }),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useDeleteIndexItemMutation(): UseMutationResult<unknown, Error, DeleteIndexItemParams> {
  const queryClient = useQueryClient();
  const removeToolkitSchedule = useIndexesStore((state) => state.removeToolkitSchedule);
  return useMutation({
    mutationFn: deleteIndexItem,
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({ queryKey: INDEXES_QUERY_ROOT });
      removeToolkitSchedule(variables.indexName);
    },
  });
}

/* ── stopIndexingItem — DELETE elitea_core/index_cancel/prompt_lib/{projectId}/{toolkitId}/{indexName}/{taskId} ── */

export interface StopIndexingItemParams {
  readonly projectId: string | number;
  readonly toolkitId: string;
  readonly indexName: string;
  readonly taskId: string;
}

export async function stopIndexingItem(params: StopIndexingItemParams): Promise<unknown> {
  const { projectId, toolkitId, indexName, taskId } = params;
  return fetchData<unknown>(
    `/elitea_core/index_cancel/prompt_lib/${String(projectId)}/${toolkitId}/${indexName}/${taskId}`,
    { method: 'DELETE' },
  );
}

export function useStopIndexingItemMutation(): UseMutationResult<unknown, Error, StopIndexingItemParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: stopIndexingItem,
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: INDEXES_QUERY_ROOT }),
  });
}

/* ── startIndexExecution — POST elitea_core/test_toolkit_tool/prompt_lib/{projectId} ── */

/** The `execution_contract` query value the index-ingest capability is admitted under (Go: `index.ingest.v1`). Module-private: only `startIndexExecution` sends it. */
const INDEX_INGEST_EXECUTION_CONTRACT = 'index.ingest.v1';

export interface StartIndexExecutionParams {
  readonly projectId: string | number | undefined;
  /** Resolved into the REQUIRED `toolkit_config.toolkit_id` — the Go handler 422s without a positive one. */
  readonly toolkitId: string;
  readonly toolParams: Readonly<Record<string, unknown>>;
  /** Optional model name — Go's `requestedLLMModel` wants a JSON string. */
  readonly llmModel?: string;
  /** Optional settings object — Go defaults to `{max_tokens:1024,temperature:0.1}` when absent. */
  readonly llmSettings?: Readonly<Record<string, unknown>>;
}

/**
 * The dispatch half of the SSE index-run contract (issue #93): start the run
 * over REST and get back the execution id whose event stream carries the
 * progress frames the socket room used to.
 *
 * THE BODY IS THE GO CONTRACT, not the socket predict payload. Verified
 * field by field against `services/elitea-main/internal/api/v2/indexing/
 * start_handler.go`'s `currentStartBody`:
 *   toolkit_config  REQUIRED, must resolve a positive `toolkit_id` (or `id`)
 *   tool_name       REQUIRED, must be exactly `index_data`
 *   tool_params     defaults to `{}` server-side when empty
 *   llm_model       optional JSON string
 *   llm_settings    optional object
 * The earlier draft of this function spread the socket `chat_predict`
 * payload instead, which carries no `toolkit_config` at all — a guaranteed
 * 422 that the caller's catch turned into a silent socket fallback, i.e.
 * dead code. Keep this body aligned with that Go struct.
 *
 * `await_response=false` is what makes the call return immediately with an
 * id instead of blocking on the whole run.
 *
 * Graceful degradation is the CALLER's job, deliberately: a backend without
 * this route (404) or one that answers without a `task_id` both mean "no SSE
 * execution to follow". Note that a `task_id` alone does NOT prove the Go
 * runtime answered — legacy pylon returns one too — so the caller must also
 * treat a stream that fails to open as a fallback signal (see
 * `../../lib/hooks/useToolkitChatDispatch.hooks.ts`).
 */
export interface StartIndexExecutionResult {
  readonly task_id?: string;
  readonly [key: string]: unknown;
}

/**
 * #311, the save-path half. `ToolFormContainer.resolveFieldValue` (already
 * fixed) keeps a cleared field's key PRESENT with value `undefined`, so the
 * form layer can tell "cleared" apart from "never set". Plain
 * `JSON.stringify` does not carry that distinction over the wire: it drops
 * every key whose value is `undefined`, so a cleared `tool_params` field
 * never reaches the Go handler (`start_handler.go` decodes `tool_params` as
 * raw JSON, byte for byte). The index's saved configuration then comes back
 * from a later fetch one key short — indistinguishable, again, from "never
 * set" — and `resolveFieldValue`'s own `key in toolInputVariables` guard
 * reapplies `property.default`, the exact defect #311 reports, one layer
 * down.
 *
 * This replacer turns an `undefined` value into an explicit `null` before
 * serialization, so the key survives. `null` is not `undefined`, so both
 * `resolveFieldValue` and `computeDefaultConfigValues`
 * (`../ui/IndexDetails/IndexDetails.helpers.ts`) already treat a returned
 * `null` as "present, deliberately cleared" and leave it alone.
 */
function preserveExplicitClears(_key: string, value: unknown): unknown {
  return value === undefined ? null : value;
}

export async function startIndexExecution(params: StartIndexExecutionParams): Promise<StartIndexExecutionResult> {
  const { projectId, toolkitId, toolParams, llmModel, llmSettings } = params;
  const query = `?await_response=false&execution_contract=${encodeURIComponent(INDEX_INGEST_EXECUTION_CONTRACT)}`;
  return fetchData<StartIndexExecutionResult>(`/elitea_core/test_toolkit_tool/prompt_lib/${String(projectId)}${query}`, {
    method: 'POST',
    body: JSON.stringify(
      {
        toolkit_config: { toolkit_id: toolkitId },
        tool_name: 'index_data',
        tool_params: toolParams,
        ...(llmModel !== undefined ? { llm_model: llmModel } : {}),
        ...(llmSettings !== undefined ? { llm_settings: llmSettings } : {}),
      },
      preserveExplicitClears,
    ),
    headers: { 'Content-Type': 'application/json' },
  });
}

/* ── updateIndexSchedule — PATCH elitea_core/index_meta/prompt_lib/{projectId}/{toolkitId}/{indexName} ── */

export interface UpdateIndexScheduleParams {
  readonly projectId: string | number;
  readonly toolkitId: string;
  readonly indexName: string;
  readonly timezone: string;
  readonly cron?: string;
  readonly enabled?: boolean;
  readonly credentials?: unknown;
}

export async function updateIndexSchedule(params: UpdateIndexScheduleParams): Promise<unknown> {
  const { projectId, toolkitId, indexName, ...body } = params;
  return fetchData<unknown>(`/elitea_core/index_meta/prompt_lib/${String(projectId)}/${toolkitId}/${indexName}`, {
    method: 'PATCH',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useUpdateIndexScheduleMutation(): UseMutationResult<unknown, Error, UpdateIndexScheduleParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: updateIndexSchedule,
    onSuccess: (_data, variables) =>
      void queryClient.invalidateQueries({ queryKey: [...INDEX_SCHEDULE_QUERY_ROOT, variables.toolkitId] }),
  });
}

/* ── saveIndexConfiguration — PUT elitea_core/index_meta/prompt_lib/{projectId}/{toolkitId}/{indexName}/configuration ── */

/**
 * The SAVE half of the Indexes tab's "Save" / "Save & Reindex" split
 * (ELITEA-2880).
 *
 * Until this route existed, the ONLY writer of an index's stored
 * `index_configuration` was an indexing RUN: `startIndexExecution` above sends
 * the form as `tool_params`, and the run records them on the index metadata
 * document on its way through. So the configuration tab could persist a
 * changed `progress_step` only by re-indexing the whole collection — and a
 * scheduled reindex, which reads that same stored field
 * (`internal/runtimecomposition/index_schedule_inspector.go`), kept running the
 * configuration of the last RUN rather than the one on screen.
 *
 * The body is the same object `startIndexExecution` sends as `tool_params`,
 * under an `index_configuration` key, and it goes through the same
 * `preserveExplicitClears` replacer for the same reason (#311): a cleared field
 * must reach the server as `null`, not vanish and be re-defaulted on the way
 * back.
 */
export interface SaveIndexConfigurationParams {
  readonly projectId: string | number;
  readonly toolkitId: string;
  /** The index's `metadata.collection` — the key this route addresses, exactly as the schedule PATCH does. */
  readonly indexName: string;
  readonly configuration: Readonly<Record<string, unknown>>;
}

export async function saveIndexConfiguration(params: SaveIndexConfigurationParams): Promise<unknown> {
  const { projectId, toolkitId, indexName, configuration } = params;
  return fetchData<unknown>(
    `/elitea_core/index_meta/prompt_lib/${String(projectId)}/${toolkitId}/${encodeURIComponent(indexName)}/configuration`,
    {
      method: 'PUT',
      body: JSON.stringify({ index_configuration: configuration }, preserveExplicitClears),
      headers: { 'Content-Type': 'application/json' },
    },
  );
}

export function useSaveIndexConfigurationMutation(): UseMutationResult<unknown, Error, SaveIndexConfigurationParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: saveIndexConfiguration,
    // The list carries `metadata.index_configuration`, which this write
    // changes: without the invalidation the rail keeps serving the
    // pre-save configuration to the next index that is opened.
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: INDEXES_QUERY_ROOT }),
  });
}

/* ── getIndexSchedule — GET elitea_core/tool/prompt_lib/{projectId}/{toolkitId} ── */

export interface GetIndexScheduleParams {
  readonly projectId: string | number | undefined;
  readonly toolkitId: string | undefined;
}

interface ToolDetailWire {
  readonly meta?: { readonly indexes_meta?: Readonly<Record<string, ScheduleEntry>> };
}

export async function getIndexSchedule(params: GetIndexScheduleParams, signal?: AbortSignal): Promise<ToolDetailWire> {
  const { projectId, toolkitId } = params;
  return fetchData<ToolDetailWire>(`/elitea_core/tool/prompt_lib/${String(projectId)}/${String(toolkitId)}`, signal ? { signal } : {});
}

export function useIndexScheduleQuery(params: GetIndexScheduleParams): UseQueryResult<ToolDetailWire> {
  const { projectId, toolkitId } = params;
  const setToolkitScheduler = useIndexesStore((state) => state.setToolkitScheduler);
  return useQuery({
    queryKey: [...INDEX_SCHEDULE_QUERY_ROOT, toolkitId, projectId],
    queryFn: async ({ signal }) => {
      const result = await getIndexSchedule(params, signal);
      setToolkitScheduler(result.meta?.indexes_meta ?? {});
      return result;
    },
    enabled: projectId !== undefined && toolkitId !== undefined,
    refetchOnMount: 'always',
    refetchOnWindowFocus: true,
    refetchOnReconnect: true,
  });
}

/* ── getIndexHistoryConversationDetails — GET elitea_core/conversation/prompt_lib/{projectId}/{conversationId} ── */
/* Local duplicate of entities/run-history's getRunHistoryDetails — see file header. */

export interface GetIndexHistoryConversationDetailsParams {
  readonly projectId: string | number | undefined;
  readonly conversationId: string | undefined;
}

/**
 * Loosely typed — the persisted conversation shape is `entities/
 * conversation`'s concern, not re-derived here — EXCEPT `message_groups`/
 * `participants`, typed against `entities/message`'s real wire types so
 * `../lib/helpers/conversationHistory.local.ts` (which normalizes exactly
 * those two fields) can consume this without a cast.
 */
export interface ConversationDetailsWire {
  readonly id?: string | number;
  readonly message_groups?: readonly MessageGroupWire[];
  readonly participants?: readonly ConversationParticipantWire[];
  readonly [key: string]: unknown;
}

export async function getIndexHistoryConversationDetails(
  params: GetIndexHistoryConversationDetailsParams,
  signal?: AbortSignal,
): Promise<ConversationDetailsWire> {
  const { projectId, conversationId } = params;
  return fetchData<ConversationDetailsWire>(
    `/elitea_core/conversation/prompt_lib/${String(projectId)}/${String(conversationId)}`,
    signal ? { signal } : {},
  );
}

export function useIndexHistoryConversationDetailsQuery(
  params: GetIndexHistoryConversationDetailsParams,
  options: { enabled?: boolean } = {},
): UseQueryResult<ConversationDetailsWire> {
  const { projectId, conversationId } = params;
  return useQuery({
    queryKey: [...INDEXES_QUERY_ROOT, 'historyConversation', projectId, conversationId],
    queryFn: ({ signal }) => getIndexHistoryConversationDetails(params, signal),
    enabled: (options.enabled ?? true) && projectId !== undefined && conversationId !== undefined,
  });
}
