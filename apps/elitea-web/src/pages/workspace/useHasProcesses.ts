/**
 * Whether the folder's session has had a background process (#1223): true
 * once `process_list` returns any or a `process_started` arrives for it, so
 * the Processes tab (and its terminal chunk) loads only when there is
 * something to show.
 */
import { useEffect, useState } from 'react';

import { tauriProcessIpc } from '@/shared/desktop/processIpc';

export function useHasProcesses(workspaceId: string): boolean {
  const [has, setHas] = useState<{ workspaceId: string; has: boolean }>({ workspaceId, has: false });
  useEffect(() => {
    const ipc = tauriProcessIpc();
    if (ipc === undefined) return undefined;
    let live = true;
    let unlisten: () => void = () => undefined;
    const found = (): void => {
      if (live) setHas({ workspaceId, has: true });
    };
    void ipc
      .onEvent((event) => {
        if (event.workspace_id === workspaceId && event.kind === 'process_started') found();
      })
      .then((off) => {
        if (!live) off();
        else unlisten = off;
        return ipc.list(workspaceId);
      })
      .then(
        (listed) => {
          if (listed.length > 0) found();
        },
        () => undefined,
      );
    return () => {
      live = false;
      unlisten();
    };
  }, [workspaceId]);
  return has.workspaceId === workspaceId && has.has;
}
