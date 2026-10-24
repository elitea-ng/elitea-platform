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
 * including that turn. The server's answer maps the copied messages
 * (`forked_messages`), and the host rewrites the copied `done` events with
 * it, so a turn of the new thread names the new conversation's messages and
 * a fork of the fork works. A server message (a thread this computer has no
 * record of) is forked on the server only.
 *
 * If the host copy fails after the server fork, the new thread still opens
 * (its messages are on the server) and the notice says the local history
 * was not copied. The notice is handed to the new thread's session, which
 * mounts fresh after the navigation (`useForkNotice`). When the fork point is
 * a recorded turn, the notice offers to restore the folder's files to that
 * point (`ForkRestoreFiles`): a dry run first, then only on confirmation.
 *
 * "Rewind to here" (issue 1234) is the same fork with `mode: 'rewind'`: the
 * server records a summary of the abandoned turns on the new conversation,
 * which the agent's next turn sees after the fork point. The new thread
 * takes the source's place in the folder's list, and the source is listed
 * under it as the earlier attempt; nothing is deleted.
 */
import { useEffect, useRef, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';

import { recordThread, threadsQueryKey } from '@/features/workspace';
import { rememberThreadUuid } from '@/features/workspace-handoff';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { forkConversation, getListConversationsQueryKey } from '@/shared/api/generated/chat/chat';
import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

/** A plain fork, or a rewind (the abandoned turns are summarised on the new conversation). */
export type ForkMode = 'fork' | 'rewind';

/** Where to fork: nothing is the whole thread. */
export interface ForkPoint {
  /** The server message the fork ends with (a message group UUID or id). */
  messageId: string;
  /** The recorded turn it belongs to: the host copies the turns up to it. Absent for a server-only message. */
  turnId?: string;
}

interface ForkNotice {
  severity: 'success' | 'warning';
  text: string;
  /** The recorded fork point turn: the notice offers to restore the folder's files to it. */
  restoreTo?: string;
}

/** The notices for threads a fork just opened, by conversation id: read once by the new thread's session. */
const notices = new Map<string, ForkNotice>();

/**
 * The notice of the fork that opened `conversationId`, shown once: read it in a state
 * initializer (pure, so React may call it twice) and forget it after mount.
 */
function useForkNotice(conversationId: string): [ForkNotice | null, () => void] {
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

interface ForkThread {
  /** Fork (or rewind) at `point`, or fork the whole thread. Never rejects: a failure is `error`. */
  fork: (point?: ForkPoint, mode?: ForkMode) => Promise<void>;
  pending: boolean;
  /** Why the last fork did not happen. */
  error: string | null;
  clearError: () => void;
}

/** The refusal a failed fork answered: its status and the server's `error` code. */
function forkRefusal(error: unknown): { status: number; code: unknown } | undefined {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return undefined;
  return { status: error.failure.status, code: (error.failure.body as { error?: unknown } | undefined)?.error };
}

function describeForkFailure(error: unknown): string {
  const refusal = forkRefusal(error);
  if (refusal?.status === 409 && refusal.code === 'thread_executor_is_cloud') {
    return t('workspace.fork.inCloud', 'This thread is running in the cloud. Take it back to this computer, then fork it.');
  }
  switch (refusal?.status ?? 0) {
    case 413:
      return t('workspace.fork.tooLarge', 'This thread is too long to fork there. Fork from an earlier message.');
    case 409:
      return t('workspace.fork.unsettled', 'This thread cannot be forked there yet: a turn is still running or waits for an approval.');
    case 404:
      return t('workspace.fork.notFound', 'This thread or message is no longer there.');
    default:
      return t('workspace.fork.failed', 'The thread could not be forked. Try again.');
  }
}

interface Created {
  id: string;
  uuid: string;
  name: string;
  /** Source message UUID → the copy's (`forked_messages`). */
  messageIds: Readonly<Record<string, string>>;
}

/**
 * The server half: the new conversation, or a throw. `forkId` makes a resent request answer the same fork: the
 * caller keeps one per fork the person asked for, so trying again after a lost answer makes no second fork.
 */
async function forkOnServer(projectId: number, conversationId: string, point: ForkPoint | undefined, mode: ForkMode, forkId: string): Promise<Created> {
  const body = point === undefined ? { fork_id: forkId } : { message_id: point.messageId, ...(mode === 'rewind' ? { mode } : {}), fork_id: forkId };
  const response = await forkConversation(String(projectId), conversationId, body);
  if (response.status !== 201) throw new Error(`the fork answered ${String(response.status)}`);
  const row = response.data;
  if (row.uuid === undefined || row.uuid === '') throw new Error('the fork answered without a uuid');
  return { id: String(row.id), uuid: row.uuid, name: row.name, messageIds: row.forked_messages ?? {} };
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
      messageIds: created.messageIds,
    });
    return true;
  } catch {
    return false;
  }
}

