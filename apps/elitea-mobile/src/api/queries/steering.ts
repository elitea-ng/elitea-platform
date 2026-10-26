import { randomUUID } from 'expo-crypto';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { ApiError, parseRetryAfter } from '@/api/errors';
import {
  STEERING_ERROR,
  STEERING_RETRY_LIMIT,
  parseSteeringList,
  parseSteeringMessage,
  steeringErrorCode,
  steeringRetryAfterMs,
  type SteeringItem,
} from '@elitea/client-core/stream';

/**
 * Steering of a running answer (client contract 1.27):
 *
 *   POST   /elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering        steerChatExecution
 *   GET    /elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering        listChatExecutionSteering
 *   PATCH  /elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering/{id}   updateChatExecutionSteering
 *   DELETE /elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering/{id}   removeChatExecutionSteering
 *   POST   /elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering/handoff handOffChatExecutionSteering
 *
 * A message sent while the answer runs is queued against it; the worker
 * injects it at the next safe point of the same turn. Each call answers a
 * value: a refusal is `{ ok: false, code }` with the contract's `error`
 * code (`turn_not_running`, `steering_not_supported`, `steering_queue_full`,
 * `steering_not_pending`, ...), so that the caller picks the fallback. A
 * network failure is `{ ok: false, status: undefined }`. A session failure
 * (401, 426) throws: the session layer already reacted to it. A 429
 * `rate_limited` also has `retryAfterMs`: the body's `retry_after_ms`, else
 * `Retry-After`, else 2 s.
 */

export type SteeringCallResult =
  | { ok: true; item: SteeringItem | undefined }
  | { ok: false; code: string | undefined; status: number | undefined; retryAfterMs?: number };

const UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

const ROUTE = '/elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering' as const;
const HANDOFF_ROUTE = '/elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering/handoff' as const;
const ITEM_ROUTE = '/elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering/{steering_id}' as const;

/** One key per intended message: a uuid without braces (`^[A-Za-z0-9_-]+$`, at most 128). */
export function newSteeringKey(uuid: () => string = randomUUID): string {
  return uuid().toLowerCase();
}

function validTarget(projectId: number, responseMessageId: string): boolean {
  return Number.isSafeInteger(projectId) && projectId > 0 && UUID.test(responseMessageId);
}

function path(projectId: number, responseMessageId: string) {
  return { project_id: String(projectId), response_message_id: responseMessageId };
}

/** Runs one call; a network failure or timeout becomes `{ ok: false, status: undefined }`. */
async function call(run: () => Promise<{ data?: unknown; error?: unknown; response: Response }>): Promise<SteeringCallResult> {
  let result: { data?: unknown; error?: unknown; response: Response };
  try {
    result = await run();
  } catch (error) {
    if (error instanceof ApiError && (error.kind === 'network' || error.kind === 'timeout')) return { ok: false, code: undefined, status: undefined };
    if (error instanceof ApiError && error.kind === 'http') return refused(error.code, error.status, undefined, error.retryAfterMs);
    throw error;
  }
  if (result.response.ok) return { ok: true, item: parseSteeringMessage(result.data) };
  const header = parseRetryAfter(result.response.headers.get('Retry-After'));
  return refused(steeringErrorCode(result.error), result.response.status, result.error, header);
}

function refused(code: string | undefined, status: number | undefined, body: unknown, headerMs: number | undefined): SteeringCallResult {
  if (code !== STEERING_ERROR.rateLimited) return { ok: false, code, status };
  return { ok: false, code, status, retryAfterMs: steeringRetryAfterMs(body, headerMs) };
}

/** Queues `content` against the running answer. A retry sends the SAME `idempotencyKey`. */
export function queueSteering(
  projectId: number,
  responseMessageId: string,
  content: string,
  idempotencyKey: string,
  client: ApiClient = sessionApiClient(),
): Promise<SteeringCallResult> {
  if (!validTarget(projectId, responseMessageId)) return Promise.resolve({ ok: false, code: 'steering_invalid', status: undefined });
  return call(() =>
    client.POST(ROUTE, {
      params: { path: path(projectId, responseMessageId), header: { 'Idempotency-Key': idempotencyKey } },
      body: { content },
    }),
  );
}

export interface SteerRetryOptions {
  client?: ApiClient;
  /** Resolves after `ms`. */
  sleep: (ms: number) => Promise<void>;
  /** A `rate_limited` refusal: the next try goes after `ms`. */
  onWait?: (ms: number) => void;
}

