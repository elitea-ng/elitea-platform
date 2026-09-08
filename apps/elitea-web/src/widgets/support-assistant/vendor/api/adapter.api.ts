/**
 * The support assistant's API adapter.
 *
 * Ported from `@eliteaai/elitea-assistant`'s `src/api/adapter.api.ts`, which is a
 * bare `fetch` wrapper carrying its own `Authorization` header and
 * `credentials: 'include'` — the shape an EMBEDDABLE widget needs, because it
 * cannot assume anything about the host application's HTTP layer.
 *
 * Inside this app that assumption is available, so every JSON call goes through
 * `eliteaFetch` instead: one place that resolves the API origin, attaches the
 * session, unwraps the envelope, and classifies failures. Re-implementing any of
 * that here is how a widget acquires a second, subtly different notion of "am I
 * signed in" from the app it is mounted in.
 *
 * `uploadAttachment` (issue #625 item 2): the platform's start contract now
 * DOES carry an attachment to the agent (#606 added
 * `CurrentApplicationStartRequest.Attachments`, and `Predict` — internal/api/
 * v2/supportassistant/predict.go — parses `attachments` into it the same
 * way the main chat composer's `payload.attachments` does), so the upload
 * this file adds is not the dead end an earlier version of this comment
 * described. It goes through `eliteaFetch` with a `FormData` body — the
 * SAME established pattern `features/skills/api/skillIconApi.ts`'s
 * `uploadSkillIcon` and `http.ts`'s own doc comment cite — needing no
 * `XMLHttpRequest`: R-A4's "XHR lives only in shared/api/upload.ts" still
 * holds without an exception being carved for this widget.
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';

import type {
  TChatAPI,
  TConversationListItem,
  TConversationsResponse,
  TRawConversation,
} from '../lib/types';

/** The subrouter this widget talks to, relative to the app's API base. */
const BASE = '/support_assistant';

/**
 * `eliteaFetch` RESOLVES TO THE ENVELOPE, NOT THE BODY.
 *
 * Its return is `{ data, status, headers } as T` — the `as T` is a cast, so
 * `eliteaFetch<TAssistantConfig>(…)` type-checks perfectly and hands back an
 * object whose `enabled` is `undefined`, with a 200 and nothing in the console.
 * That is not hypothetical here: it is exactly the defect
 * `../../api/supportAssistantConfigApi.test.tsx` was written to pin, on this
 * very endpoint, and the same shape as issue #132.
 *
 * Every call in this file therefore goes through `unwrap()`, which names the
 * envelope in the type and returns `.data`. Adding a call that uses
 * `eliteaFetch` directly reintroduces the bug silently.
 */
async function unwrap<T>(url: string, options?: RequestInit, transport?: { readonly background?: boolean }): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options ?? {}, transport ?? {});
  return envelope.data;
}

/**
 * `startTurn`'s answer — the agent-execution start body, of which this widget
 * reads one field.
 */
export interface TSupportTurnStarted {
  readonly events_url?: string;
  readonly execution_id?: string;
  readonly task_id?: string;
}

/** One file already uploaded via `uploadAttachment`, ready to ride a turn. */
export interface TSupportAttachmentRef {
  /** Exactly what `uploadAttachment` answered with — `/{bucket}/{conversationUuid}/{name}`. */
  readonly filepath: string;
  /** Display name only; the object key IS `filepath`, not this. */
  readonly name: string;
}

/** One question, with the page context that was collected alongside it. */
export interface TSupportTurnRequest {
  readonly content: string;
  readonly question_id: string;
  readonly support_assistant_context?: Record<string, unknown> | undefined;
  readonly attachments?: readonly TSupportAttachmentRef[] | undefined;
}

/** `uploadAttachment`'s answer — one array entry, matching the main chat composer's own upload response shape. */
export interface TSupportAttachmentUploaded {
  readonly filepath: string;
  readonly file_size: number;
}

/** The adapter, widened with the three calls the socket transport used to cover. */
export type TSupportApi = TChatAPI & {
  startTurn: (conversationUuid: string, request: TSupportTurnRequest) => Promise<TSupportTurnStarted>;
  uploadAttachment: (conversationUuid: string, file: File) => Promise<TSupportAttachmentUploaded>;
};

export const createSupportApi = (): TSupportApi => ({

  getConversations: () => unwrap<TConversationsResponse>(`${BASE}/conversations/`),

  getConversation: (conversationId: string) =>
    unwrap<TRawConversation>(`${BASE}/conversation/${encodeURIComponent(conversationId)}`),

  createConversation: () =>
    unwrap<TConversationListItem>(`${BASE}/conversations/`, {
      method: 'POST',
      body: JSON.stringify({}),
      headers: { 'Content-Type': 'application/json' },
    }),



  /**
   * Start one turn. This is the REST call that replaces the widget's
   * `support_predict` socket emit; the answer names the SSE stream the frames
   * arrive on (`vendor/lib/hooks/stream.hook.ts`).
   */
  startTurn: (conversationUuid: string, request: TSupportTurnRequest) =>
    unwrap<TSupportTurnStarted>(`${BASE}/predict/${encodeURIComponent(conversationUuid)}`, {
      method: 'POST',
      body: JSON.stringify(request),
      headers: { 'Content-Type': 'application/json' },
    }),

  /**
   * Stores one file for a support conversation, through the SAME artifact
   * path the main chat composer's own upload uses server-side
   * (internal/api/v2/supportassistant/attachments.go). No `Content-Type`
   * header: `fetch` sets its own multipart boundary for a `FormData` body,
   * and setting one here would be the wrong one.
   *
   * The route answers an ARRAY of one entry (`attachmentCreated[]`,
   * matching the main chat route's own response shape) — `unwrap` returns
   * it as-is, and the first (only) entry is what the caller needs.
   */
  uploadAttachment: async (conversationUuid: string, file: File): Promise<TSupportAttachmentUploaded> => {
    const form = new FormData();
    form.append('file', file);
    const created = await unwrap<readonly TSupportAttachmentUploaded[]>(
      `${BASE}/attachments/${encodeURIComponent(conversationUuid)}`,
      { method: 'POST', body: form },
    );
    const [first] = created;
    if (!first) throw new Error('the upload answered with no file');
    return first;
  },

});
