/**
 * Ends the in-flight state of a chat message when the transport gives up, and
 * — when there is nothing in flight to end — puts the failure on screen at all.
 *
 * Split out of `model/useChatStreamTransport.ts` to keep that file inside the
 * §3.5 file-length budget of 400 lines. The HITL resume path added the
 * `subscribeToRun` and `resume` callbacks, which pushed it to 415.
 */
import { t } from '@/shared/i18n';
import { ROLES } from '@/shared/lib/enums';

import type { ExecutionEventData } from '@/shared/api/sse';

import { nowIso, type ChatStreamContext } from './chatStreamShared';
import type { ChatMessage } from './convertMessagesToChatHistory';

/**
 * Clear the in-flight flags on whatever is still streaming.
 *
 * A transport failure leaves the run's message spinning forever otherwise:
 * the frames that would have ended it are exactly the ones that stopped
 * arriving.
 *
 * Returns the SAME array when nothing was in flight, which is what lets
 * `recordStreamFailure` below tell "settled a message" from "there was no
 * message to settle" without a second pass over the history.
 */
export function settleInFlight(history: readonly ChatMessage[], exception?: unknown): readonly ChatMessage[] {
  let changed = false;
  const next = history.map((message) => {
    if (!message.isStreaming && !message.isLoading) return message;
    changed = true;
    return {
      ...message,
      isStreaming: false,
      isLoading: false,
      isRegenerating: false,
      ...(exception !== undefined ? { exception } : {}),
    };
  });
  return changed ? next : history;
}

/**
 * Read the sentence the server actually sent with `execution.failed`.
 *
 * The payload is `{code, safe_message, retryable}` for every producer of that
 * event — `internal/application/output/runtime_failure.go:163-167` builds the
 * browser data for a runtime refusal, and the two canned failures in
 * `internal/infra/db/repos/command_outbox.go:29-30` (plus the index-ingest one
 * in `index_ingest_jobs.go:32`) spell out the same three keys. `safe_message`
 * is the only human sentence in it, so it is what the user sees; `code` (e.g.
 * `UNSUPPORTED_CAPABILITY`) is a poor second, but it is still the difference
 * between naming the refusal and hiding it.
 *
 * NOT read: `error`, which is what this was reading before and is a key
 * nothing writes. It is absent from all three producers, and the socket-shaped
 * `error` / `llm_error` frames that share this envelope do not use it either —
 * `chatStreamTurnFrames.ts:309` takes their reason from `content` /
 * `exception`. Reading a key no producer sends is how a refusal with a perfectly
 * good sentence in it reached the user as "The agent run failed."
 */
export function runtimeFailureReason(frame: ExecutionEventData): string {
  for (const key of ['safe_message', 'code'] as const) {
    const value = frame[key];
    if (typeof value === 'string' && value.trim() !== '') return value;
  }
  return t('chatMessages.stream.runFailed', 'The agent run failed.');
}

/**
 * Make a failure visible in the transcript, whether or not a message existed.
 *
 * A refusal can arrive BEFORE any progress frame — a runtime that rejects the
 * agent profile emits `execution.failed` as the first and only event of the
 * run. The reducer creates the assistant message on the first frame it folds
 * in (`chatStreamShared.ts`'s `createAssistantMessage`), so at that point the
 * history holds the user's question and nothing else: `settleInFlight` matches
 * nothing, returns the history unchanged, and the composer re-enables over an
 * empty transcript. Measured on a live stack against the native Rust runtime,
 * whose `UNSUPPORTED_CAPABILITY` refusal was durably recorded in
 * `elitea_runtime.execution_replay_events` and rendered as nothing at all.
 *
 * The message is therefore appended, carrying the reason as `exception` — the
 * shape `ApplicationAnswer` renders through `ErrorTrace`, which takes a plain
 * string as its headline.
 */
export function recordStreamFailure(
  history: readonly ChatMessage[],
  exception: unknown,
  context: ChatStreamContext | undefined,
  /** The question this refused turn answered — the transport's, since no frame supplied one. */
  questionId?: string,
  /** Use the accepted run's persisted answer identity before its first frame. */
  responseMessageId?: string,
): readonly ChatMessage[] {
  const settled = settleInFlight(history, exception);
  if (settled !== history) return settled;
  const identity = context ?? {};
  return [
    ...history,
    {
      id: responseMessageId ?? crypto.randomUUID(),
      role: ROLES.Assistant,
      name: identity.name ?? '',
      content: '',
      createdAt: nowIso(identity),
      isStreaming: false,
      isLoading: false,
      exception,
      ...(questionId !== undefined ? { questionId } : {}),
      ...(identity.participantId !== undefined ? { participantId: identity.participantId } : {}),
      ...(identity.avatar !== undefined ? { avatar: identity.avatar } : {}),
    },
  ];
}
