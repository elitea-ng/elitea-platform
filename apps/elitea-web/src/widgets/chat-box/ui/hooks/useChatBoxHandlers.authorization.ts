/**
 * Exact durable continuation for delegated OAuth authorization requests.
 *
 * Each authorization card resolves on its own, like the reference
 * (`ChatBox.jsx` `continueMcpExecution` filters out exactly the clicked request
 * id; it has no "same server URL" grouping, so neither does this). Today's
 * route consumes the complete pending set
 * (`decisionsMatchCurrentAuthorizationRequests`), so a decided card waits in
 * `model/hitlDecisionQueue` — `{interruptId, toolCallId?, action}` only, never a
 * token — until its last sibling is decided, and the set then goes out as ONE
 * continuation.
 */
import { conversationApi } from '@/entities/conversation';
import type { ChatMessage } from '@/features/chat-messages';
import { ToolActionStatus } from '@/shared/lib/chat';

import {
  claimHitlSubmission,
  forgetHitlDecisions,
  queueHitlDecision,
  queuedHitlDecisions,
  releaseHitlSubmission,
  retainHitlDecisions,
  type HitlQueueScope,
  type QueuedHitlDecision,
} from '../../model/hitlDecisionQueue';
import {
  buildDeclinedServersList,
  readServerUrl,
  trackMcpAuthDecision,
} from './useChatBoxHandlers.helpers';
import type { ChatBoxHandlerDeps, ToolActionLike } from './useChatBoxHandlers.helpers';
import { undeliveredText } from './useChatBoxHandlers.turns';

import type { McpAuthorizationAction } from './useChatBoxHandlers.authorization.types';

function exactRequestId(action: ToolActionLike | undefined): string | undefined {
  const meta = action?.toolMeta ?? {};
  const value = action?.authorizationRequestId ?? meta['authorization_request_id']
    ?? meta['interrupt_id'] ?? meta['tool_run_id'] ?? meta['tool_call_id'] ?? action?.id;
  return typeof value === 'string' && value !== '' ? value : undefined;
}

function toolCallId(action: ToolActionLike): string | undefined {
  const value = action.toolMeta?.['tool_call_id'];
  return typeof value === 'string' && value !== '' ? value : undefined;
}

function pendingActions(message: ChatMessage): readonly ToolActionLike[] {
  return ((message.toolActions ?? []) as readonly ToolActionLike[]).filter(
    (action) => action.status === ToolActionStatus.actionRequired && exactRequestId(action),
  );
}

function findSelectedAction(
  actions: readonly ToolActionLike[],
  requestId: string | undefined,
): ToolActionLike | undefined {
  if (!requestId) return actions[0];
  return actions.find((action) => exactRequestId(action) === requestId);
}

/**
 * Cards decided and taken off screen, held for this page life so a refused
 * set can be put back. In memory only — they carry tool detail, which is never
 * persisted. Bounded like the queue (oldest response evicted).
 */
const MAX_HELD_RESPONSES = 8;
const heldCards = new Map<string, Map<string, ToolActionLike>>();

function heldKey(scope: HitlQueueScope): string {
  return JSON.stringify([scope.conversation, scope.message]);
}

function holdCards(scope: HitlQueueScope, cards: readonly ToolActionLike[]): void {
  const key = heldKey(scope);
  const held = heldCards.get(key) ?? new Map<string, ToolActionLike>();
  for (const card of cards) {
    const id = exactRequestId(card);
    if (id) held.set(id, card);
  }
  heldCards.delete(key);
  heldCards.set(key, held);
  while (heldCards.size > MAX_HELD_RESPONSES) {
    const oldest = heldCards.keys().next().value;
    if (oldest === undefined) break;
    heldCards.delete(oldest);
  }
}

function heldFor(scope: HitlQueueScope): readonly ToolActionLike[] {
  return [...(heldCards.get(heldKey(scope))?.values() ?? [])];
}

function releaseScope(scope: HitlQueueScope): void {
  forgetHitlDecisions(scope);
  heldCards.delete(heldKey(scope));
}

function authorizationScope(deps: ChatBoxHandlerDeps, messageId: string): HitlQueueScope | undefined {
  return deps.conversationUuid ? { conversation: deps.conversationUuid, message: messageId, kind: 'authorization' } : undefined;
}

