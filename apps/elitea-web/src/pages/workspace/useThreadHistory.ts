/**
 * The earlier turns of a reopened thread.
 *
 * First the host's own record (`thread_history`): every turn this machine ran
 * in the thread, with its tool rows, statuses and changed files, replayed
 * through the live turn reducer. When it has none (the thread ran on the web
 * or another machine, or before the host kept history) the conversation's
 * messages are read from the server instead — the server stays the source
 * of truth for content; the local record only adds what the desktop saw.
 *
 * Read once per session mount: the turns this session starts are on screen
 * already, so a re-read would only duplicate them.
 */
import { useQuery } from '@tanstack/react-query';

import { conversationApi } from '@/entities/conversation';
import { unwrapList } from '@/shared/api/unwrap';
import type { StoredTurn, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';

/** The newest server messages a reopened thread shows (the rest are a click away, in chat). */
export const SERVER_MESSAGES = 100;

export interface ServerMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
}

export type ThreadHistory =
  /** A new thread, or nothing earlier anywhere. */
  | { kind: 'none' }
  | { kind: 'loading' }
  | { kind: 'local'; turns: StoredTurn[] }
  | { kind: 'server'; messages: ServerMessage[] }
  /** Neither the host nor the server answered. */
  | { kind: 'unavailable' };

function toMessage(row: Record<string, unknown>): ServerMessage | null {
  const { id, uid, role, content } = row;
  if ((role !== 'user' && role !== 'assistant') || typeof content !== 'string' || content.trim() === '') return null;
  const key = typeof uid === 'string' && uid !== '' ? uid : String(id);
  return { id: key, role, content };
}

async function serverMessages(projectId: number, conversationId: string): Promise<ServerMessage[]> {
  // Newest first (the route's default order), then put back in reading order.
  const payload = await conversationApi.messageList({ projectId, conversationId, page: 0, pageSize: SERVER_MESSAGES });
  return unwrapList<Record<string, unknown>>(payload, 'conversation.messageList')
    .map(toMessage)
    .filter((message): message is ServerMessage => message !== null)
    .reverse();
}

async function readHistory(ipc: WorkspaceIpc, workspaceId: string, projectId: number, conversationId: string): Promise<ThreadHistory> {
  // A host that cannot answer (not signed in, no store) reads as "nothing recorded".
  const local = await ipc.threadHistory(workspaceId, conversationId).catch((): StoredTurn[] => []);
  if (local.length > 0) return { kind: 'local', turns: local };
  try {
    const messages = await serverMessages(projectId, conversationId);
    return messages.length > 0 ? { kind: 'server', messages } : { kind: 'none' };
  } catch {
    return { kind: 'unavailable' };
  }
}

export function useThreadHistory(ipc: WorkspaceIpc, workspaceId: string, projectId: number, conversationId: string): ThreadHistory {
  const enabled = conversationId !== '';
  const query = useQuery({
    queryKey: ['workspace', 'thread-history', workspaceId, projectId, conversationId],
    queryFn: () => readHistory(ipc, workspaceId, projectId, conversationId),
    enabled,
    staleTime: Number.POSITIVE_INFINITY,
    gcTime: 0,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
  if (!enabled) return { kind: 'none' };
  return query.data ?? { kind: 'loading' };
}
