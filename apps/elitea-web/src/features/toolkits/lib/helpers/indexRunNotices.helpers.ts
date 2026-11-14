/**
 * What the index view says about ONE run, live (#2041).
 *
 * Until a run's first node event arrives the panel used to read "No messages
 * yet", and when the run finished only the list refreshed, so the outcome was
 * visible in History alone. Two messages close that gap without a reload:
 *
 *  - a PROGRESS placeholder, added the moment the start call returns the
 *    execution id, so the panel shows a running state at once;
 *  - a SUMMARY, built from the `index.ingest.completed` frame (status,
 *    message and the typed counts `indexReplayData` carries), which replaces
 *    the placeholder when the run settles.
 *
 * Counts are the worker's own typed fields; an older worker's frame carries
 * none and the summary then says only what the frame says (status + message).
 * Strings are composed here with plain template literals, never `{{}}`
 * placeholders: a bundle value wins over the call-site fallback and an
 * options-less `t` would print the placeholder literally.
 */
import type { ExecutionEventData } from '@/shared/api/sse';
import { t } from '@/shared/i18n';
import { ROLES } from '@/shared/lib/enums';

import { IndexStatuses, SocketMessageType } from '../../indexes/lib/constants/indexDetails.constants';
import type { IndexChatMessage } from '../../indexes/lib/helpers/indexChatMessage.types';
import type { ToolkitChatMessage } from '../hooks/useToolkitChat.types';

/** The id of the placeholder, so the summary (or a failure) can replace exactly it. */
const INDEX_RUN_PROGRESS_ID = 'index-run-progress';

const SYSTEM_PARTICIPANT = 'system';

/**
 * `startedAtMs` is the moment THIS client started the run, or `undefined` for
 * a run it only joined (a start answered 409 names a run begun elsewhere, at a
 * time this client was never told). The placeholder keeps it in `created_at`,
 * with 0 for "unknown", and the summary measures its duration from it: no
 * duration is better than a wrong one.
 */
function buildIndexRunProgressMessage(startedAtMs: number | undefined): IndexChatMessage {
  return {
    id: INDEX_RUN_PROGRESS_ID,
    role: ROLES.Assistant,
    content: `🔄 ${t('features.toolkits.indexRun.started', 'Indexing in progress. The steps and the result appear here.')}`,
    isLoading: true,
    isStreaming: true,
    created_at: startedAtMs ?? 0,
    participant_id: SYSTEM_PARTICIPANT,
  };
}

function asCount(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : undefined;
}

function noun(count: number, singular: string, plural: string): string {
  return `${String(count)} ${count === 1 ? singular : plural}`;
}

/** `{"too_large": 2}` -> `too large 2`. Only closed lower-snake reasons ever arrive; anything else is dropped. */
function skippedParts(value: unknown): string[] {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return [];
  const parts: string[] = [];
  for (const [reason, count] of Object.entries(value as Record<string, unknown>)) {
    const n = asCount(count);
    if (n !== undefined && n > 0 && /^[a-z][a-z0-9_]*$/.test(reason)) parts.push(`${reason.replaceAll('_', ' ')} ${String(n)}`);
  }
  return parts;
}

function durationText(startedAtMs: number | undefined, nowMs: number): string | undefined {
  if (startedAtMs === undefined || nowMs < startedAtMs) return undefined;
  const seconds = Math.max(1, Math.round((nowMs - startedAtMs) / 1000));
  if (seconds < 60) return `${String(seconds)} s`;
  return `${String(Math.floor(seconds / 60))} min ${String(seconds % 60)} s`;
}

function headline(status: unknown): string {
  if (status === 'error') return `❌ ${t('features.toolkits.indexRun.failed', 'Indexing failed')}`;
  if (status === 'partly_indexed') return `⚠️ ${t('features.toolkits.indexRun.partly', 'Indexing finished with some documents skipped')}`;
  if (status === undefined || status === 'ok') return `✅ ${t('features.toolkits.indexRun.done', 'Indexing finished')}`;
  return `❌ ${t('features.toolkits.indexRun.failed', 'Indexing failed')}`;
}

function positive(value: unknown): number | undefined {
  const count = asCount(value);
  return count !== undefined && count > 0 ? count : undefined;
}