/** The fork_id of the fork asked for: the one held while that same fork is asked for again, else a new one. */
function intendedForkId(
  intent: { current: { key: string; forkId: string } | null },
  [projectId, conversationId, point, mode]: [number, string, ForkPoint | undefined, ForkMode],
): string {
  const key = JSON.stringify([projectId, conversationId, point?.messageId ?? null, mode]);
  if (intent.current?.key !== key) intent.current = { key, forkId: crypto.randomUUID() };
  return intent.current.forkId;
}

function useForkThread({ ipc, workspaceId, projectId, conversationId }: ForkThreadInput): ForkThread {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The fork_id of the fork asked for last, until it succeeds: asking again for the same one (after a failure,
  // which may have been a lost answer) resends it; another fork point, mode or thread gets a new one.
  const intent = useRef<{ key: string; forkId: string } | null>(null);

  const fork = async (point?: ForkPoint, mode: ForkMode = 'fork'): Promise<void> => {
    if (conversationId === '' || pending) return;
    setPending(true);
    setError(null);
    const forkId = intendedForkId(intent, [projectId, conversationId, point, mode]);
    let created: Created;
    try {
      created = await forkOnServer(projectId, conversationId, point, mode, forkId);
      intent.current = null;
    } catch (cause: unknown) {
      setError(describeForkFailure(cause));
      setPending(false);
      return;
    }
    const copied = await copyLocalTurns(ipc, workspaceId, conversationId, created, point);
    recordThread(workspaceId, { id: created.id, title: created.name, updatedAt: Date.now() }, mode === 'rewind' && point !== undefined ? conversationId : undefined);
    rememberThreadUuid(workspaceId, created.id, created.uuid);
    void queryClient.invalidateQueries({ queryKey: threadsQueryKey(workspaceId) });
    void queryClient.invalidateQueries({ queryKey: getListConversationsQueryKey(String(projectId)).slice(0, 1) });
    const done =
      mode === 'rewind'
        ? t('workspace.fork.rewound', 'Rewound: this thread goes on from that turn, and the earlier attempt is listed under it. Files stay as they are now.')
        : t('workspace.fork.done', 'Forked into a new thread. Files stay as they are now.');
    notices.set(created.id, {
      ...(copied
        ? { severity: 'success', text: done }
        : {
            severity: 'warning',
            text: t('workspace.fork.localFailed', 'Forked into a new thread, but the tool history on this computer was not copied. Files stay as they are now.'),
          }),
      ...(point?.turnId === undefined ? {} : { restoreTo: point.turnId }),
    });
    setPending(false);
    void navigate({ to: '/workspaces/$workspaceId', params: { workspaceId }, search: { conversation: created.id } });
  };

  return { fork, pending, error, clearError: () => setError(null) };
}

export interface SessionFork extends Pick<ForkThread, 'error' | 'clearError'> {
  /** "Fork from here" / "Rewind to here" on a message; `undefined` while it is not offered (a fork runs, or the thread is in the cloud). */
  onFork: ((point: ForkPoint, mode: ForkMode) => void) | undefined;
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
    onFork: offered && !forking.pending ? (point, mode) => void forking.fork(point, mode) : undefined,
    onForkThread: offered ? () => void forking.fork() : undefined,
    notice,
    dismissNotice,
    error: forking.error,
    clearError: forking.clearError,
  };
}
