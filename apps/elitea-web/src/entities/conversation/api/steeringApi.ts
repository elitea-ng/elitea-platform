/**
 * Steering of a running chat turn (client contract 1.27).
 *
 * A message that the user sends while a cloud turn runs is queued against the
 * running answer. The worker injects it at the next safe point. These
 * adapters wrap the five generated operations (`steerChatExecution`,
 * `listChatExecutionSteering`, `updateChatExecutionSteering`,
 * `removeChatExecutionSteering`, `handOffChatExecutionSteering`) and turn
 * each answer into a value: a
 * refusal is `{ ok: false, code }` with the contract's `error` code, never a
 * throw, so that the caller can choose the fallback.
 *
 * `eliteaFetch` returns the `{ data, status, headers }` envelope (see
 * `stopChatTask`'s neighbour `fetchData`), so each adapter reads `.data`.
 */
import {
  STEERING_ERROR,
  parseSteeringList,
  parseSteeringMessage,
  steeringErrorCode,
  steeringRetryAfterMs,
  type SteeringItem,
} from '@elitea/client-core/stream';

import {
  handOffChatExecutionSteering,
  listChatExecutionSteering,
  removeChatExecutionSteering,
  steerChatExecution,
  updateChatExecutionSteering,
} from '@/shared/api/generated/chat/chat';
import { EliteaApiError } from '@/shared/api/generated/mutator';

/** The running answer that a steering call addresses. */
export interface SteeringTarget {
  readonly projectId: string | number;
  readonly responseMessageId: string;
}

/** The answer of one steering call. */
export type SteeringCallResult =
  | { readonly ok: true; readonly item: SteeringItem | undefined }
  | {
      readonly ok: false;
      /** The contract's `error` code (`turn_not_running`, `steering_not_supported`, ...), if the server sent one. */
      readonly code: string | undefined;
      /** The HTTP status, or `undefined` for a network failure. */
      readonly status: number | undefined;
      /** On a 429 `rate_limited` only: the wait before the call goes again with the SAME key. */
      readonly retryAfterMs?: number;
    };

function refusal(error: unknown): SteeringCallResult {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const code = steeringErrorCode(error.failure.body);
    if (code === STEERING_ERROR.rateLimited) {
      return { ok: false, code, status: error.failure.status, retryAfterMs: steeringRetryAfterMs(error.failure.body) };
    }
    return { ok: false, code, status: error.failure.status };
  }
  if (error instanceof EliteaApiError && error.failure.kind === 'auth') {
    return { ok: false, code: undefined, status: error.failure.status };
  }
  return { ok: false, code: undefined, status: undefined };
}

/** A fresh `Idempotency-Key`: a uuid without braces (`^[A-Za-z0-9_-]+$`, at most 128). */
export function newSteeringKey(): string {
  return crypto.randomUUID();
}

/**
 * Queues `content` against the running answer. Send the same `idempotencyKey`
 * again on a retry: the server then answers the first message again.
 */
export async function queueSteering(target: SteeringTarget, content: string, idempotencyKey: string): Promise<SteeringCallResult> {
  try {
    const envelope = await steerChatExecution(String(target.projectId), target.responseMessageId, { content }, {
      headers: { 'Idempotency-Key': idempotencyKey },
    });
    return { ok: true, item: parseSteeringMessage(envelope.data) };
  } catch (error) {
    return refusal(error);
  }
}

/** Every steering message of the answer, oldest first, in all states. Throws when the list cannot be read. */
export async function listSteering(target: SteeringTarget, signal?: AbortSignal): Promise<SteeringItem[]> {
  const envelope = await listChatExecutionSteering(String(target.projectId), target.responseMessageId, signal ? { signal } : undefined);
  return parseSteeringList(envelope.data);
}

/** Changes the text of a pending message. 409 `steering_not_pending`: the turn injected it first. */
export async function editSteering(target: SteeringTarget, steeringId: string, content: string): Promise<SteeringCallResult> {
  try {
    const envelope = await updateChatExecutionSteering(String(target.projectId), target.responseMessageId, steeringId, { content });
    return { ok: true, item: parseSteeringMessage(envelope.data) };
  } catch (error) {
    return refusal(error);
  }
}

/** Removes a pending message (204, also when it is already removed). 409 `steering_not_pending`: it was injected. */
export async function removeSteering(target: SteeringTarget, steeringId: string): Promise<SteeringCallResult> {
  try {
    await removeChatExecutionSteering(String(target.projectId), target.responseMessageId, steeringId);
    return { ok: true, item: undefined };
  } catch (error) {
    return refusal(error);
  }
}

/**
 * Takes the caller's `turn_ended` messages of the answer (now `handed_off`)
 * and returns them, oldest first. The caller sends ONLY these texts, as one
 * message with the question id `questionId`. An empty list means that the
 * server (or another tab) took them first: send nothing. Send the same
 * `idempotencyKey` and `questionId` again on a retry: the server then answers
 * the same messages again. When no message with `questionId` comes, the
 * server starts that turn itself after about a minute. Throws when the call
 * fails.
 */
export async function handOffSteering(target: SteeringTarget, idempotencyKey: string, questionId: string): Promise<SteeringItem[]> {
  const envelope = await handOffChatExecutionSteering(String(target.projectId), target.responseMessageId, { question_id: questionId }, {
    headers: { 'Idempotency-Key': idempotencyKey },
  });
  return parseSteeringList(envelope.data);
}