function indexedLine(frame: ExecutionEventData): string | undefined {
  const documents = positive(frame['indexed_documents']);
  if (documents === undefined) return undefined;
  const chunks = positive(frame['indexed_chunks']);
  const chunkText = chunks === undefined ? '' : ` (${noun(chunks, 'chunk', 'chunks')})`;
  return `${noun(documents, 'document', 'documents')} ${t('features.toolkits.indexRun.indexed', 'indexed')}${chunkText}`;
}

function unchangedLine(frame: ExecutionEventData): string | undefined {
  const unchanged = positive(frame['unchanged_documents']);
  return unchanged === undefined ? undefined : `${noun(unchanged, 'document', 'documents')} ${t('features.toolkits.indexRun.unchanged', 'already indexed (unchanged)')}`;
}

function skippedLine(frame: ExecutionEventData): string | undefined {
  const skipped = skippedParts(frame['skipped']);
  return skipped.length === 0 ? undefined : `${t('features.toolkits.indexRun.skipped', 'Skipped')}: ${skipped.join(', ')}`;
}

function failedChunksLine(frame: ExecutionEventData): string | undefined {
  const failed = positive(frame['failed_chunks']);
  return failed === undefined ? undefined : `${noun(failed, 'chunk', 'chunks')} ${t('features.toolkits.indexRun.chunksFailed', 'could not be embedded or written')}`;
}

function totalLine(frame: ExecutionEventData): string | undefined {
  const total = asCount(frame['total_documents']);
  return total === undefined ? undefined : `${t('features.toolkits.indexRun.inIndex', 'In the index now')}: ${noun(total, 'document', 'documents')}`;
}

/** The count lines of a summary, each only when the worker stated it. */
function countLines(frame: ExecutionEventData): string[] {
  return [indexedLine(frame), unchangedLine(frame), skippedLine(frame), failedChunksLine(frame), totalLine(frame)].filter(
    (line): line is string => line !== undefined,
  );
}

/** The summary message of a finished run: outcome, the worker's sentence, the counts and how long it took. */
export function buildIndexRunSummaryMessage(frame: ExecutionEventData, startedAtMs: number | undefined, nowMs: number): IndexChatMessage {
  const duration = durationText(startedAtMs, nowMs);
  const message = typeof frame['message'] === 'string' ? frame['message'].trim() : '';
  const head = duration === undefined ? headline(frame['status']) : `${headline(frame['status'])} (${duration})`;
  const lines = countLines(frame).map((line) => `- ${line}`);
  const content = [head, message, lines.join('\n')].filter((part) => part !== '').join('\n\n');
  return {
    id: crypto.randomUUID(),
    role: ROLES.Assistant,
    content,
    isStreaming: false,
    created_at: nowMs,
    participant_id: SYSTEM_PARTICIPANT,
  };
}

/** The run's recorded start: carried by the placeholder, and from it by the first run message that replaced it. 0 means unknown. */
function placeholderStart(history: readonly ToolkitChatMessage[]): number | undefined {
  const running = history.find((message) => {
    const flags = message as { isLoading?: boolean; isStreaming?: boolean };
    return message.id === INDEX_RUN_PROGRESS_ID || flags.isLoading === true || flags.isStreaming === true;
  }) as IndexChatMessage | undefined;
  return running !== undefined && running.created_at > 0 ? running.created_at : undefined;
}

/** Every message the run left running is finished: its spinner and streaming flag go, whatever the outcome. */
function finished(message: ToolkitChatMessage): ToolkitChatMessage {
  const running = message as { isLoading?: boolean; isStreaming?: boolean };
  return running.isLoading === true || running.isStreaming === true ? { ...message, isLoading: false, isStreaming: false } : message;
}

/** `history` with the run settled: the placeholder gone, every message it created finished, and `next` appended. */
export function settleIndexRunProgress(history: readonly ToolkitChatMessage[], next: ToolkitChatMessage): ToolkitChatMessage[] {
  return [...history.filter((message) => message.id !== INDEX_RUN_PROGRESS_ID).map(finished), next];
}

/** `settleIndexRunProgress` with the summary of a completed run, its duration measured from the recorded start. */
export function settleIndexRunWithSummary(history: readonly ToolkitChatMessage[], frame: ExecutionEventData, nowMs: number): ToolkitChatMessage[] {
  return settleIndexRunProgress(history, buildIndexRunSummaryMessage(frame, placeholderStart(history), nowMs));
}