/**
 * Queues `content` and tries again with the SAME `idempotencyKey`: one more
 * time after a network failure, and after the wait of each `rate_limited`
 * refusal, at most `STEERING_RETRY_LIMIT` times. The key makes sure that
 * the server keeps the message one time only. Resolves to the last answer.
 */
export async function queueSteeringWithRetry(
  projectId: number,
  responseMessageId: string,
  content: string,
  idempotencyKey: string,
  options: SteerRetryOptions,
): Promise<SteeringCallResult> {
  const once = () => queueSteering(projectId, responseMessageId, content, idempotencyKey, options.client);
  let result = await once();
  for (let retry = 0; ; retry++) {
    if (!result.ok && result.status === undefined) result = await once();
    if (result.ok || result.code !== STEERING_ERROR.rateLimited || result.retryAfterMs === undefined || retry >= STEERING_RETRY_LIMIT) return result;
    options.onWait?.(result.retryAfterMs);
    await options.sleep(result.retryAfterMs);
    result = await once();
  }
}

/** Every steering message of the answer, oldest first, in all states. Throws when the list cannot be read. */
export async function listSteering(projectId: number, responseMessageId: string, client: ApiClient = sessionApiClient()): Promise<SteeringItem[]> {
  if (!validTarget(projectId, responseMessageId)) throw new Error('steering: invalid answer');
  const { data, response } = await client.GET(ROUTE, { params: { path: path(projectId, responseMessageId) } });
  if (!response.ok) throw new Error(`steering list: ${response.status}`);
  return parseSteeringList(data);
}

/** Changes the text of a pending message. 409 `steering_not_pending`: the turn injected it first. */
export function editSteering(
  projectId: number,
  responseMessageId: string,
  steeringId: string,
  content: string,
  client: ApiClient = sessionApiClient(),
): Promise<SteeringCallResult> {
  if (!validTarget(projectId, responseMessageId)) return Promise.resolve({ ok: false, code: 'steering_invalid', status: undefined });
  return call(() =>
    client.PATCH(ITEM_ROUTE, {
      params: { path: { ...path(projectId, responseMessageId), steering_id: steeringId } },
      body: { content },
    }),
  );
}

/** Removes a message (204, also when it is already removed). 409 `steering_not_pending`: it was injected. */
export function removeSteering(
  projectId: number,
  responseMessageId: string,
  steeringId: string,
  client: ApiClient = sessionApiClient(),
): Promise<SteeringCallResult> {
  if (!validTarget(projectId, responseMessageId)) return Promise.resolve({ ok: false, code: 'steering_invalid', status: undefined });
  return call(() => client.DELETE(ITEM_ROUTE, { params: { path: { ...path(projectId, responseMessageId), steering_id: steeringId } } }));
}

/**
 * Takes the caller's `turn_ended` messages of the ended answer (now
 * `handed_off`) and returns them, oldest first. Send ONLY these texts as the
 * next message. An empty list means that the server took them first (it
 * then starts the next turn itself): send nothing. A retry sends the SAME
 * `idempotencyKey` and gets the same messages. Throws when the call fails.
 */
export async function handOffSteering(
  projectId: number,
  responseMessageId: string,
  idempotencyKey: string,
  client: ApiClient = sessionApiClient(),
): Promise<SteeringItem[]> {
  if (!validTarget(projectId, responseMessageId)) throw new Error('steering: invalid answer');
  const { data, response } = await client.POST(HANDOFF_ROUTE, {
    params: { path: path(projectId, responseMessageId), header: { 'Idempotency-Key': idempotencyKey } },
  });
  if (!response.ok) throw new Error(`steering hand-off: ${response.status}`);
  return parseSteeringList(data);
}

/** The waits between the tries of a failed hand-off. */
const HANDOFF_RETRY_MS = [500, 1_500] as const;

/**
 * `handOffSteering`, tried again with the SAME `idempotencyKey` after each
 * failure (at most `HANDOFF_RETRY_MS.length` more times). The key makes sure
 * that a lost answer loses no text: the retry gets the same messages.
 * Throws the last failure.
 */
export async function handOffSteeringWithRetry(
  projectId: number,
  responseMessageId: string,
  idempotencyKey: string,
  options: { client?: ApiClient; sleep: (ms: number) => Promise<void> },
): Promise<SteeringItem[]> {
  for (let attempt = 0; ; attempt++) {
    try {
      return await handOffSteering(projectId, responseMessageId, idempotencyKey, options.client);
    } catch (error) {
      const wait = HANDOFF_RETRY_MS[attempt];
      if (wait === undefined) throw error;
      await options.sleep(wait);
    }
  }
}