function removeCards(message: ChatMessage, ids: ReadonlySet<string>, fields: Partial<ChatMessage> = {}): ChatMessage {
  const toolActions = ((message.toolActions ?? []) as readonly ToolActionLike[])
    .filter((action) => !ids.has(exactRequestId(action) ?? ''));
  return { ...message, ...fields, toolActions: toolActions as unknown as ChatMessage['toolActions'] };
}

function decidedCredentialKeys(
  cards: readonly ToolActionLike[],
  decisions: readonly QueuedHitlDecision[],
): { readonly tokenKeys: ReadonlySet<string>; readonly declinedUrls: ReadonlySet<string> } {
  const selected = new Map(decisions.map((decision) => [decision.interruptId, decision.action]));
  const tokenKeys = new Set<string>();
  const declinedUrls = new Set<string>();
  for (const action of cards) {
    const decision = selected.get(exactRequestId(action) ?? '');
    const key = readServerUrl(action);
    if (decision === 'authorize' && key) tokenKeys.add(key);
    if (decision === 'skip') {
      const actualUrl = action.toolMeta?.['server_url'];
      const url = typeof actualUrl === 'string' && actualUrl ? actualUrl : key;
      if (url) declinedUrls.add(url);
    }
  }
  return { tokenKeys, declinedUrls };
}

function continuationBody(
  deps: ChatBoxHandlerDeps,
  message: ChatMessage,
  cards: readonly ToolActionLike[],
  decisions: readonly QueuedHitlDecision[],
): Record<string, unknown> | undefined {
  const projectId = Number(deps.projectId);
  if (!Number.isSafeInteger(projectId) || projectId <= 0 || !deps.conversationUuid) return undefined;
  const { tokenKeys, declinedUrls } = decidedCredentialKeys(cards, decisions);
  // A continuation carries only the credentials for its exact decided cards.
  // Unrelated browser tokens and prior declines are not authority for this run.
  const tokens = Object.fromEntries(Object.entries(deps.getMcpTokens?.() ?? {})
    .filter(([key]) => tokenKeys.has(key)));
  const common = {
    project_id: projectId,
    conversation_uuid: deps.conversationUuid,
    message_id: message.id,
    thread_id: message.threadId ?? '',
    mcp_tokens: tokens,
    ignored_mcp_servers: [],
    user_declined_mcp_servers: buildDeclinedServersList(deps.sessionDeclinedMcpServersRef)
      .filter((entry) => typeof entry['server_url'] === 'string' && declinedUrls.has(entry['server_url'])),
  };
  const first = decisions[0];
  if (decisions.length === 1 && first) {
    return {
      ...common,
      hitl_resume: false,
      hitl_decisions: [],
      authorization_request_id: first.interruptId,
      authorization_action: first.action,
    };
  }
  return {
    ...common,
    hitl_resume: true,
    hitl_decisions: decisions.map((decision) => ({
      interrupt_id: decision.interruptId,
      ...(decision.toolCallId ? { tool_call_id: decision.toolCallId } : {}),
      guardrail_type: 'mcp_auth',
      action: decision.action,
    })),
    authorization_request_id: '',
    authorization_action: '',
  };
}

/**
 * Record the session-declined servers for exactly the decided cards — at
 * submit, so a set restored after a reload (when the session ref is empty)
 * still sends the declines it was decided with.
 */
function trackDecisions(deps: ChatBoxHandlerDeps, cards: readonly ToolActionLike[], decisions: readonly QueuedHitlDecision[]): void {
  const byId = new Map(decisions.map((decision) => [decision.interruptId, decision.action]));
  for (const card of cards) {
    const action = byId.get(exactRequestId(card) ?? '');
    if (action) trackMcpAuthDecision(deps.sessionDeclinedMcpServersRef, card, readServerUrl(card), action === 'skip');
  }
}

function putBackCards(deps: ChatBoxHandlerDeps, messageId: string, cards: readonly ToolActionLike[], exception: string): void {
  deps.setChatHistory((previous) => previous.map((item) => {
    if (item.id !== messageId) return item;
    const ids = new Set(((item.toolActions ?? []) as readonly ToolActionLike[]).map((action) => exactRequestId(action)));
    const restored = cards.filter((card) => !ids.has(exactRequestId(card)));
    return {
      ...item,
      toolActions: [...(item.toolActions ?? []), ...restored] as ChatMessage['toolActions'],
      isLoading: false,
      isStreaming: false,
      exception,
    };
  }));
}

