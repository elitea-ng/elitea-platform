/**
 * A local thread's name. A new thread is called after its first message;
 * once its first turn is committed, the server names the conversation from
 * that exchange (`generateConversationName`: the project's default model,
 * billed to the person), and the thread takes the name. The person can
 * rename a thread (the conversation's own rename, `updateConversation`), and
 * a name changed elsewhere (the web chat, another device) is read back when
 * the thread opens. A thread whose title is the conversation's own name is
 * `named`: it is not generated again.
 */
import { useCallback, useEffect, useRef } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { generateConversationName, getConversation, updateConversation } from '@/shared/api/generated/chat/chat';

import { readThreads, renameThread, threadsQueryKey } from './threads';

export interface ThreadRef {
  workspaceId: string;
  projectId: number;
  /** The conversation id ('' for a new thread, which has none yet). */
  conversationId: string;
}

const nameOf = (data: unknown): string | undefined => {
  const name = (data as { name?: unknown } | undefined)?.name;
  return typeof name === 'string' && name.trim() !== '' ? name : undefined;
};

/** Keeps the thread's title and invalidates the sidebar's read of it. */
function useStoreName(): (ref: ThreadRef, name: string) => void {
  const queryClient = useQueryClient();
  return useCallback(
    (ref: ThreadRef, name: string) => {
      renameThread(ref.workspaceId, ref.conversationId, name);
      void queryClient.invalidateQueries({ queryKey: threadsQueryKey(ref.workspaceId) });
    },
    [queryClient],
  );
}

/**
 * Once `committed` (the thread's turn is stored on the server), asks the server to name a thread that is not named
 * yet; at most once per thread and visit. A failure leaves the first-message title (the next visit asks again).
 */
export function useThreadNaming(ref: ThreadRef, committed: boolean): void {
  const store = useStoreName();
  const asked = useRef(new Set<string>());
  const { workspaceId, projectId, conversationId } = ref;
  useEffect(() => {
    if (!committed || conversationId === '' || asked.current.has(conversationId)) return;
    const thread = readThreads(workspaceId).find((candidate) => candidate.id === conversationId);
    if (thread === undefined || thread.named === true) return;
    asked.current.add(conversationId);
    generateConversationName(String(projectId), conversationId, { expected_name: thread.title })
      .then((response) => {
        const name = nameOf(response.data);
        if (name !== undefined) store({ workspaceId, projectId, conversationId }, name);
      })
      .catch(() => undefined);
  }, [committed, workspaceId, projectId, conversationId, store]);
}

/** Reads the conversation's name when the thread opens, and keeps it as the thread's title when it changed elsewhere. */
export function useServerThreadName(ref: ThreadRef): void {
  const store = useStoreName();
  const { workspaceId, projectId, conversationId } = ref;
  const read = useQuery({
    queryKey: ['workspace', 'threadName', projectId, conversationId] as const,
    queryFn: async () => nameOf((await getConversation(String(projectId), conversationId)).data) ?? null,
    enabled: conversationId !== '',
    staleTime: 60_000,
    retry: false,
  });
  const name = read.data;
  useEffect(() => {
    if (name === undefined || name === null) return;
    const thread = readThreads(workspaceId).find((candidate) => candidate.id === conversationId);
    // The same name: nothing changed elsewhere (a thread still named after its first message waits for useThreadNaming).
    if (thread === undefined || thread.title === name) return;
    store({ workspaceId, projectId, conversationId }, name);
  }, [name, workspaceId, projectId, conversationId, store]);
}

/** Renames a thread: the conversation on the server, then the thread here. */
export function useRenameThread() {
  const store = useStoreName();
  return useMutation({
    mutationFn: async ({ ref, name }: { ref: ThreadRef; name: string }) => {
      await updateConversation(String(ref.projectId), ref.conversationId, { name });
      return { ref, name };
    },
    onSuccess: ({ ref, name }) => store(ref, name),
  });
}
