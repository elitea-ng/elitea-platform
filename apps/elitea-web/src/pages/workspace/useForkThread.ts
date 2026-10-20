/**
 * "Fork" (the thread's ⋯ menu) and "Fork from here" (a message's context
 * menu) of a Local work thread: a new thread in the SAME folder that holds
 * the history up to the fork point.
 *
 * Two halves, in this order. The server forks the conversation
 * (`forkConversation`, client contract 1.11): the messages up to the fork
 * point, the agent and its settings, the folder settings. Then the host
 * copies what only this computer saw — the turns' tool rows, statuses and
 * changed files — into the new thread (`thread_history_fork`, IPC.md
 * "Thread history"). The files in the folder are not rolled back: the fork
 * starts from the folder as it is now, and the notice says so.
 *
 * The fork point of a recorded or live turn is its answer (the last of its
 * `done` event's `message_ids`), and the host copies the turns up to and
 * including that turn. A server message (a thread this computer has no
 * record of) is forked on the server only.
 *
 * If the host copy fails after the server fork, the new thread still opens
 * (its messages are on the server) and the notice says the local history
 * was not copied. The notice is handed to the new thread's session, which
 * mounts fresh after the navigation (`useForkNotice`).
 */
import { useEffect, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';

import { recordThread, threadsQueryKey } from '@/features/workspace';
import { rememberThreadUuid } from '@/features/workspace-handoff';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { forkConversation, getListConversationsQueryKey } from '@/shared/api/generated/chat/chat';
import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

/** Where to fork: nothing is the whole thread. */
export interface ForkPoint {
  /** The server message the fork ends with (a message group UUID or id). */
  messageId: string;
  /** The recorded turn it belongs to: the host copies the turns up to it. Absent for a server-only message. */
  turnId?: string;
}

export interface ForkNotice {
  severity: 'success' | 'warning';
  text: string;
}

/** The notices for threads a fork just opened, by conversation id: read once by the new thread's session. */
const notices = new Map<string, ForkNotice>();

/**
 * The notice of the fork that opened `conversationId`, shown once: read it in a state
 * initializer (pure, so React may call it twice) and forget it after mount.
 */
export function useForkNotice(conversationId: string): [ForkNotice | null, () => void] {
  const [notice, setNotice] = useState(() => notices.get(conversationId) ?? null);
  useEffect(() => {
    notices.delete(conversationId);
  }, [conversationId]);
  return [notice, () => setNotice(null)];
}

export interface ForkThreadInput {
  ipc: WorkspaceIpc;
  workspaceId: string;
  projectId: number;
  /** The thread on screen ('' = a new one, which cannot be forked). */
  conversationId: string;
}

export interface ForkThread {
  /** Fork at `point`, or the whole thread. Never rejects: a failure is `error`. */
  fork: (point?: ForkPoint) => Promise<void>;
  pending: boolean;
  /** Why the last fork did not happen. */
  error: string | null;
  clearError: () => void;
}

function describeForkFailure(error: unknown): string {
  if (error instanceof EliteaApiError && error.failure.kind === 'http' && error.failure.status === 409) {
    return t('workspace.fork.unsettled', 'This thread cannot be forked there yet: a turn is still running or waits for an approval.');
  }
  if (error instanceof EliteaApiError && error.failure.kind === 'http' && error.failure.status === 404) {
    return t('workspace.fork.notFound', 'This thread or message is no longer there.');
  }
  return t('workspace.fork.failed', 'The thread could not be forked. Try again.');
}

interface Created {
  id: string;
  uuid: string;
  name: string;
}

/** The server half: the new conversation, or a throw. */
async function forkOnServer(projectId: number, conversationId: string, point: ForkPoint | undefined): Promise<Created> {
  const response = await forkConversation(String(projectId), conversationId, point === undefined ? {} : { message_id: point.messageId });
  if (response.status !== 201) throw new Error(`the fork answered ${String(response.status)}`);
  const row = response.data;
  if (row.uuid === undefined || row.uuid === '') throw new Error('the fork answered without a uuid');
  return { id: String(row.id), uuid: row.uuid, name: row.name };
}

/** The local half; false when the host could not copy. A server-only message has no recorded turns to copy. */
async function copyLocalTurns(ipc: WorkspaceIpc, workspaceId: string, conversationId: string, created: Created, point: ForkPoint | undefined): Promise<boolean> {
  if (point !== undefined && point.turnId === undefined) return true;
  try {
    await ipc.forkThreadHistory({
      workspaceId,
      sourceConversationId: conversationId,
      targetConversationId: created.id,
      targetConversationUuid: created.uuid,
      ...(point?.turnId === undefined ? {} : { upToTurnId: point.turnId }),
    });
    return true;
  } catch {
    return false;
  }
}

export function useForkThread({ ipc, workspaceId, projectId, conversationId }: ForkThreadInput): ForkThread {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fork = async (point?: ForkPoint): Promise<void> => {
    if (conversationId === '' || pending) return;
    setPending(true);
    setError(null);
    let created: Created;
    try {
      created = await forkOnServer(projectId, conversationId, point);
    } catch (cause: unknown) {
      setError(describeForkFailure(cause));
      setPending(false);
      return;
    }
    const copied = await copyLocalTurns(ipc, workspaceId, conversationId, created, point);
    recordThread(workspaceId, { id: created.id, title: created.name, updatedAt: Date.now() });
    rememberThreadUuid(workspaceId, created.id, created.uuid);
    void queryClient.invalidateQueries({ queryKey: threadsQueryKey(workspaceId) });
    void queryClient.invalidateQueries({ queryKey: getListConversationsQueryKey(String(projectId)).slice(0, 1) });
    notices.set(
      created.id,
      copied
        ? { severity: 'success', text: t('workspace.fork.done', 'Forked into a new thread. Files stay as they are now.') }
        : {
            severity: 'warning',
            text: t('workspace.fork.localFailed', 'Forked into a new thread, but the tool history on this computer was not copied. Files stay as they are now.'),
          },
    );
    setPending(false);
    void navigate({ to: '/workspaces/$workspaceId', params: { workspaceId }, search: { conversation: created.id } });
  };

  return { fork, pending, error, clearError: () => setError(null) };
}

export interface SessionFork extends Pick<ForkThread, 'error' | 'clearError'> {
  /** "Fork from here" on a message; `undefined` while it is not offered (a fork runs, or the thread is in the cloud). */
  onFork: ((point: ForkPoint) => void) | undefined;
  /** ⋯ ▸ Fork Thread; `undefined` while the thread is in the cloud. */
  onForkThread: (() => void) | undefined;
  /** What the fork that opened this thread copied. */
  notice: ForkNotice | null;
  dismissNotice: () => void;
}

/** The thread screen's fork: a thread away from this computer is not forked here (its newest turns are the cloud's). */
export function useSessionFork(input: ForkThreadInput & { offLaptop: boolean }): SessionFork {
  const forking = useForkThread(input);
  const [notice, dismissNotice] = useForkNotice(input.conversationId);
  const offered = !input.offLaptop;
  return {
    onFork: offered && !forking.pending ? (point) => void forking.fork(point) : undefined,
    onForkThread: offered ? () => void forking.fork() : undefined,
    notice,
    dismissNotice,
    error: forking.error,
    clearError: forking.clearError,
  };
}
