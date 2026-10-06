import { continueChatExecution } from '@/shared/api/generated/chat/chat';
import { StaticPipelineRootContinuation, StaticPipelineToolsContinuation, StaticPipelineLeafDecision, StaticPipelineContinuationReceipt, type ChatContinueRequest, type EditorTestRunsPage } from '@/shared/api/generated/model';
/** Conversation adapters retain existing routes and ordinary chat behavior.
 * OpenAPI now documents atomic create, detail, and typed editor Test recovery.
 * Remaining mutation adapters use the same generated fetch transport.
 */
import { useMutation, useQuery, type UseMutationResult, type UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';

import type { ChatParticipantWire } from '../lib/wire';

async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

/** A persisted conversation row — loosely typed, no OpenAPI schema exists for this resource (see module doc). */
export interface ConversationWire {
  readonly id: string | number;
  readonly uuid?: string;
  readonly name: string;
  readonly source?: string;
  readonly editor_test_runs?: EditorTestRunsPage;
  readonly is_private?: boolean;
  readonly folder_id?: string | number;
  readonly created_at?: string;
  readonly updated_at?: string;
  readonly participants?: readonly ChatParticipantWire[];
  readonly meta?: Readonly<Record<string, unknown>>;
  readonly [key: string]: unknown;
}

/* ── conversationCreate — POST elitea_core/conversations/prompt_lib/{projectId} ── */
/* manifest: conversation.create */

export interface ConversationCreateParams {
  readonly projectId: string | number;
  readonly name: string;
  readonly is_private: boolean;
  readonly source?: string;
  readonly participants?: readonly unknown[];
  readonly meta?: Readonly<Record<string, unknown>>;
}

export async function conversationCreate(params: ConversationCreateParams): Promise<ConversationWire> {
  const { projectId, ...body } = params;
  return fetchData<ConversationWire>(`/elitea_core/conversations/prompt_lib/${String(projectId)}`, {
    method: 'POST',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useConversationCreateMutation(): UseMutationResult<ConversationWire, unknown, ConversationCreateParams> {
  return useMutation({ mutationFn: conversationCreate });
}

/* ── conversationEdit — PUT elitea_core/conversation/prompt_lib/{projectId}/{id} ── */
/* manifest: conversation.edit */

export interface ConversationEditParams {
  readonly projectId: string | number;
  readonly id: string | number;
  readonly name?: string;
  readonly is_private?: boolean;
  readonly [key: string]: unknown;
}

export async function conversationEdit(params: ConversationEditParams): Promise<ConversationWire> {
  const { projectId, id, ...body } = params;
  return fetchData<ConversationWire>(`/elitea_core/conversation/prompt_lib/${String(projectId)}/${String(id)}`, {
    method: 'PUT',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useConversationEditMutation(): UseMutationResult<ConversationWire, unknown, ConversationEditParams> {
  return useMutation({ mutationFn: conversationEdit });
}

/* ── deleteConversation — DELETE elitea_core/conversation/prompt_lib/{projectId}/{id} ── */
/* manifest: conversation.delete */

export interface DeleteConversationParams {
  readonly projectId: string | number;
  readonly id: string | number;
}

export async function deleteConversation(params: DeleteConversationParams): Promise<unknown> {
  return fetchData<unknown>(`/elitea_core/conversation/prompt_lib/${String(params.projectId)}/${String(params.id)}`, { method: 'DELETE' });
}

export function useDeleteConversationMutation(): UseMutationResult<unknown, unknown, DeleteConversationParams> {
  return useMutation({ mutationFn: deleteConversation });
}

/* ── conversationDetails — GET elitea_core/conversation/prompt_lib/{projectId}/{id} ── */
/* Reuses `toolkits.getIndexHistoryConversationDetails`'s route — no new manifest entry (see module doc). */

export interface ConversationDetailsParams {
  readonly projectId: string | number;
  readonly id: string | number;
  readonly editor_test_runs?: boolean;
  readonly runs_limit?: number;
  readonly runs_offset?: number;
  readonly messages_offset?: number;
  readonly messages_limit?: number;
  readonly sort_order?: string;
}

function detailsQueryString(params: ConversationDetailsParams): string {
  const query = new URLSearchParams();
  if (params.editor_test_runs !== undefined) query.set('editor_test_runs', String(params.editor_test_runs));
  if (params.runs_limit !== undefined) query.set('runs_limit', String(params.runs_limit));
  if (params.runs_offset !== undefined) query.set('runs_offset', String(params.runs_offset));
  if (params.messages_offset !== undefined) query.set('messages_offset', String(params.messages_offset));
  if (params.messages_limit !== undefined) query.set('messages_limit', String(params.messages_limit));
  if (params.sort_order !== undefined) query.set('sort_order', params.sort_order);
  const qs = query.toString();
  return qs ? `?${qs}` : '';
}

export async function conversationDetails(params: ConversationDetailsParams, signal?: AbortSignal): Promise<ConversationWire> {
  const url = `/elitea_core/conversation/prompt_lib/${String(params.projectId)}/${String(params.id)}${detailsQueryString(params)}`;
  return fetchData<ConversationWire>(url, signal ? { signal } : {});
}

export function useConversationDetailsQuery(params: ConversationDetailsParams, options: { enabled?: boolean } = {}): UseQueryResult<ConversationWire> {
  return useQuery({
    queryKey: ['conversation', 'details', params.projectId, params.id, params.messages_offset, params.messages_limit, params.sort_order, params.editor_test_runs, params.runs_limit, params.runs_offset],
    queryFn: ({ signal }) => conversationDetails(params, signal),
    enabled: options.enabled ?? true,
  });
}

/* ── selectConversation — POST elitea_core/select_conversation/prompt_lib/{projectId}/{conversationId} ── */
/* manifest: conversation.select */

export interface SelectConversationParams {
  readonly projectId: string | number;
  readonly conversationId: string | number;
}

export async function selectConversation(params: SelectConversationParams): Promise<unknown> {
  return fetchData<unknown>(`/elitea_core/select_conversation/prompt_lib/${String(params.projectId)}/${String(params.conversationId)}`, {
    method: 'POST',
    body: JSON.stringify({}),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useSelectConversationMutation(): UseMutationResult<unknown, unknown, SelectConversationParams> {
  return useMutation({ mutationFn: selectConversation });
}

/* ── unselectConversation — DELETE elitea_core/select_conversation/prompt_lib/{projectId} ── */
/* manifest: conversation.unselect */

export interface UnselectConversationParams {
  readonly projectId: string | number;
}

export async function unselectConversation(params: UnselectConversationParams): Promise<unknown> {
  return fetchData<unknown>(`/elitea_core/select_conversation/prompt_lib/${String(params.projectId)}`, { method: 'DELETE' });
}

export function useUnselectConversationMutation(): UseMutationResult<unknown, unknown, UnselectConversationParams> {
  return useMutation({ mutationFn: unselectConversation });
}

/* ── regenerate — POST elitea_core/regenerate/prompt_lib/{projectId}/{id} ── */
/* manifest: conversation.regenerate */

export interface RegenerateParams {
  readonly projectId: string | number;
  readonly id: string | number;
  /**
   * Opt into the SSE execution contract (issue #93 — `agent.regenerate.v1`).
   * Sent as the `execution_contract` QUERY parameter and deliberately kept
   * OUT of the body: the Go route reads it from the query and rejects an
   * unrecognised body shape outright. Omitted ⇒ the pre-#93 call, whose
   * response carries no `events_url` and therefore keeps the socket path.
   */
  readonly executionContract?: string;
  readonly [key: string]: unknown;
}

export async function regenerate(params: RegenerateParams): Promise<AgentExecutionStart> {
  const { projectId, id, executionContract, ...body } = params;
  const query = executionContract ? `?execution_contract=${encodeURIComponent(executionContract)}` : '';
  return fetchData<AgentExecutionStart>(`/elitea_core/regenerate/prompt_lib/${String(projectId)}/${String(id)}${query}`, {
    method: 'POST',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useRegenerateMutation(): UseMutationResult<AgentExecutionStart, unknown, RegenerateParams> {
  return useMutation({ mutationFn: regenerate });
}

/* ── startAgentExecution — POST elitea_core/messages/prompt_lib/{projectId}/{conversationUuid} ── */
/* manifest: conversation.startAgentExecution */

/**
 * The three `execution_contract` values the Go agent-execution route admits
 * (`services/elitea-main/internal/api/v2/agentexecution/route.go`:
 * `CurrentApplicationStartContract` / `CurrentAdhocStartContract` /
 * `CurrentRegenerationContract`). The route REQUIRES one — a POST without a
 * recognised contract is a 400 — which is what makes it safe for a caller to
 * treat any failure as "this backend has not landed the SSE path" and fall
 * back to socket.io (issue #93).
 */
export const AGENT_EXECUTE_APPLICATION_CONTRACT = 'agent.execute.application.v1';
export const AGENT_EXECUTE_ADHOC_CONTRACT = 'agent.execute.adhoc.v1';
export const AGENT_REGENERATE_CONTRACT = 'agent.regenerate.v1';

/**
 * The start/regenerate response. `events_url` is the field that matters:
 * it is the absolute path of this execution's SSE stream (the Go route
 * builds `"/api/v2/executions/" + projectID + "/" + executionID + "/events"`
 * itself, so a client must NOT re-derive it). Its ABSENCE is the documented
 * fallback signal — an older backend answering the same route without one.
 */
export interface AgentExecutionStart {
  readonly events_url?: string;
  readonly task_id?: string;
  readonly execution_id?: string;
  readonly response_message_id?: string;
  readonly [key: string]: unknown;
}

export interface StartAgentExecutionParams {
  readonly projectId: string | number;
  /** Conversation UUID — the route's `{conversationID}` segment. */
  readonly conversationUuid: string;
  /** `agent.execute.application.v1` for an agent-app conversation, `agent.execute.adhoc.v1` for an ad-hoc/test one. */
  readonly contract: string;
  readonly body: Readonly<Record<string, unknown>>;
}

export async function startAgentExecution(params: StartAgentExecutionParams): Promise<AgentExecutionStart> {
  const { projectId, conversationUuid, contract, body } = params;
  return fetchData<AgentExecutionStart>(
    `/elitea_core/messages/prompt_lib/${String(projectId)}/${conversationUuid}?execution_contract=${encodeURIComponent(contract)}`,
    { method: 'POST', body: JSON.stringify(body), headers: { 'Content-Type': 'application/json' } },
  );
}

/* ── continueAgentExecution — POST elitea_core/continue_predict/prompt_lib/{projectId}/{conversationUuid} ── */
/* manifest: conversation.continueAgentExecution */

/**
 * The continuation contracts used by the current Go route. Output-limit
 * continuation has its own contract because it starts a fresh model call in
 * the same durable session; it is not a HITL or checkpoint resume.
 */
export const AGENT_CONTINUE_STATIC_CONTRACT = 'agent.continue.static.v1';
export const AGENT_CONTINUE_HITL_CONTRACT = 'agent.continue.hitl.v1';
/**
 * MCP tool authorization. Its own contract because the route's checks are the
 * OPPOSITE of the HITL arm's on three fields: `mcp_tokens`,
 * `ignored_mcp_servers` and `user_declined_mcp_servers` must be PRESENT (an
 * object and two arrays), where the HITL arm refuses all three.
 */
export const AGENT_CONTINUE_AUTHORIZATION_CONTRACT = 'agent.continue.authorization.v1';
export const AGENT_CONTINUE_OUTPUT_LIMIT_CONTRACT = 'agent.continue.output-limit.v1';

export interface ContinueAgentExecutionParams {
  readonly projectId: string | number;
  /** Conversation UUID — the route's `{conversationID}` segment. */
  readonly conversationUuid: string;
  readonly contract: string;
  readonly body: Readonly<Record<string, unknown>>;
}

/**
 * Resume one paused run.
 *
 * The body must satisfy the route's own checks: `project_id` is a NUMBER equal
 * to the path project, `conversation_uuid` equals the path conversation, and
 * `message_id` is not empty. The route answers 422 for anything else.
 */
export async function continueAgentExecution(params: ContinueAgentExecutionParams): Promise<AgentExecutionStart> {
  const { projectId, conversationUuid, contract, body } = params;
  if(contract===AGENT_CONTINUE_STATIC_CONTRACT){
    const request = 'static_pause_id' in body
      ? StaticPipelineRootContinuation.strict().parse(body)
      : StaticPipelineToolsContinuation.strict().parse({ ...body, static_decisions: Array.isArray(body['static_decisions']) ? body['static_decisions'].map(decision => StaticPipelineLeafDecision.strict().parse(decision)) : body['static_decisions'] });
    if (request.project_id !== Number(projectId) || request.conversation_uuid !== conversationUuid) throw new Error('Static continuation scope mismatch');
    const receipt=await continueChatExecution(String(projectId),conversationUuid,request satisfies ChatContinueRequest,{execution_contract:AGENT_CONTINUE_STATIC_CONTRACT});
    const accepted = StaticPipelineContinuationReceipt.strict().parse(receipt.data);
    if (accepted.response_message_id !== request.message_id) throw new Error('Static continuation response mismatch');
    return accepted;
  }
  return fetchData<AgentExecutionStart>(
    `/elitea_core/continue_predict/prompt_lib/${String(projectId)}/${conversationUuid}?execution_contract=${encodeURIComponent(contract)}`,
    { method: 'POST', body: JSON.stringify(body), headers: { 'Content-Type': 'application/json' } },
  );
}

/* ── stopChatTask — DELETE elitea_core/task/prompt_lib/{projectId}/{taskId} ── */
/* Reuses `pipelines.stopLlmTask`'s route — no new manifest entry (see module doc). Baseline param name is `messageGroupUuid`; same route. */

export interface StopChatTaskParams {
  readonly projectId: string | number;
  readonly messageGroupUuid: string;
}

export async function stopChatTask(params: StopChatTaskParams): Promise<unknown> {
  return fetchData<unknown>(`/elitea_core/task/prompt_lib/${String(params.projectId)}/${params.messageGroupUuid}`, { method: 'DELETE' });
}

export function useStopChatTaskMutation(): UseMutationResult<unknown, unknown, StopChatTaskParams> {
  return useMutation({ mutationFn: stopChatTask });
}