/** Submit the complete set and settle by the route's answer. Never falls back to the socket. */
async function submitAuthorizationSet(
  deps: ChatBoxHandlerDeps,
  message: ChatMessage,
  scope: HitlQueueScope,
  cards: readonly ToolActionLike[],
  decisions: readonly QueuedHitlDecision[],
): Promise<void> {
  trackDecisions(deps, cards, decisions);
  const body = continuationBody(deps, message, cards, decisions);
  if (!body || !deps.continueStreamedExecution || !deps.conversationUuid) {
    releaseScope(scope);
    putBackCards(deps, message.id, cards, undeliveredText());
    return;
  }
  deps.setStreamingInfo(message.questionId ?? message.id);
  const outcome = await deps.continueStreamedExecution({
    conversationUuid: deps.conversationUuid,
    contract: conversationApi.contracts.continueAuthorization,
    body,
  });
  releaseScope(scope);
  if (outcome.started) return;
  if (outcome.reason === 'already-resolved') {
    // The set was consumed elsewhere: the cards stay answered, quietly.
    deps.setChatHistory((previous) => previous.map((item) => (
      item.id === message.id ? { ...item, isLoading: false, isStreaming: false } : item
    )));
    return;
  }
  putBackCards(deps, message.id, cards, outcome.reason === 'rejected' ? outcome.message : undeliveredText());
}

/** Record ONE card's decision; resume only once every pending request of the response has one. */
export async function resumeMcpAuthorization(
  deps: ChatBoxHandlerDeps,
  messageId: string,
  decision: McpAuthorizationAction,
  selectedRequestId?: string,
): Promise<void> {
  const message = deps.chatHistory.find((item) => item.id === messageId);
  if (!message) return;
  const pending = pendingActions(message);
  const selected = findSelectedAction(pending, selectedRequestId);
  const interruptId = exactRequestId(selected);
  const scope = authorizationScope(deps, messageId);
  if (!selected || !interruptId) return;
  if (!scope) {
    deps.setChatHistory((previous) => previous.map((item) => (item.id === messageId ? { ...item, exception: undeliveredText() } : item)));
    return;
  }
  const queued = queueHitlDecision(scope, { interruptId, toolCallId: toolCallId(selected), action: decision });
  if (!queued.queued) return;
  holdCards(scope, [selected]);
  const byId = new Map(queuedHitlDecisions(scope).map((entry) => [entry.interruptId, entry]));
  const remaining = pending.filter((action) => !byId.has(exactRequestId(action) ?? ''));
  const startsExecution = remaining.length === 0;
  deps.setChatHistory((previous) => previous.map((item) => (
    item.id === messageId
      ? removeCards(item, new Set([interruptId]), startsExecution ? { isLoading: true, isStreaming: true, exception: undefined } : {})
      : item
  )));
  if (!startsExecution) return;
  const cards = heldFor(scope);
  const decisions = cards
    .map((card) => byId.get(exactRequestId(card) ?? ''))
    .filter((item): item is QueuedHitlDecision => item !== undefined);
  if (!claimHitlSubmission(scope)) return;
  try {
    await submitAuthorizationSet(deps, message, scope, cards, decisions);
  } finally {
    releaseHitlSubmission(scope);
  }
}

/**
 * Re-apply authorization decisions persisted before a reload: their cards come
 * off screen again (held for a possible put-back); decisions for requests the
 * server no longer holds pending are dropped. A restore that would leave a
 * response with no card to click drops the queue instead.
 */
export function restoreQueuedAuthorizations(
  history: readonly ChatMessage[],
  conversation: string | undefined,
): readonly ChatMessage[] {
  if (!conversation) return history;
  let changed = false;
  const restored = history.map((message) => {
    const pending = pendingActions(message);
    if (pending.length < 2) return message;
    const scope: HitlQueueScope = { conversation, message: message.id, kind: 'authorization' };
    const ids = new Set(pending.map((action) => exactRequestId(action) ?? ''));
    const kept = new Set(retainHitlDecisions(scope, ids).map((decision) => decision.interruptId));
    if (kept.size === 0) return message;
    if (kept.size >= ids.size) {
      releaseScope(scope);
      return message;
    }
    holdCards(scope, pending.filter((action) => kept.has(exactRequestId(action) ?? '')));
    changed = true;
    return removeCards(message, kept);
  });
  return changed ? restored : history;
}
