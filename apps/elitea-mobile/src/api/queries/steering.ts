import { randomUUID } from 'expo-crypto';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { ApiError } from '@/api/errors';
import { parseSteeringList, parseSteeringMessage, steeringErrorCode, type SteeringItem } from '@elitea/client-core/stream';

/**
 * Steering of a running answer (client contract 1.26):
 *
 *   POST   /elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering        steerChatExecution
 *   GET    /elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering        listChatExecutionSteering
 *   PATCH  /elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering/{id}   updateChatExecutionSteering
 *   DELETE /elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering/{id}   removeChatExecutionSteering
 *
 * A message sent while the answer runs is queued against it; the worker
 * injects it at the next safe point of the same turn. Each call answers a
 * value: a refusal is `{ ok: false, code }` with the contract's `error`
 * code (`turn_not_running`, `steering_not_supported`, `steering_queue_full`,
 * `steering_not_pending`, ...), so that the caller picks the fallback. A
 * network failure is `{ ok: false, status: undefined }`. A session failure
 * (401, 426) throws: the session layer already reacted to it.
 */

export type SteeringCallResult =
  | { ok: true; item: SteeringItem | undefined }
  | { ok: false; code: string | undefined; status: number | undefined };

const UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

const ROUTE = '/elitea_core/task/prompt_lib/{project_id}/{response_message_id}/steering' as const;
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
    if (error instanceof ApiError && error.kind === 'http') return { ok: false, code: error.code, status: error.status };
    throw error;
  }
  if (result.response.ok) return { ok: true, item: parseSteeringMessage(result.data) };
  return { ok: false, code: steeringErrorCode(result.error), status: result.response.status };
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
