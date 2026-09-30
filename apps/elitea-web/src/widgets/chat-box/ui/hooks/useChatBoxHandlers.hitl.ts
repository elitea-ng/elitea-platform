/**
 * hooks/useChatBoxHandlers.hitl.ts — the HITL continuation body.
 *
 * `continueHitl` resumes a paused run over REST
 * (`POST /elitea_core/continue_predict/prompt_lib/{projectID}/{conversationID}`
 * with `execution_contract=agent.continue.hitl.v1`). The route validates every
 * field of that body, so the shaping lives here, beside the rules it obeys,
 * rather than inside the handler.
 *
 * Its own file for the §3.5 file-length budget: `useChatBoxHandlers.helpers.ts`
 * is already at the limit.
 */
import type { ChatMessage } from '@/features/chat-messages';

import type { HitlInterruptAction } from './useChatBoxHandlers.helpers';

/** The five `hitl_action` values the Go route admits (`currentRootHITLAction`). */
const ROOT_HITL_ACTIONS: readonly string[] = ['approve', 'reject', 'edit', 'block_with_comment', 'answer'];

/** The actions whose decision carries a `hitl_value`; the route REFUSES an empty one. */
const VALUED_HITL_ACTIONS: readonly string[] = ['edit', 'block_with_comment', 'answer'];

/**
 * The `hitl_value` of a clarification answer — STRUCTURED, not prose.
 *
 * `currentHITLValue` (services/elitea-main/internal/api/v2/agentexecution/
 * route.go) admits a JSON object or a JSON string for `answer` and refuses
 * anything else, then canonicalises what it admitted; the worker parses that
 * canonical text back with `AskUserRequest::format_answer`. The card encodes
 * its `{questionId: answer}` object into `value` because every layer between
 * the two types the value as a string, so it is decoded here — sending the
 * ENCODED string would reach the model as one JSON blob quoted at it rather
 * than as the answers to its questions.
 *
 * An input that is not JSON (or is JSON of a shape the route refuses, e.g. an
 * array) travels on as the plain string it already is: that is the one other
 * shape the route admits, and it beats refusing the resume outright.
 */
function hitlAnswerValue(raw: string): unknown {
  try {
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed === 'string') return parsed;
    if (typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed)) return parsed;
  } catch {
    return raw;
  }
  return raw;
}

/**
 * One entry of the `hitl_decisions` array. The route admits these four keys
 * and no other.
 *
 * `value` is `unknown` rather than `string` because the route runs the SAME
 * `currentHITLValue` over a decision's value as over the root `hitl_value`:
 * an `answer` decision carries the structured answers, everything else a
 * string.
 */
interface HitlDecision {
  readonly interrupt_id: string;
  readonly tool_call_id: string;
  readonly action: string;
  readonly value: unknown;
}

export interface HitlContinueBodyParams {
  readonly projectId: string | number | undefined;
  readonly conversationUuid: string | undefined;
  readonly messageId: string;
  readonly threadId?: string | undefined;
  readonly action: HitlInterruptAction;
  readonly interruptId?: string | undefined;
}

/**
 * The `interrupt_id` of the pause one decision answers.
 *
 * The route REQUIRES the field in every `hitl_decisions` entry, so a pause
 * that carries none cannot be resumed over REST at all.
 */
export function findHitlInterruptId(message: ChatMessage, toolCallId: string | undefined): string | undefined {
  const entries = (message.hitlInterrupts ?? []) as readonly { readonly interrupt_id?: string; readonly tool_call_id?: string }[];
  const match = toolCallId ? entries.find((entry) => entry.tool_call_id === toolCallId) : entries[0];
  const single = message.hitlInterrupt as { readonly interrupt_id?: string } | undefined;
  const id = match?.interrupt_id ?? single?.interrupt_id;
  return id ? id : undefined;
}

/**
 * The identity fields the route checks first, or `undefined` when this pause
 * cannot address the route at all.
 *
 * `project_id` is a NUMBER and must equal the path project; the socket payload
 * sends a string, so it is rebuilt rather than reused. `conversation_uuid`
 * must equal the path conversation, and `message_id` must not be empty.
 */
function hitlContinueIdentity(params: HitlContinueBodyParams): Record<string, unknown> | undefined {
  const projectId = Number(params.projectId);
  if (!Number.isSafeInteger(projectId) || projectId <= 0) return undefined;
  if (!params.conversationUuid || !params.messageId) return undefined;
  return {
    project_id: projectId,
    conversation_uuid: params.conversationUuid,
    message_id: params.messageId,
    ...(params.threadId ? { thread_id: params.threadId } : {}),
    hitl_resume: true,
  };
}

/**
 * The decision for a tool pause, including a child on the root thread.
 *
 * `thread_id` belongs at the TOP level of the body. Inside a decision entry
 * the route refuses it, because `currentHITLDecisions` admits only
 * `interrupt_id`, `tool_call_id`, `action` and `value`.
 */
function hitlFanoutDecision(params: HitlContinueBodyParams): HitlDecision | undefined {
  if (!params.interruptId) return undefined;
  return {
    interrupt_id: params.interruptId,
    tool_call_id: params.action.toolCallId ?? '',
    action: params.action.action,
    value:
      params.action.action === 'answer'
        ? hitlAnswerValue(params.action.value ?? '')
        : (params.action.value ?? ''),
  };
}

/**
 * The HITL continuation body, or `undefined` when this pause cannot be
 * expressed in the contract.
 *
 * `undefined` means "no REST body fits", and the caller then emits over the
 * socket. A fan-out child whose interrupt carries no `interrupt_id` is the one
 * pause that lands there.
 *
 * The body never carries `mcp_tokens`, `ignored_mcp_servers` or
 * `user_declined_mcp_servers`: the route refuses all three alongside a HITL
 * resume.
 */
export function buildHitlContinueBody(params: HitlContinueBodyParams): Record<string, unknown> | undefined {
  const base = hitlContinueIdentity(params);
  if (base === undefined) return undefined;
  if (params.action.childThreadId || params.action.toolCallId) {
    const decision = hitlFanoutDecision(params);
    return decision === undefined ? undefined : { ...base, hitl_decisions: [decision] };
  }
  if (!ROOT_HITL_ACTIONS.includes(params.action.action)) return undefined;
  const withValue = VALUED_HITL_ACTIONS.includes(params.action.action)
    ? {
        hitl_value:
          params.action.action === 'answer'
            ? hitlAnswerValue(params.action.value ?? '')
            : (params.action.value ?? ''),
      }
    : {};
  return { ...base, hitl_action: params.action.action, ...withValue };
}
