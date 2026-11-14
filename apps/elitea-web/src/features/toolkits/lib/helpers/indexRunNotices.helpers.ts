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

import { SocketMessageType } from '../../indexes/lib/constants/indexDetails.constants';
import type { IndexChatMessage } from '../../indexes/lib/helpers/indexChatMessage.types';
import type { ToolkitChatMessage } from '../hooks/useToolkitChat.types';

/** The id of the placeholder, so the summary (or a failure) can replace exactly it. */
const INDEX_RUN_PROGRESS_ID = 'index-run-progress';

const SYSTEM_PARTICIPANT = 'system';

function buildIndexRunProgressMessage(nowMs: number): IndexChatMessage {
  return {
    id: INDEX_RUN_PROGRESS_ID,
    role: ROLES.Assistant,
    content: `🔄 ${t('features.toolkits.indexRun.started', 'Indexing in progress. The steps and the result appear here.')}`,
    isLoading: true,
    isStreaming: true,
    created_at: nowMs,
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

/** `history` with the progress placeholder removed and `next` appended; the placeholder is the only message dropped. */
export function settleIndexRunProgress(history: readonly ToolkitChatMessage[], next: ToolkitChatMessage): ToolkitChatMessage[] {
  return [...history.filter((message) => message.id !== INDEX_RUN_PROGRESS_ID), next];
}

/** `history` plus the placeholder, unless one is already there (a replayed start must not stack them). */
export function addIndexRunProgress(history: readonly ToolkitChatMessage[], nowMs: number): ToolkitChatMessage[] {
  if (history.some((message) => message.id === INDEX_RUN_PROGRESS_ID)) return [...history];
  return [...history, buildIndexRunProgressMessage(nowMs)];
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
export function ensureRunMessage(history: readonly ToolkitChatMessage[], frameType: string, messageId: string | undefined, nowMs: number): ToolkitChatMessage[] {
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
    created_at: nowMs,
    participant_id: SYSTEM_PARTICIPANT,
  };
  return [...history.filter((message) => message.id !== INDEX_RUN_PROGRESS_ID), created];
}
