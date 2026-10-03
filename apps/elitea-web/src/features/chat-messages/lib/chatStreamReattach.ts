/**
 * lib/chatStreamReattach.ts — reopen the stream of a turn that was in flight
 * when the page loaded (#6654).
 *
 * A reload mid-turn seeds the transcript from the conversation read. Its last
 * row says `is_streaming: true` and carries the execution id in `task_id`, but
 * nothing subscribed to that execution again. The answer stayed a "..."
 * placeholder, and the seed rule kept the unsettled row on screen even after a
 * refetch had the final text. The execution log is durable, so the client
 * replays it from cursor 0 into the same message instead.
 *
 * The events path is built here because no route returns `events_url` for an
 * execution that this page did not start. The shape is the one every start
 * route answers with (`agentexecution/route.go`).
 */
import { ROLES } from '@/shared/lib/enums';

import type { ChatMessage } from './convertMessagesToChatHistory';

/**
 * The shape of an execution id the client may put in a URL path segment.
 *
 * Main mints execution ids as random hex (`submit_job.go` randomID), but the
 * id reaches this client from a conversation read, not from a start route.
 * Accept only letters, digits, `-` and `_`. That refuses `.`, `..`, `/`, `%`
 * and every other character URL normalization or decoding can turn into a
 * different same-origin route.
 */
const EXECUTION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9_-]{0,255}$/;

/** Whether a value is an execution id this client may subscribe to. */
function isExecutionId(value: string): boolean {
  return EXECUTION_ID_PATTERN.test(value);
}

/** The in-flight turn a fresh page can observe again. */
export interface ReattachableTurn {
  readonly messageId: string;
  readonly executionId: string;
  readonly questionId?: string | undefined;
}

/**
 * The transcript's LAST message, when it is an assistant turn still in flight
 * with an execution to observe. Only the last one: an older row that kept the
 * flag is history, the same rule `hasUnsettledTurn` applies to the seed.
 */
export function findReattachableTurn(history: readonly ChatMessage[]): ReattachableTurn | undefined {
  const last = history[history.length - 1];
  if (last === undefined || last.role !== ROLES.Assistant) return undefined;
  if (last.isStreaming !== true) return undefined;
  const executionId = typeof last.taskId === 'string' ? last.taskId.trim() : '';
  if (!isExecutionId(executionId) || typeof last.id !== 'string' || last.id === '') return undefined;
  return { messageId: last.id, executionId, ...(last.questionId ? { questionId: last.questionId } : {}) };
}

/**
 * The durable replay stream of one execution, or `undefined` when the
 * execution id does not have the expected shape. Nothing subscribes then.
 */
export function executionEventsPath(projectId: string | number, executionId: string): string | undefined {
  if (!isExecutionId(executionId)) return undefined;
  return `/api/v2/executions/${encodeURIComponent(String(projectId))}/${encodeURIComponent(executionId)}/events`;
}

/**
 * Clear what the seed drew for the turn, so a replay from cursor 0 rebuilds it
 * once. The seed shows "..." for a streaming row; folding the replayed chunks
 * onto it would print "...answer", and the tool rows would appear twice.
 */
export function resetTurnForReplay(history: readonly ChatMessage[], messageId: string): readonly ChatMessage[] {
  const index = history.findIndex((message) => message.id === messageId);
  if (index === -1) return history;
  const next = [...history];
  next[index] = { ...history[index], content: '', messageItems: [], toolActions: [], isStreaming: true, isLoading: true } as ChatMessage;
  return next;
}
