/**
 * The folder's index settings as the inspector's Index pane: the settings the
 * chip's dialog shows, in place. The page opens the index (it checks the
 * folder for changes), like the chip it replaces. Renders nothing without a
 * host (a plain browser tab).
 */
import type { IndexIpc } from '@/shared/desktop/indexIpc';

import { useIndexIpc } from '../model/ipcContext';
import { useEmbeddings } from '../model/useEmbeddings';
import { useWorkspaceIndex } from '../model/useWorkspaceIndex';
import { IndexSettingsPanel } from './IndexSettingsDialog';

function Host({ ipc, workspaceId }: { ipc: IndexIpc; workspaceId: string }): React.JSX.Element {
  const index = useWorkspaceIndex(ipc, workspaceId, { open: true });
  // The pane is on screen whenever it is mounted: read while the index is on.
  const on = index.view?.kind === 'status' && index.view.status.state !== 'off';
  const embeddings = useEmbeddings(ipc, workspaceId, on);
  return <IndexSettingsPanel index={index} embeddings={embeddings} />;
}

export function IndexPanelHost({ workspaceId }: { workspaceId: string }): React.JSX.Element | null {
  const ipc = useIndexIpc();
  if (ipc === undefined) return null;
  return <Host ipc={ipc} workspaceId={workspaceId} />;
}

/** Whether the host has an index to configure (the Folder menu entry is offered only then). */
export function useHasIndexHost(): boolean {
  return useIndexIpc() !== undefined;
}
