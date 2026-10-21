import { sessionApiClient, type ApiClient } from '@/api/client';
import { isApiError, parseRetryAfter } from '@/api/errors';

/**
 * The Chats list's actions on one conversation, as requests:
 * - rename: `updateConversation` (PUT with `name` alone; client contract 1.4);
 * - pin / unpin: `pinEntity` / `unpinEntity` (a shared project pin on the conversation; the
 *   list row's `is_pinned` and its delta carry it from 1.4);
 * - delete: `deleteConversation` (204; the other devices get a `deleted` tombstone).
 *
 * Each answer is classified for the action queue (src/sync/conversationOps.ts):
 * `done`, `gone` (404: the conversation no longer exists for this caller, nothing left to
 * do; for a delete that is success), `retry` (no answer, 408, 425, 429, 5xx, or the update
 * screen holds every request), `rejected` (any other refusal: never sent again).
 */

/** The longest title the rename sheet accepts (the web client's limit). */
export const CONVERSATION_NAME_MAX = 256;

export type ConversationAction = { kind: 'rename'; name: string } | { kind: 'pin'; pinned: boolean } | { kind: 'delete' };

export type ActionOutcome = { outcome: 'done' } | { outcome: 'gone' } | { outcome: 'retry'; retryAfterMs?: number } | { outcome: 'rejected'; status?: number };

/**
 * A rename's name as sent, so that the server (client contract 1.4) accepts it: every run of
 * control characters (U+0000-U+001F, U+007F-U+009F, which the server refuses with 400) becomes
 * one space, the result is trimmed, and it is cut to `CONVERSATION_NAME_MAX` code points (the
 * server counts runes, so an emoji is one, and a surrogate pair is never split). Null when blank.
 */
export function cleanConversationName(input: string): string | null {
  const folded = input.replace(/[\u0000-\u001f\u007f-\u009f]+/g, ' ').trim();
  const name = Array.from(folded).slice(0, CONVERSATION_NAME_MAX).join('').trim();
  return name === '' ? null : name;
}

function classifyStatus(response: Response): ActionOutcome {
  if (response.ok) return { outcome: 'done' };
  const status = response.status;
  if (status === 404) return { outcome: 'gone' };
  if (status === 408 || status === 425 || status === 429 || status >= 500) {
    return { outcome: 'retry', retryAfterMs: parseRetryAfter(response.headers.get('Retry-After')) };
  }
  return { outcome: 'rejected', status };
}

function classifyThrown(error: unknown): ActionOutcome {
  if (!isApiError(error)) return { outcome: 'retry' };
  switch (error.kind) {
    case 'http':
      if (error.status === 404) return { outcome: 'gone' };
      if (error.status !== undefined && (error.status === 408 || error.status === 425 || error.status === 429 || error.status >= 500)) {
        return { outcome: 'retry', retryAfterMs: error.retryAfterMs };
      }
      return { outcome: 'rejected', status: error.status };
    case 'origin_mismatch':
      return { outcome: 'rejected' };
    default:
      // network, timeout, aborted, upgrade_required, session_invalid, session_ending: later.
      return { outcome: 'retry', retryAfterMs: error.retryAfterMs };
  }
}

/** Sends one action. Never throws. */
export async function sendConversationAction(
  projectId: number,
  conversationId: number,
  action: ConversationAction,
  client: ApiClient = sessionApiClient(),
): Promise<ActionOutcome> {
  const path = { project_id: String(projectId), conversation_id: String(conversationId) };
  try {
    let response: Response;
    switch (action.kind) {
      case 'rename': {
        const name = cleanConversationName(action.name);
        if (name === null) return { outcome: 'rejected' };
        ({ response } = await client.PUT('/elitea_core/conversation/prompt_lib/{project_id}/{conversation_id}', {
          params: { path },
          body: { name },
        }));
        break;
      }
      case 'pin': {
        const pinPath = { project_id: String(projectId), entity_type: 'conversation', entity_id: conversationId };
        ({ response } = action.pinned
          ? await client.POST('/social/pin/prompt_lib/{project_id}/{entity_type}/{entity_id}', { params: { path: pinPath } })
          : await client.DELETE('/social/pin/prompt_lib/{project_id}/{entity_type}/{entity_id}', { params: { path: pinPath } }));
        break;
      }
      case 'delete':
        ({ response } = await client.DELETE('/elitea_core/conversation/prompt_lib/{project_id}/{conversation_id}', { params: { path } }));
        break;
    }
    const outcome = classifyStatus(response);
    // A delete of a conversation that is already gone did what was asked.
    return action.kind === 'delete' && outcome.outcome === 'gone' ? { outcome: 'done' } : outcome;
  } catch (error) {
    const outcome = classifyThrown(error);
    return action.kind === 'delete' && outcome.outcome === 'gone' ? { outcome: 'done' } : outcome;
  }
}