/** The notice for a run the user stopped: "stopped", never "failed". The wording is the History tab's. */
export function buildIndexRunStoppedMessage(nowMs: number): IndexChatMessage {
  return {
    id: crypto.randomUUID(),
    role: ROLES.Assistant,
    content: `⏹️ ${t('features.toolkits.indexHistory.stopped', 'Indexing was stopped before it finished.')}`,
    isStreaming: false,
    created_at: nowMs,
    participant_id: SYSTEM_PARTICIPANT,
  };
}

/** The notice for a run this client can no longer follow (no live connection to the runtime): the run may still be going. */
export function buildIndexRunUnfollowedMessage(nowMs: number): IndexChatMessage {
  return {
    id: crypto.randomUUID(),
    role: ROLES.Assistant,
    content: `⚠️ ${t('features.toolkits.indexRun.unfollowed', 'The live view of this run could not be opened. The run may still be going; its result appears in History.')}`,
    isStreaming: false,
    created_at: nowMs,
    participant_id: SYSTEM_PARTICIPANT,
  };
}

/** `history` plus the placeholder, unless one is already there (a replayed start must not stack them). */
export function addIndexRunProgress(history: readonly ToolkitChatMessage[], startedAtMs: number | undefined): ToolkitChatMessage[] {
  if (history.some((message) => message.id === INDEX_RUN_PROGRESS_ID)) return [...history];
  return [...history, buildIndexRunProgressMessage(startedAtMs)];
}

/** The frame types the index reducer applies to an EXISTING message of the run (a step, a tool call, streamed text). */
const FRAMES_ON_A_RUN_MESSAGE: ReadonlySet<string> = new Set([
  SocketMessageType.AgentToolStart,
  SocketMessageType.AgentToolEnd,
  SocketMessageType.AgentToolError,
  SocketMessageType.AgentThinkingStep,
  SocketMessageType.AgentThinkingStepUpdate,
  SocketMessageType.AgentResponse,
  SocketMessageType.Chunk,
  SocketMessageType.AIMessageChunk,
]);

/**
 * The run's message for a progress frame that names one this panel has not
 * seen. The socket runtime announced a run with a `start_task` frame that
 * created that message; the execution stream carries no such frame, so without
 * this every step frame found nothing to attach to and was dropped (#2041).
 * The placeholder gives way to the first real message.
 */
export function ensureRunMessage(history: readonly ToolkitChatMessage[], frameType: string, messageId: string | undefined): ToolkitChatMessage[] {
  // A `start_task` frame brings its own running message, so the placeholder gives way to it.
  if (frameType === SocketMessageType.StartTask) return history.filter((message) => message.id !== INDEX_RUN_PROGRESS_ID);
  if (!FRAMES_ON_A_RUN_MESSAGE.has(frameType) || messageId === undefined || messageId === '') return [...history];
  if (history.some((message) => message.id === messageId)) return [...history];
  const created: IndexChatMessage = {
    id: messageId,
    role: ROLES.Assistant,
    content: `🔄 ${t('features.toolkits.indexRun.working', 'Indexing…')}`,
    isLoading: true,
    isStreaming: true,
    toolActions: [],
    created_at: placeholderStart(history) ?? 0,
    participant_id: SYSTEM_PARTICIPANT,
  };
  return [...history.filter((message) => message.id !== INDEX_RUN_PROGRESS_ID), created];
}

/**
 * The index state a `chat_predict` frame ends a run with on the socket.io
 * transport, or `undefined` for a frame that does not end it. It mirrors the
 * reducer's own `onFinish` calls (a tool error or a general error fails the
 * run, a streamed frame carrying a `finish_reason` completes it) so the finish
 * can be reported OUTSIDE the state updater, where side effects do not belong.
 */
export function socketFrameFinishState(frame: { readonly type: string; readonly response_metadata?: unknown }): string | undefined {
  if (frame.type === SocketMessageType.AgentToolError || frame.type === SocketMessageType.Error || frame.type === SocketMessageType.AgentException) {
    return IndexStatuses.fail;
  }
  const finishReason = (frame.response_metadata as { finish_reason?: unknown } | undefined)?.finish_reason;
  const streamed: readonly string[] = [SocketMessageType.AgentThinkingStep, SocketMessageType.AgentThinkingStepUpdate, SocketMessageType.AgentResponse, SocketMessageType.Chunk, SocketMessageType.AIMessageChunk];
  return streamed.includes(frame.type) && typeof finishReason === 'string' && finishReason !== '' ? IndexStatuses.success : undefined;
}
