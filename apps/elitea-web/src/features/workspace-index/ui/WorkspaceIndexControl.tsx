/**
 * The index chip of one workspace, opening its settings dialog: what the
 * workspace rows and the session header show. Renders nothing without a
 * host (a plain browser tab).
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
}

function Control({ ipc, workspaceId, name }: WorkspaceIndexControlProps & { ipc: IndexIpc }): React.JSX.Element {
  const index = useWorkspaceIndex(ipc, workspaceId);
  const [open, setOpen] = useState(false);
  return (
    <>
      <IndexStatusChip view={index.view} loadError={index.loadError} progress={index.progress} onClick={() => setOpen(true)} />
      <IndexSettingsDialog open={open} onClose={() => setOpen(false)} name={name} index={index} />
    </>
  );
}

export function WorkspaceIndexControl({ workspaceId, name }: WorkspaceIndexControlProps): React.JSX.Element | null {
  const ipc = useIndexIpc();
  if (ipc === undefined) return null;
  return <Control ipc={ipc} workspaceId={workspaceId} name={name} />;
}
