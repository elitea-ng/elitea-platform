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
import { conversationApi } from '@/entities/conversation';
import type { ChatMessage } from '@/features/chat-messages';
import { t } from '@/shared/i18n';

import {
  claimHitlSubmission,
  forgetHitlDecisions,
  queueHitlDecision,
  queuedHitlDecisions,
  releaseHitlSubmission,
  type HitlQueueScope,
  type QueuedHitlDecision,
} from '../../model/hitlDecisionQueue';
import { buildChatContinuePayload, tryEmit } from './useChatBoxHandlers.helpers';
import type { ChatBoxHandlerDeps, HitlInterruptAction, StreamStartOutcome } from './useChatBoxHandlers.helpers';
import { cardFlags, cardKey, cardsOf, patchMessageCards, pendingCards, type HitlCard } from './useChatBoxHandlers.hitlCards';
import { undeliveredText } from './useChatBoxHandlers.turns';

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

interface HitlContinueBodyParams {
  readonly projectId: string | number | undefined;
  readonly conversationUuid: string | undefined;
  readonly messageId: string;
  readonly threadId?: string | undefined;
  readonly action: HitlInterruptAction;
  readonly interruptId?: string | undefined;
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
function buildHitlContinueBody(params: HitlContinueBodyParams): Record<string, unknown> | undefined {
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

/**
 * The body for a whole pending set: one entry per card, in message order.
 *
 * A set of one keeps the single-card body above, exactly as before. Every card
 * of a larger set must carry an `interrupt_id`; `undefined` otherwise.
 */
function hitlSetBody(
  params: Omit<HitlContinueBodyParams, 'action' | 'interruptId'>,
  set: readonly HitlCard[],
  decisions: readonly QueuedHitlDecision[],
): Record<string, unknown> | undefined {
  const [onlyCard] = set;
  const [onlyDecision] = decisions;
  if (set.length === 1 && onlyCard && onlyDecision) {
    return buildHitlContinueBody({ ...params, action: decisionAction(onlyDecision), interruptId: onlyCard.interrupt_id || undefined });
  }
  const base = hitlContinueIdentity({ ...params, action: { action: '' } });
  if (base === undefined) return undefined;
  const entries = set.map((card, index) => {
    const decision = decisions[index];
    if (!decision) return undefined;
    return hitlFanoutDecision({
      ...params,
      action: { ...decisionAction(decision), toolCallId: card.tool_call_id ?? '' },
      interruptId: card.interrupt_id || undefined,
    });
  });
  if (entries.some((entry) => entry === undefined)) return undefined;
  return { ...base, hitl_decisions: entries };
}

function decisionAction(decision: Pick<QueuedHitlDecision, 'action' | 'value' | 'toolCallId'>): HitlInterruptAction {
  return {
    action: decision.action,
    ...(decision.value !== undefined ? { value: decision.value } : {}),
    ...(decision.toolCallId !== undefined ? { toolCallId: decision.toolCallId } : {}),
  };
}

/** The legacy socket payload, for a deployment that serves no continuation route at all. */
function hitlSocketPayload(deps: ChatBoxHandlerDeps, message: ChatMessage, submission: HitlSubmission): Record<string, unknown> {
  const { set, decisions, threadId, independent } = submission;
  const [first] = decisions;
  const question = first?.action === 'edit' ? (first.value ?? '') : (first?.action ?? '');
  const base = { ...buildChatContinuePayload(deps, { messageId: message.id, threadId, question }), hitl_resume: true };
  if (set.length === 1 && first && !independent) {
    const valued = first.action === 'edit' || first.action === 'block_with_comment';
    return { ...base, hitl_action: first.action, ...(valued ? { hitl_value: first.value ?? '' } : {}) };
  }
  return {
    ...base,
    hitl_decisions: set.map((card, index) => ({
      ...(independent && threadId ? { thread_id: threadId } : {}),
      interrupt_id: card.interrupt_id ?? '',
      tool_call_id: card.tool_call_id ?? decisions[index]?.toolCallId ?? '',
      action: decisions[index]?.action ?? '',
      value: decisions[index]?.value ?? '',
    })),
  };
}

/** A legacy fan-out child resumes on its own thread, alone, as it always has. */
function isIndependentChild(card: HitlCard): boolean {
  return Boolean(card.child_thread_id) && card.resume_strategy === 'aggregate_child';
}

function findTargetCard(pending: readonly HitlCard[], action: HitlInterruptAction, cards: readonly HitlCard[]): HitlCard | undefined {
  if (action.interruptId) return pending.find((card) => cardKey(card, cards) === action.interruptId);
  if (action.toolCallId) return pending.find((card) => card.tool_call_id === action.toolCallId);
  return pending.find((card) => !card.queued) ?? pending[0];
}

function conversationKey(deps: ChatBoxHandlerDeps): string {
  return deps.conversationUuid || String(deps.conversationId ?? '') || 'unsaved';
}

function refusedText(): string {
  return t('widgets.chatBox.hitlDecisionRefused', 'The decision could not be sent. Try again.');
}

function notQueuedText(): string {
  return t('widgets.chatBox.hitlDecisionNotQueued', 'This decision could not be recorded. Reload the conversation and try again.');
}

interface HitlSubmission {
  readonly message: ChatMessage;
  readonly set: readonly HitlCard[];
  readonly decisions: readonly QueuedHitlDecision[];
  readonly threadId: string | undefined;
  /** A legacy fan-out child: resumed alone, on its own thread. */
  readonly independent: boolean;
  readonly scope: HitlQueueScope;
}

async function resumeSetOverRest(deps: ChatBoxHandlerDeps, submission: HitlSubmission): Promise<StreamStartOutcome | undefined> {
  if (!deps.continueStreamedExecution || !deps.conversationUuid) return undefined;
  const body = hitlSetBody(
    { projectId: deps.projectId, conversationUuid: deps.conversationUuid, messageId: submission.message.id, threadId: submission.threadId },
    submission.set,
    submission.decisions,
  );
  if (body === undefined) return undefined;
  return deps.continueStreamedExecution({
    conversationUuid: deps.conversationUuid,
    contract: conversationApi.contracts.continueHitl,
    body,
  });
}

/**
 * Submit one complete set and settle the cards by the route's answer.
 *
 * Only here does the message start streaming. `already-resolved` (HTTP 409:
 * the set was consumed elsewhere) keeps the cards answered and quiet. Any other
 * route answer puts the cards back with the route's reason. The socket is
 * tried ONLY when no REST answer exists at all.
 */
async function submitHitlSet(deps: ChatBoxHandlerDeps, submission: HitlSubmission): Promise<void> {
  const { message, set, scope } = submission;
  const cards = cardsOf(message);
  const keys = new Set(set.map((card) => cardKey(card, cards)));
  const settle = (fields: Partial<ChatMessage>, patch = cardFlags.restored): void =>
    patchMessageCards(deps.setChatHistory, message.id, keys, patch, { isLoading: false, isStreaming: false, ...fields });
  patchMessageCards(deps.setChatHistory, message.id, keys, cardFlags.decided, { isLoading: true, isStreaming: true, exception: undefined });
  deps.setStreamingInfo(message.questionId ?? message.id);
  const outcome = await resumeSetOverRest(deps, submission);
  forgetHitlDecisions(scope);
  if (outcome?.started) return;
  if (outcome && outcome.reason === 'already-resolved') return settle({}, cardFlags.decided);
  if (outcome && outcome.reason !== 'no-transport') {
    return settle({ exception: outcome.reason === 'rejected' ? outcome.message : refusedText() });
  }
  const payload = hitlSocketPayload(deps, message, submission);
  if (!tryEmit(() => deps.emitSocket('chat_continue_predict', payload), 'continueHitl')) {
    settle({ exception: undeliveredText() });
  }
}

/**
 * Decide ONE pause card. The single owner of the queue-then-submit rule.
 *
 * The decision is recorded for that card's identity and only that card is
 * marked queued and hidden; siblings stay answerable and the message's
 * streaming state does not change. When the last pending card of the response
 * is decided, the whole set goes out as ONE continuation, because today's
 * route consumes the complete pending set (`decisionsMatchCurrentHITLInterrupts`).
 * Wave 2's per-card POST changes this function and nothing else.
 */
export async function decideHitlCard(deps: ChatBoxHandlerDeps, action: HitlInterruptAction): Promise<void> {
  const message = [...deps.chatHistory].reverse().find((item) => pendingCards(item).length > 0);
  if (!message) return;
  const cards = cardsOf(message);
  const pending = pendingCards(message);
  const target = findTargetCard(pending, action, cards);
  if (!target) return;
  const independent = Boolean(action.childThreadId) || isIndependentChild(target);
  const set = independent ? [target] : pending.filter((card) => !isIndependentChild(card));
  const scope: HitlQueueScope = { conversation: conversationKey(deps), message: message.id, kind: 'hitl' };
  const decision: QueuedHitlDecision = { ...decisionAction(action), interruptId: cardKey(target, cards) };
  let decisions: readonly QueuedHitlDecision[] = [decision];
  if (set.length > 1) {
    const queued = queueHitlDecision(scope, decision);
    if (!queued.queued) {
      deps.setChatHistory((previous) => previous.map((item) => (item.id === message.id ? { ...item, exception: notQueuedText() } : item)));
      return;
    }
    const byKey = new Map(queuedHitlDecisions(scope).map((entry) => [entry.interruptId, entry]));
    const missing = set.filter((card) => !byKey.has(cardKey(card, cards)));
    if (missing.length > 0) {
      patchMessageCards(deps.setChatHistory, message.id, new Set([decision.interruptId]), cardFlags.queued);
      return;
    }
    decisions = set.map((card) => byKey.get(cardKey(card, cards)) ?? decision);
  }
  if (!claimHitlSubmission(scope)) return;
  try {
    await submitHitlSet(deps, {
      message,
      set,
      decisions,
      threadId: independent ? action.childThreadId || target.child_thread_id : message.threadId,
      independent,
      scope,
    });
  } finally {
    releaseHitlSubmission(scope);
  }
}
