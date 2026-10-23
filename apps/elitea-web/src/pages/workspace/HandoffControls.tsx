/**
 * The thread's cloud handoff on the session page: the banner above the
 * composer while the thread is not running here, and the dialogs behind it
 * and behind the composer footer's run-location menu (`ComposerControls`).
 */


import { ChangedFilesCard, useWorkspaceIpc } from '@/features/workspace';
import { CloudThreadBanner, EraseCloudCopyDialog, HandoffSettingsDialog, SendToCloudDialog, TakeBackDialog } from '@/features/workspace-handoff';

import type { ThreadHandoff } from './useThreadHandoff';

/** Above the composer, while the thread is not running here. */
export function HandoffBanner({ handoff }: { handoff: ThreadHandoff }): React.JSX.Element | null {
  if (!handoff.offLaptop || handoff.status === undefined) return null;
  return <CloudThreadBanner status={handoff.status} onTakeBack={() => handoff.open('takeBack')} onErase={() => handoff.open('erase')} />;
}

export function HandoffDialogs({ handoff, workspaceId, projectId }: { handoff: ThreadHandoff; workspaceId: string; projectId: number }): React.JSX.Element | null {
  const { ipc, uuid, dialog } = handoff;
  const workspaceIpc = useWorkspaceIpc();
  if (ipc === undefined || uuid === undefined || dialog === null) return null;
  switch (dialog) {
    case 'send':
      return (
        <SendToCloudDialog
          open
          ipc={ipc}
          workspaceId={workspaceId}
          conversationUuid={uuid}
          onClose={handoff.close}
          onSent={() => {
            handoff.moved();
            handoff.close();
          }}
        />
      );
    case 'settings':
      return <HandoffSettingsDialog open ipc={ipc} workspaceId={workspaceId} onClose={handoff.close} />;
    case 'takeBack':
      return (
        <TakeBackDialog
          open
          ipc={ipc}
          conversationUuid={uuid}
          onClose={handoff.close}
          onSettled={handoff.moved}
          // The diffs of what the take back writes, read-only like a recorded turn's.
          renderFiles={workspaceIpc === undefined ? undefined : (files, key) => <ChangedFilesCard ipc={workspaceIpc} turnId={key} files={files} />}
        />
      );
    case 'erase':
      return (
        <EraseCloudCopyDialog
          open
          ipc={ipc}
          conversationUuid={uuid}
          projectId={projectId}
          onClose={handoff.close}
          onErased={() => {
            handoff.moved();
            handoff.close();
          }}
        />
      );
  }
}
