/**
 * The index chip of one workspace, opening its settings dialog: what the
 * workspace rows and the session header show. Renders nothing without a
 * host (a plain browser tab). A row only reads the status; the session
 * header (`open`) opens the index, which checks the folder for changes.
 */
import { useState } from 'react';

import type { IndexIpc } from '@/shared/desktop/indexIpc';

import { useIndexIpc } from '../model/ipcContext';
import { useWorkspaceIndex } from '../model/useWorkspaceIndex';
import { IndexSettingsDialog } from './IndexSettingsDialog';
import { IndexStatusChip } from './IndexStatusChip';

export interface WorkspaceIndexControlProps {
  workspaceId: string;
  /** The folder's name, for the dialog's title. */
  name: string;
  /** Open the index (the folder's session page) instead of only reading its status. */
  open?: boolean;
}

function Control({ ipc, workspaceId, name, open: opensIndex = false }: WorkspaceIndexControlProps & { ipc: IndexIpc }): React.JSX.Element {
  const index = useWorkspaceIndex(ipc, workspaceId, { open: opensIndex });
  const [open, setOpen] = useState(false);
  return (
    <>
      <IndexStatusChip view={index.view} loadError={index.loadError} progress={index.progress} onClick={() => setOpen(true)} />
      <IndexSettingsDialog open={open} onClose={() => setOpen(false)} name={name} index={index} />
    </>
  );
}

export function WorkspaceIndexControl({ workspaceId, name, open = false }: WorkspaceIndexControlProps): React.JSX.Element | null {
  const ipc = useIndexIpc();
  if (ipc === undefined) return null;
  return <Control ipc={ipc} workspaceId={workspaceId} name={name} open={open} />;
}
