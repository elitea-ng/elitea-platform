import { useQuery } from '@tanstack/react-query';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { queryKeys } from '@/api/queries/keys';
import { rememberConversation } from '@/sync/appSync';
import { useDeviceCopy } from '@/api/queries/deviceCopy';
import { offlineConversation, readWithFallback } from '@/sync/offlineReads';

export type ConversationDetail = components['schemas']['ConversationDetail'];

/** How many of the latest message groups the thread loads. */
export const THREAD_MESSAGES_LIMIT = 50;

/** A conversation id from a route: a positive integer id or a uuid. */
export function isConversationRef(value: unknown): value is string {
  return (
    typeof value === 'string' &&
    (/^[1-9]\d{0,15}$/.test(value) || /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/.test(value))
  );
}

/**
 * One conversation with its latest message groups (newest first from the
 * server). A network read is kept in the offline store (when open) and its
 * transcript synced; when the workspace does not answer, the stored copy is
 * returned instead (`isOfflineCopy`).
 */
export async function fetchConversation(
  projectId: number,
  conversationRef: string,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<ConversationDetail> {
  if (!isConversationRef(conversationRef)) throw new Error('invalid conversation id');
  let fromNetwork = false;
  const detail = await readWithFallback<ConversationDetail>(
    async (deadline) => {
      const read = await unwrap(
        client.GET('/elitea_core/conversation/prompt_lib/{project_id}/{conversation_id}', {
          params: {
            path: { project_id: String(projectId), conversation_id: conversationRef },
            query: { messages_limit: THREAD_MESSAGES_LIMIT, sort_order: 'desc' },
          },
          signal: deadline,
        }),
      );
      fromNetwork = true;
      return read;
    },
    () => offlineConversation(projectId, conversationRef),
    { signal },
  );
  if (fromNetwork) rememberConversation(projectId, detail);
  return detail;
}

export function hasStreamingGroup(detail: ConversationDetail | undefined): boolean {
  return (detail?.message_groups ?? []).some((g) => g?.is_streaming === true);
}

/** While a turn is still being written server-side, poll every 3 s until it settles. */
export const STREAMING_POLL_MS = 3000;

/**
 * How long a conversation with several agents is still read after its last answer settled
 * (ADR-0050 7c/7d): Main starts the next addressed or handed agent's turn itself and sends no
 * frame for it, so the thread reads it from the conversation.
 */
export const FLOOR_FOLLOW_MS = 20_000;

function agentCount(detail: ConversationDetail | undefined): number {
  return (detail?.participants ?? []).filter((p) => p?.entity_name === 'application').length;
}

function newestGroupAt(detail: ConversationDetail | undefined): number {
  let newest = 0;
  for (const group of detail?.message_groups ?? []) {
    const at = Date.parse(String((group as { created_at?: unknown } | null)?.created_at ?? ''));
    if (Number.isFinite(at) && at > newest) newest = at;
  }
  return newest;
}

/** The poll interval of a conversation read: while a turn streams, and briefly after one settles with several agents. */
export function conversationPollInterval(detail: ConversationDetail | undefined, now: number = Date.now()): number | false {
  if (hasStreamingGroup(detail)) return STREAMING_POLL_MS;
  if (agentCount(detail) < 2) return false;
  const newest = newestGroupAt(detail);
  return newest > 0 && now - newest < FLOOR_FOLLOW_MS ? STREAMING_POLL_MS : false;
}

export function useConversation(projectId: number | null, conversationRef: string, options: { pollWhileStreaming?: boolean } = {}) {
  const poll = options.pollWhileStreaming ?? true;
  const enabled = projectId !== null && isConversationRef(conversationRef);
  // The saved copy shows at once while the workspace is asked (DEF-R5). A
  // shallow copy, so it is not flagged as an offline answer.
  const saved = useDeviceCopy(queryKeys.conversation(projectId ?? 0, conversationRef), enabled, () =>
    offlineConversation(projectId as number, conversationRef),
  );
  return useQuery({
    queryKey: queryKeys.conversation(projectId ?? 0, conversationRef),
    enabled,
    placeholderData: saved === null ? undefined : { ...saved },
    queryFn: ({ signal }) => fetchConversation(projectId as number, conversationRef, sessionApiClient(), signal),
    refetchInterval: (query) => (poll ? conversationPollInterval(query.state.data) : false),
  });
}
