/**
 * A thread's cloud handoff, as the session page needs it: the conversation's
 * UUID (handoff is keyed by it; the page only holds the numeric id), the
 * status (kept current while the thread is away), whether the policy lets
 * this computer send, and which dialog is open. A "take back requested"
 * notification (from the web's Take back button) opens the take back of its
 * thread here.
 */
import { useEffect, useState } from 'react';

import { useQuery, useQueryClient } from '@tanstack/react-query';

import { conversationApi } from '@/entities/conversation';
import { threadsQueryKey } from '@/features/workspace';
import { handoffStatusManyKey, isOffLaptop, rememberThreadUuid, useHandoffIpc, useHandoffStatus, useTakeBackRequests } from '@/features/workspace-handoff';
import type { HandoffIpc, HandoffStatus } from '@/shared/desktop/handoffIpc';

type HandoffDialog = 'send' | 'takeBack' | 'takeBackRest' | 'erase' | 'settings';

export interface ThreadHandoff {
  ipc: HandoffIpc | undefined;
  /** The conversation's UUID; `undefined` for a thread with no server conversation yet. */
  uuid: string | undefined;
  /** `undefined` while reading it, or when the host could not answer. */
  status: HandoffStatus | undefined;
  /** The thread is not running here (in the cloud, or on its way back): read-only, with a banner. */
  offLaptop: boolean;
  /** Offer "Send to cloud": a started thread, running here, under a policy that allows it. */
  canSend: boolean;
  dialog: HandoffDialog | null;
  open: (dialog: HandoffDialog) => void;
  close: () => void;
  /** The thread moved: read its status and history again. */
  moved: () => void;
}

export function useThreadHandoff(workspaceId: string, projectId: number, conversationId: string): ThreadHandoff {
  const ipc = useHandoffIpc();
  const queryClient = useQueryClient();
  const details = conversationApi.useDetails({ projectId, id: conversationId }, { enabled: conversationId !== '' && ipc !== undefined });
  const uuid = details.data?.uuid === undefined || details.data.uuid === '' ? undefined : details.data.uuid;
  const { status, refresh } = useHandoffStatus(ipc, uuid, projectId);
  const allowed = useQuery({ queryKey: ['workspace', 'handoff', 'allowed'], queryFn: () => (ipc as HandoffIpc).allowed(), enabled: ipc !== undefined, retry: false });
  const [dialog, setDialog] = useState<HandoffDialog | null>(null);

  // The thread list remembers the UUID, to badge the thread's row without opening it.
  useEffect(() => {
    if (uuid === undefined || conversationId === '') return;
    rememberThreadUuid(workspaceId, conversationId, uuid);
    void queryClient.invalidateQueries({ queryKey: threadsQueryKey(workspaceId) });
  }, [workspaceId, conversationId, uuid, queryClient]);

  // A web "Take back" asked for this thread.
  const requested = useTakeBackRequests((state) => state.requested);
  useEffect(() => {
    if (uuid === undefined || requested === null) return;
    if (requested !== uuid.toLowerCase() && requested !== conversationId.toLowerCase()) return;
    useTakeBackRequests.getState().clear(requested);
    setDialog('takeBack');
  }, [uuid, conversationId, requested]);

  const offLaptop = isOffLaptop(status);
  return {
    ipc,
    uuid,
    status,
    offLaptop,
    canSend: allowed.data === true && uuid !== undefined && status !== undefined && !offLaptop,
    dialog,
    open: setDialog,
    close: () => setDialog(null),
    moved: () => {
      void refresh();
      // The sidebar's badges read the batch, not this thread's own status.
      void queryClient.invalidateQueries({ queryKey: handoffStatusManyKey() });
      void queryClient.invalidateQueries({ queryKey: ['workspace', 'thread-history'] });
    },
  };
}
