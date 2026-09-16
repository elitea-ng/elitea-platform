/**
 * Hand-written client for the participant-scoped conversation mutations
 * (unit C1). Source: `apps/elitea-ui/src/[fsd]/features/chat/api/
 * chat.api.js:124-184` (`addParticipantIntoConversation`/
 * `deleteParticipantFromConversation`/`updateParticipantSettings`/
 * `updateParticipantLlmSettings` RTK Query endpoints).
 *
 * WHY HAND-WRITTEN, NOT GENERATED: none of `chat.api.js`'s 28
 * `/elitea_core/conversation(s)/...`-family endpoints appear in
 * `services/elitea-main/api/openapi/v2.yaml`, so orval never generates a
 * client for them — a documented spec-coverage gap (mission preamble),
 * NOT a backend gap: every route below IS real and wired
 * (`services/elitea-main/internal/api/router.go:425-428`,
 * `internal/api/v2/conversations/handler.go`). Same pattern already
 * precedented twice against this exact backend domain —
 * `features/pipelines/api/aiAssistantPredict.ts`'s `stopLlmTask` and
 * `features/toolkits/indexes/api/indexesApi.ts`'s
 * `getIndexHistoryConversationDetails` — this file follows their
 * `eliteaFetch`-based `fetchData<T>` unwrap convention exactly. Per R-A5,
 * every endpoint below is reported for merge into
 * `src/shared/api/endpoints.manifest.json` as `source: "handwritten"`
 * (not edited directly here — see this unit's report).
 *
 * TanStack Query replaces RTK Query's `invalidatesTags`: there is no
 * generated or hand-written `conversationDetails` query anywhere in this
 * app yet (the sibling two endpoints above are the only handwritten chat-
 * domain entries so far, and neither is `conversationDetails`), so there is
 * `entities/conversation` now owns the corresponding details query. Mutations
 * invalidate its real prefix (`['conversation', 'details', projectId,
 * conversationId]`) so saved participant settings are visible after a
 * refetch instead of remaining trapped in a stale cache entry.
 */
import { useMutation, useQueryClient, type UseMutationResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';

import type { Participant } from '../model/types';
import { normaliseParticipants } from '../lib/normalise';
import type { ParticipantWire } from '../lib/normalise';

async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

function conversationDetailsQueryKey(projectId: string, conversationId: string) {
  return ['conversation', 'details', projectId, conversationId] as const;
}

/* ── addParticipantIntoConversation — POST elitea_core/participants/prompt_lib/{projectId}/{conversationId} ── */

/**
 * One wire-shaped participant-to-add entry — `entity_name`/`entity_meta`/
 * `entity_settings` (chat.api.js:124-137's `participants` body verbatim,
 * shape confirmed against `AddParticipant`'s repo write path,
 * `internal/infra/db/repos/conversations.go:241-267`: `entity_name`,
 * `entity_meta`, `entity_settings` are the only keys read off each item).
 * Not part of this slice's public API (unexported: only referenced by
 * `AddParticipantParams.participants` below).
 */
interface ParticipantAddInput {
  readonly entity_name: string;
  readonly entity_meta?: Readonly<Record<string, unknown>>;
  readonly entity_settings?: Readonly<Record<string, unknown>>;
}

export interface AddParticipantParams {
  readonly projectId: string | number;
  readonly conversationId: string;
  /**
   * MUST be a JSON array — `AddParticipant`'s handler decodes the body as
   * `[]map[string]any` and 400s on anything else (handler.go:551-560; the
   * handler's own `// Try as single object` comment is dead code, there is
   * no actual single-object fallback). `chat.api.js`'s RTK mutation always
   * passed its `participants` argument straight through as the body, so
   * this constraint was already implicit at every real call site.
   */
  readonly participants: readonly ParticipantAddInput[];
}

/** Response: the conversation's full, refreshed participant list (handler.go:569-571). */
export async function addParticipantIntoConversation(params: AddParticipantParams): Promise<Participant[]> {
  const { projectId, conversationId, participants } = params;
  const wire = await fetchData<readonly ParticipantWire[]>(
    `/elitea_core/participants/prompt_lib/${String(projectId)}/${conversationId}`,
    { method: 'POST', body: JSON.stringify(participants), headers: { 'Content-Type': 'application/json' } },
  );
  return normaliseParticipants(wire);
}

export function useAddParticipantMutation(): UseMutationResult<Participant[], Error, AddParticipantParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: addParticipantIntoConversation,
    onSuccess: (_data, variables) =>
      void queryClient.invalidateQueries({
        queryKey: conversationDetailsQueryKey(String(variables.projectId), variables.conversationId),
      }),
  });
}

