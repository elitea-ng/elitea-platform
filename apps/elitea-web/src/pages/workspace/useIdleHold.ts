/**
 * Keeps the host from compacting a thread in the background while its
 * composer has unsent text (`compaction_idle_hold`): held when the draft
 * becomes non-empty, released when it empties (a send clears it), when the
 * thread changes and on unmount. Only the transitions reach the host. The
 * first local composer also moves the old idle switch to the host
 * (`migrateIdleSwitch`).
 */
import { useCallback, useEffect, useRef } from 'react';

import { compactionIpc, migrateIdleSwitch } from '@/shared/desktop/compactionSettings';

type Held = { workspaceId: string; conversationId: string };

function sendHold(thread: Held, hold: boolean): void {
  void compactionIpc()
    ?.hold(thread.workspaceId, thread.conversationId, hold)
    .catch(() => undefined);
}

/** The composer's `onDraftChange` for a thread; `local`: the thread runs here (no hold for the cloud copy or a new thread). */
export function useIdleHold(workspaceId: string, conversationId: string, local: boolean): (hasDraft: boolean) => void {
  const held = useRef<Held | null>(null);
  const release = useCallback(() => {
    const thread = held.current;
    if (thread === null) return;
    held.current = null;
    sendHold(thread, false);
  }, []);
  useEffect(() => {
    const ipc = compactionIpc();
    if (ipc !== undefined) void migrateIdleSwitch(ipc);
  }, []);
  // Another thread (or no longer here): the old hold goes; the composer reports its draft again.
  useEffect(() => release, [workspaceId, conversationId, local, release]);
  return useCallback(
    (hasDraft: boolean) => {
      if (!hasDraft) {
        release();
        return;
      }
      if (!local || conversationId === '' || held.current !== null) return;
      held.current = { workspaceId, conversationId };
      sendHold(held.current, true);
    },
    [workspaceId, conversationId, local, release],
  );
}
