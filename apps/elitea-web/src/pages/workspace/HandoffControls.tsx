/**
 * The thread's cloud handoff on the session page: the badge and the Send to
 * cloud button in the header, the banner above the composer while the
 * thread is not running here, and the dialogs behind them.
 */
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';

import CloudUploadOutlinedIcon from '@mui/icons-material/CloudUploadOutlined';

import { CloudThreadBanner, EraseCloudCopyDialog, ExecutorBadge, SendToCloudDialog, TakeBackDialog } from '@/features/workspace-handoff';
import { t } from '@/shared/i18n';

import type { ThreadHandoff } from './useThreadHandoff';

/** Where the thread runs, and the way to send it. */
export function HandoffHeader({ handoff }: { handoff: ThreadHandoff }): React.JSX.Element | null {
  if (handoff.status === undefined && !handoff.canSend) return null;
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexShrink: 0 }}>
      {handoff.status !== undefined && <ExecutorBadge executor={handoff.status.executor} />}
      {handoff.canSend && (
        <Button size="small" startIcon={<CloudUploadOutlinedIcon fontSize="inherit" />} onClick={() => handoff.open('send')}>
          {t('workspace.handoff.send', 'Send to cloud')}
        </Button>
      )}
    </Box>
  );
}

/** Above the composer, while the thread is not running here. */
export function HandoffBanner({ handoff }: { handoff: ThreadHandoff }): React.JSX.Element | null {
  if (!handoff.offLaptop || handoff.status === undefined) return null;
  return <CloudThreadBanner status={handoff.status} onTakeBack={() => handoff.open('takeBack')} onErase={() => handoff.open('erase')} />;
}

export function HandoffDialogs({ handoff, workspaceId, projectId }: { handoff: ThreadHandoff; workspaceId: string; projectId: number }): React.JSX.Element | null {
  const { ipc, uuid, dialog } = handoff;
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
    case 'takeBack':
      return <TakeBackDialog open ipc={ipc} conversationUuid={uuid} onClose={handoff.close} onSettled={handoff.moved} />;
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