/* ── deleteParticipantFromConversation — DELETE elitea_core/participant/prompt_lib/{projectId}/{conversationId}/{participantId} ── */

export interface DeleteParticipantParams {
  readonly projectId: string | number;
  readonly conversationId: string;
  readonly id: string;
}

/** `RemoveParticipant` returns `204 No Content` (handler.go:574-583) — resolves to `void`, not an echoed body. */
export async function deleteParticipantFromConversation(params: DeleteParticipantParams): Promise<void> {
  const { projectId, conversationId, id } = params;
  await eliteaFetch<unknown>(
    `/elitea_core/participant/prompt_lib/${String(projectId)}/${conversationId}/${id}`,
    { method: 'DELETE' },
  );
}

export function useDeleteParticipantMutation(): UseMutationResult<void, Error, DeleteParticipantParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: deleteParticipantFromConversation,
    onSuccess: (_data, variables) =>
      void queryClient.invalidateQueries({
        queryKey: conversationDetailsQueryKey(String(variables.projectId), variables.conversationId),
      }),
  });
}

/* ── updateParticipantSettings — PUT elitea_core/entity_settings/prompt_lib/{projectId}/{conversationId}/{participantId} ── */

export interface UpdateParticipantSettingsParams {
  readonly projectId: string | number;
  readonly conversationId: string;
  readonly participantId: string;
  /** Arbitrary entity-settings patch — `chat.api.js:150-165`'s `...body` spread (everything but the three path/id fields). */
  readonly settings: Readonly<Record<string, unknown>>;
}

/**
 * Response is `{entity_settings: <echoed body>}` (handler.go:585-625), NOT
 * the conversation. `UpdateEntitySettings` additionally strips
 * `llm_settings` server-side when the participant is a non-published
 * agent's `application` (handler.go:595-618) — the echoed body reflects
 * that stripping, so callers should read this return value rather than
 * assuming their own request body was applied verbatim.
 */
export async function updateParticipantSettings(
  params: UpdateParticipantSettingsParams,
): Promise<Readonly<Record<string, unknown>>> {
  const { projectId, conversationId, participantId, settings } = params;
  const wire = await fetchData<{ readonly entity_settings: Readonly<Record<string, unknown>> }>(
    `/elitea_core/entity_settings/prompt_lib/${String(projectId)}/${conversationId}/${participantId}`,
    { method: 'PUT', body: JSON.stringify(settings), headers: { 'Content-Type': 'application/json' } },
  );
  return wire.entity_settings;
}

export function useUpdateParticipantSettingsMutation(): UseMutationResult<
  Readonly<Record<string, unknown>>,
  Error,
  UpdateParticipantSettingsParams
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: updateParticipantSettings,
    onSuccess: (_data, variables) =>
      void queryClient.invalidateQueries({
        queryKey: conversationDetailsQueryKey(String(variables.projectId), variables.conversationId),
      }),
  });
}

/* ── updateParticipantLlmSettings — PATCH elitea_core/entity_settings/prompt_lib/{projectId}/{conversationId} ── */

