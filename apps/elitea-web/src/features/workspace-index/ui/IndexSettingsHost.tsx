/**
 * The folder's index settings, opened from somewhere other than a chip (the
 * toolbar's ⋯ menu, the Folder menu): the same dialog, controlled by the
 * page. Renders nothing without a host (a plain browser tab).
 */
import type { IndexIpc } from '@/shared/desktop/indexIpc';

import { useIndexIpc } from '../model/ipcContext';
import { useWorkspaceIndex } from '../model/useWorkspaceIndex';
import { IndexSettingsDialog } from './IndexSettingsDialog';

export interface IndexSettingsHostProps {
  workspaceId: string;
  name: string;
  open: boolean;
  onClose: () => void;
}

function Host({ ipc, workspaceId, name, open, onClose }: IndexSettingsHostProps & { ipc: IndexIpc }): React.JSX.Element {
  // The page opens the index (it checks the folder for changes), like the chip it replaces.
  const index = useWorkspaceIndex(ipc, workspaceId, { open: true });
  return <IndexSettingsDialog open={open} onClose={onClose} name={name} index={index} />;
}

export function IndexSettingsHost(props: IndexSettingsHostProps): React.JSX.Element | null {
  const ipc = useIndexIpc();
  if (ipc === undefined) return null;
  return <Host ipc={ipc} {...props} />;
}

/** Whether the host has an index to configure (the Folder menu entry is offered only then). */
export function useHasIndexHost(): boolean {
  return useIndexIpc() !== undefined;
}
