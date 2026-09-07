/** Exact durable continuation for delegated OAuth authorization requests. */
import { conversationApi } from '@/entities/conversation';
import type { ChatMessage } from '@/features/chat-messages';
import { ToolActionStatus } from '@/shared/lib/chat';

import {
  buildDeclinedServersList,
  readServerUrl,
  revertContinuation,
  trackMcpAuthDecision,
} from './useChatBoxHandlers.helpers';
import type { ChatBoxHandlerDeps, ToolActionLike } from './useChatBoxHandlers.helpers';
import { undeliveredText } from './useChatBoxHandlers.turns';

export type McpAuthorizationAction = 'authorize' | 'skip';

interface McpAuthorizationDecision {
  readonly interruptId: string;
  readonly toolCallId?: string | undefined;
  readonly action: McpAuthorizationAction;
}

export interface McpAuthorizationBatch {
  readonly original: ChatMessage;
  readonly decisions: Map<string, McpAuthorizationDecision>;
}

function exactRequestId(action: ToolActionLike | undefined): string | undefined {
  const meta = action?.toolMeta ?? {};
  const value = action?.authorizationRequestId ?? meta['authorization_request_id'] ?? meta['interrupt_id'] ?? action?.id;
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

function sameAuthorizationGroup(
  action: ToolActionLike,
  selected: ToolActionLike,
): boolean {
  const selectedKey = readServerUrl(selected);
  return action === selected || Boolean(selectedKey && readServerUrl(action) === selectedKey);
}

function applyDecision(
  deps: ChatBoxHandlerDeps,
  batch: McpAuthorizationBatch,
  action: ToolActionLike,
  decision: McpAuthorizationAction,
): void {
  const interruptId = exactRequestId(action);
  if (!interruptId) return;
  batch.decisions.set(interruptId, {
    interruptId,
    toolCallId: toolCallId(action),
    action: decision,
  });
  trackMcpAuthDecision(
    deps.sessionDeclinedMcpServersRef,
    action,
    readServerUrl(action),
    decision === 'skip',
  );
}

function optimisticAuthorizationUpdate(
  deps: ChatBoxHandlerDeps,
  messageId: string,
  decidedIds: ReadonlySet<string>,
  startsExecution: boolean,
): void {
  deps.setChatHistory((previous) => previous.map((message) => {
    if (message.id !== messageId) return message;
    const toolActions = ((message.toolActions ?? []) as readonly ToolActionLike[])
      .filter((action) => !decidedIds.has(exactRequestId(action) ?? ''));
    return {
      ...message,
      toolActions: toolActions as unknown as ChatMessage['toolActions'],
      ...(startsExecution ? { isLoading: true, isStreaming: true, exception: undefined } : {}),
    };
  }));
}

function decidedCredentialKeys(
  message: ChatMessage,
  decisions: readonly McpAuthorizationDecision[],
): { readonly tokenKeys: ReadonlySet<string>; readonly declinedUrls: ReadonlySet<string> } {
  const selected = new Map(decisions.map((decision) => [decision.interruptId, decision.action]));
  const tokenKeys = new Set<string>();
  const declinedUrls = new Set<string>();
  for (const action of pendingActions(message)) {
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
  decisions: readonly McpAuthorizationDecision[],
): Record<string, unknown> | undefined {
  const projectId = Number(deps.projectId);
  if (!Number.isSafeInteger(projectId) || projectId <= 0 || !deps.conversationUuid) return undefined;
  const { tokenKeys, declinedUrls } = decidedCredentialKeys(message, decisions);
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

/** Collect decisions without granting authority to unrelated cards or tokens. */
function collectAuthorizationBatch(
  deps: ChatBoxHandlerDeps,
  message: ChatMessage,
  decision: McpAuthorizationAction,
  selectedRequestId?: string,
): McpAuthorizationBatch | undefined {
  const actions = pendingActions(message);
  const selected = findSelectedAction(actions, selectedRequestId);
  if (!selected) return;

  const batches = deps.sessionMcpAuthorizationBatchesRef?.current;
  const batch: McpAuthorizationBatch = batches?.get(message.id) ?? { original: message, decisions: new Map() };
  batches?.set(message.id, batch);
  for (const action of actions.filter((item) => sameAuthorizationGroup(item, selected))) {
    applyDecision(deps, batch, action, decision);
  }
  return batch;
}

/** Record one card decision and resume only after every parallel request has a decision. */
export async function resumeMcpAuthorization(
  deps: ChatBoxHandlerDeps,
  messageId: string,
  decision: McpAuthorizationAction,
  selectedRequestId?: string,
): Promise<void> {
  const message = deps.chatHistory.find((item) => item.id === messageId);
  if (!message) return;
  const batch = collectAuthorizationBatch(deps, message, decision, selectedRequestId);
  if (!batch) return;
  const batches = deps.sessionMcpAuthorizationBatchesRef?.current;

  const originalActions = pendingActions(batch.original);
  const unresolved = originalActions.filter((action) => !batch.decisions.has(exactRequestId(action) ?? ''));
  optimisticAuthorizationUpdate(deps, messageId, new Set(batch.decisions.keys()), unresolved.length === 0);
  if (unresolved.length > 0) return;

  const decisions = originalActions
    .map((action) => batch.decisions.get(exactRequestId(action) ?? ''))
    .filter((item): item is McpAuthorizationDecision => item !== undefined);
  const body = continuationBody(deps, batch.original, decisions);
  batches?.delete(messageId);
  if (!body || !deps.continueStreamedExecution || !deps.conversationUuid) {
    revertContinuation(deps.setChatHistory, batch.original, undeliveredText());
    return;
  }
  deps.setStreamingInfo(message.questionId ?? messageId);
  const outcome = await deps.continueStreamedExecution({
    conversationUuid: deps.conversationUuid,
    contract: conversationApi.contracts.continueAuthorization,
    body,
  });
  if (outcome.started) return;
  revertContinuation(
    deps.setChatHistory,
    batch.original,
    outcome.reason === 'rejected' ? outcome.message : undeliveredText(),
  );
}