export interface UpdateParticipantLlmSettingsParams {
  readonly projectId: string | number;
  readonly conversationId: string;
  /** The participant whose `entity_settings` this call replaces. */
  readonly participantId: string;
  /**
   * The participant's CURRENT `entity_settings` (wire shape, e.g.
   * `version_id`/`variables`/`icon_meta`), spread first. Required because
   * `ConversationsRepo.UpdateEntitySettings` — the repo method BOTH this
   * batch route and the single-participant PUT ultimately call — is a full
   * REPLACE (`UPDATE ... SET entity_settings = $1`
   * `internal/infra/db/repos/conversations.go:895`), not a merge. Sending
   * only `{llm_settings}` would silently wipe every other field the batch
   * request omits. Same rule `features/agents/model/
   * useApplicationChatSwitchVersion.ts`'s own doc comment already states for
   * the single-participant PUT this shares a repo method with.
   */
  readonly currentEntitySettings?: Readonly<Record<string, unknown>> | undefined;
  readonly llm_settings: Readonly<Record<string, unknown>>;
}

/**
 * **FIXED — client body shape now matches the handler it actually hits
 * (A14, ELITEA-0386).** `chat.api.js:171-184`'s `updateParticipantLlmSettings`
 * PATCHed this URL with body `{llm_settings}` (a single JSON OBJECT). The Go
 * route this URL resolves to is `BatchUpdateEntitySettings`
 * (`internal/api/router.go:3015`, `internal/api/v2/conversations/
 * handler.go:1295-1308`), which decodes the body as `var body []map[string]
 * any` — a JSON ARRAY of per-participant settings maps. Sending an object
 * where the decoder expects an array failed JSON unmarshalling — 400
 * `{"error": "invalid request body"}` — for EVERY call. This was never wired
 * to any UI (zero callers anywhere in `src/`), so the defect was latent.
 * Fixed by sending the one-element batch array the handler actually
 * decodes, keyed by `participant_id` (not `id` — the repo reads
 * `participant_id` specifically, `conversations.go:904`; `handler_test.go`'s
 * own fixture body uses `id`, but its mock repo ignores the argument
 * entirely, so that test does not discriminate between the two keys — the
 * repo source is the ground truth here, not the test fixture).
 *
 * **DISCLOSED — this route does NOT run the non-published-agent
 * llm_settings-override guard.** That guard (`apierr.BadRequest("LLM
 * settings override is only allowed for published agents from agent
 * studio")`) lives ONLY in the HTTP handler function `(h *Handler)
 * UpdateEntitySettings` (`handler.go:1207-1240`, the single-participant PUT).
 * `BatchUpdateEntitySettings`'s repo half
 * (`ConversationsRepo.BatchUpdateEntitySettings`,
 * `internal/infra/db/repos/conversations.go:902-911`) calls the REPO
 * method of the same name directly — a plain jsonb replace with no
 * validation — never the HTTP handler, so this route cannot itself produce
 * ELITEA-0386's "spurious error": a caller through THIS endpoint always
 * succeeds. Real per-participant validation the batch route may need is a
 * backend decision (`do not change the Go contract` — this unit's brief),
 * not something to invent client-side.
 */
export async function updateParticipantLlmSettings(
  params: UpdateParticipantLlmSettingsParams,
): Promise<{ readonly ok: boolean }> {
  const { projectId, conversationId, participantId, currentEntitySettings, llm_settings } = params;
  const entitySettings = { ...currentEntitySettings, llm_settings };
  return fetchData<{ readonly ok: boolean }>(
    `/elitea_core/entity_settings/prompt_lib/${String(projectId)}/${conversationId}`,
    {
      method: 'PATCH',
      body: JSON.stringify([{ participant_id: participantId, ...entitySettings }]),
      headers: { 'Content-Type': 'application/json' },
    },
  );
}

export function useUpdateParticipantLlmSettingsMutation(): UseMutationResult<
  { readonly ok: boolean },
  Error,
  UpdateParticipantLlmSettingsParams
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: updateParticipantLlmSettings,
    onSuccess: (_data, variables) =>
      void queryClient.invalidateQueries({
        queryKey: conversationDetailsQueryKey(String(variables.projectId), variables.conversationId),
      }),
  });
}
