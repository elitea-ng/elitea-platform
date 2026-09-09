/**
 * Message like/dislike + optional comment (#880).
 *
 * This route IS described in `api/openapi/v2.yaml`
 * (`getMessageFeedback`/`setMessageFeedback`/`deleteMessageFeedback`), so
 * the URL is built with the ORVAL-GENERATED url builders
 * (`shared/api/generated/chat/chat.ts`'s `getGetMessageFeedbackUrl`/
 * `getSetMessageFeedbackUrl`/`getDeleteMessageFeedbackUrl`) rather than
 * hand-derived — the schema is the source of truth for the PATH, and
 * duplicating it here would be exactly the kind of drift
 * `scripts/check-contract-coverage.mjs` exists to catch.
 *
 * The generated per-operation ASYNC FUNCTIONS (`getMessageFeedback`/
 * `setMessageFeedback`/`deleteMessageFeedback` in chat.ts) are deliberately
 * NOT called here: orval types each one's return as the response UNION
 * across every documented status code (200 success, 400/401/403/404/500
 * error), because `override.fetch.includeHttpResponseReturnType` has no way
 * to know `eliteaFetch` already turned every non-2xx into a thrown
 * `EliteaApiError` — so a caller reading `.data` off that union sees
 * `ErrorResponse`'s shape unioned in too, and TypeScript correctly refuses
 * to narrow it (there is nothing in the STATIC type to narrow on). Calling
 * `eliteaFetch` directly, typed at the real success shape only, is what the
 * throw-on-error contract actually supports — `getGetMessageFeedbackUrl`
 * etc. are reused for the path so this is not a second URL to keep in sync,
 * only a narrower response type than the generated function declares.
 *
 * The generated file ALSO exports `useGetMessageFeedback`/
 * `useSetMessageFeedback`/`useDeleteMessageFeedback`, but every one of them
 * is a `useQuery` wrapper — `orval.config.ts` sets `query.useQuery: true`
 * globally, with no per-operation mutation override, so a POST/DELETE
 * operation gets a query hook too. That is the wrong shape for a write (no
 * `mutate`/`isPending`, and it would refetch-on-mount rather than fire on
 * demand), so — matching `conversationApi.ts`'s own established pattern —
 * the write operations here get a HAND-WRITTEN `useMutation`.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from '@tanstack/react-query';

import { getDeleteMessageFeedbackUrl, getGetMessageFeedbackUrl, getSetMessageFeedbackUrl } from '@/shared/api/generated/chat/chat';
import type { MessageFeedbackSummary } from '@/shared/api/generated/model';
import { eliteaFetch } from '@/shared/api/generated/mutator';

export interface MessageFeedbackTarget {
  readonly projectId: string | number;
  readonly messageId: string;
}

export interface SetMessageFeedbackParams extends MessageFeedbackTarget {
  /** `1` = like, `-1` = dislike. */
  readonly rating: 1 | -1;
  readonly comment?: string;
}

/** The query key every feedback read/write for one message shares — a write's `onSuccess` seeds this directly rather than invalidating and re-fetching. */
export function messageFeedbackQueryKey(target: MessageFeedbackTarget): readonly unknown[] {
  return ['message-feedback', String(target.projectId), target.messageId] as const;
}

/** `eliteaFetch` resolves the ENVELOPE (`{data, status, headers}`), never the bare body — see this module's doc comment. Read once, here, not at every call site (the #132 shape). */
async function unwrap(url: string, options?: RequestInit): Promise<MessageFeedbackSummary> {
  const envelope = await eliteaFetch<{ data: MessageFeedbackSummary }>(url, options);
  return envelope.data;
}

export async function getMessageFeedback(target: MessageFeedbackTarget): Promise<MessageFeedbackSummary> {
  return unwrap(getGetMessageFeedbackUrl(String(target.projectId), target.messageId));
}

export async function setMessageFeedback(params: SetMessageFeedbackParams): Promise<MessageFeedbackSummary> {
  const { projectId, messageId, rating, comment } = params;
  return unwrap(getSetMessageFeedbackUrl(String(projectId), messageId), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ rating, comment }),
  });
}

export async function deleteMessageFeedback(target: MessageFeedbackTarget): Promise<MessageFeedbackSummary> {
  return unwrap(getDeleteMessageFeedbackUrl(String(target.projectId), target.messageId), { method: 'DELETE' });
}

/**
 * Reads one message's aggregate + the caller's own vote. `enabled` is false
 * whenever `messageId` is not yet known (a streaming answer has no stable
 * id until it settles) — same guard shape `useConversationDetailsQuery`
 * (`conversationApi.ts`) uses for an absent id.
 */
export function useMessageFeedbackQuery(
  target: MessageFeedbackTarget | undefined,
): UseQueryResult<MessageFeedbackSummary> {
  return useQuery({
    queryKey: target ? messageFeedbackQueryKey(target) : ['message-feedback', 'unset'],
    queryFn: () => getMessageFeedback(target as MessageFeedbackTarget),
    enabled: target !== undefined && target.messageId !== '',
  });
}

export function useSetMessageFeedbackMutation(): UseMutationResult<
  MessageFeedbackSummary,
  unknown,
  SetMessageFeedbackParams
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: setMessageFeedback,
    onSuccess: (summary, variables) => {
      queryClient.setQueryData(messageFeedbackQueryKey(variables), summary);
    },
  });
}

export function useDeleteMessageFeedbackMutation(): UseMutationResult<
  MessageFeedbackSummary,
  unknown,
  MessageFeedbackTarget
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: deleteMessageFeedback,
    onSuccess: (summary, variables) => {
      queryClient.setQueryData(messageFeedbackQueryKey(variables), summary);
    },
  });
}
