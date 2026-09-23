/**
 * Hand-written REST layer for the 3 message-scoped endpoints of
 * `chat.api.js` (unit C1) — `messageList`, `deleteMessageFromConversation`,
 * `deleteAllMessagesFromConversation`. Same handwritten-manifest rationale
 * as `./conversationApi.ts`'s module doc (read that first).
 *
 * `messageList`'s baseline (`chat.api.js:24-74`) is an RTK Query
 * infinite-list endpoint with its own `serializeQueryArgs`/`merge`/
 * `forceRefetch` — "load page N, append onto the running list, unless
 * `page` is 0/1 in which case replace it". That merge-into-a-running-array
 * behaviour is page-state orchestration (the chat message LIST a future
 * C2-C6 unit owns), not REST-layer or entity-domain concern; this module
 * exposes the plain, page-scoped fetcher + a `useQuery` wrapper and leaves
 * the pagination-merge orchestration to the caller — same scoping precedent
 * `entities/application-form/model/mutations.ts`'s own doc comment
 * establishes for stripping page/feature orchestration out of an
 * entities-layer port ("page/feature orchestration ... entities/ may not
 * import features/pages").
 */
import { useMutation, useQuery, type UseMutationResult, type UseQueryResult } from '@tanstack/react-query';

import { attachMessageTraces } from './messageTraces';

import { eliteaFetch } from '@/shared/api/generated/mutator';

async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

/* ── messageList — GET elitea_core/messages/prompt_lib/{projectId}/{conversationId} ── */
/* manifest: conversation.messageList */

export interface MessageListParams {
  readonly projectId: string | number;
  readonly conversationId: string | number;
  readonly page: number;
  readonly pageSize?: number;
  readonly params?: Readonly<Record<string, string | number>>;
}

/**
 * Loosely typed — the persisted message-group shape is `entities/message`'s
 * concern (`no-sideways-entities` forbids importing its wire types here).
 *
 * The declared union used to be `readonly unknown[] | {rows, total?}`, copied
 * from `chat.api.js:34-44`'s `transformResponse` branch. It was WRONG: this
 * endpoint answers `{items, total, page, page_size, total_pages}` — measured
 * against the running stack (#132) — and the type being wrong is what made
 * every hand-rolled `'rows' in response ? … : response` at the call sites look
 * exhaustive to the compiler while matching neither arm at runtime. `unknown`
 * is the honest type: callers unwrap it through `unwrapList`/`unwrapListPage`
 * (`@/shared/api/unwrap`, R-A6), which handles items/rows/bare-array and never
 * falls back to the response itself.
 */
export type MessageListResponse = unknown;

function messageListQueryString(params: MessageListParams): string {
  const pageSize = params.pageSize ?? 10;
  const query = new URLSearchParams({ ...params.params, limit: String(pageSize), offset: String(params.page * pageSize) });
  return `?${query.toString()}`;
}

export async function messageList(params: MessageListParams, signal?: AbortSignal): Promise<MessageListResponse> {
  const url = `/elitea_core/messages/prompt_lib/${String(params.projectId)}/${String(params.conversationId)}${messageListQueryString(params)}`;
  const payload = await fetchData<MessageListResponse>(url, signal ? { signal } : {});
  return attachMessageTraces(payload, params.projectId, params.conversationId, signal);
}

export function useMessageListQuery(params: MessageListParams, options: { enabled?: boolean } = {}): UseQueryResult<MessageListResponse> {
  return useQuery({
    queryKey: ['conversation', 'messageList', params.projectId, params.conversationId, params.page, params.pageSize, params.params],
    queryFn: ({ signal }) => messageList(params, signal),
    enabled: options.enabled ?? true,
  });
}

/* ── deleteMessageFromConversation — DELETE elitea_core/message/prompt_lib/{projectId}/{id} ── */
/* manifest: conversation.deleteMessage */

export interface DeleteMessageFromConversationParams {
  readonly projectId: string | number;
  readonly id: string | number;
  /** Not sent — kept only so callers can invalidate the right conversation's cache (baseline: `chat.api.js:83-86`'s `invalidatesTags`). */
  readonly conversationId?: string | number;
}

/**
 * What the server reports it actually removed.
 *
 * One request can delete TWO message groups: the answer named in the URL and
 * the question it replies to, which the server pairs and removes together. The
 * route therefore answers 200 with this body rather than the 204 it used to,
 * because a caller that pruned only the id it asked for would leave the paired
 * question on screen until a reload — worse than not pairing at all.
 *
 * `deleted` holds message-group UUIDs, newest first. It is the SERVER'S list,
 * not a prediction: the pairing rule lives on the server, so a client that
 * re-derived it here would be a second copy to keep in step.
 */
export interface DeleteMessageResponse {
  readonly deleted: readonly string[];
}

/**
 * Deletes one message group and returns every group id that really went.
 *
 * The fallback to `[params.id]` is for a server that still answers 204 (body
 * `undefined`) or that omits the field: the id the caller named was certainly
 * deleted — the request succeeded — so pruning it is right, and the caller gets
 * the old single-message behaviour instead of pruning nothing at all.
 */
export async function deleteMessageFromConversation(params: DeleteMessageFromConversationParams): Promise<DeleteMessageResponse> {
  const body = await fetchData<Partial<DeleteMessageResponse> | undefined>(
    `/elitea_core/message/prompt_lib/${String(params.projectId)}/${String(params.id)}`,
    { method: 'DELETE' },
  );
  const deleted = body?.deleted;
  if (!Array.isArray(deleted) || deleted.length === 0) return { deleted: [String(params.id)] };
  return { deleted: deleted.map(String) };
}

export function useDeleteMessageFromConversationMutation(): UseMutationResult<DeleteMessageResponse, unknown, DeleteMessageFromConversationParams> {
  return useMutation({ mutationFn: deleteMessageFromConversation });
}

/* ── deleteAllMessagesFromConversation — DELETE elitea_core/messages/prompt_lib/{projectId}/{conversationId} ── */
/* manifest: conversation.deleteAllMessages */

export interface DeleteAllMessagesFromConversationParams {
  readonly projectId: string | number;
  readonly conversationId: string | number;
}

export async function deleteAllMessagesFromConversation(params: DeleteAllMessagesFromConversationParams): Promise<unknown> {
  return fetchData<unknown>(`/elitea_core/messages/prompt_lib/${String(params.projectId)}/${String(params.conversationId)}`, { method: 'DELETE' });
}

export function useDeleteAllMessagesFromConversationMutation(): UseMutationResult<unknown, unknown, DeleteAllMessagesFromConversationParams> {
  return useMutation({ mutationFn: deleteAllMessagesFromConversation });
}
