/**
 * Hands the folder's index line (`indexLine`) to `children`: "Index ready · N
 * entities" for the toolbar's island when nothing else runs. `null` without a
 * host. Reads the query the settings dialog shares, so it costs no extra
 * request.
 */
import type { ReactNode } from 'react';

import type { IndexIpc } from '@/shared/desktop/indexIpc';

import { indexLine } from '../model/indexLine';
import { useIndexIpc } from '../model/ipcContext';
import { useWorkspaceIndex } from '../model/useWorkspaceIndex';

interface IndexIdleLineProps {
  workspaceId: string;
  children: (line: string | null) => ReactNode;
}

function Reader({ ipc, workspaceId, children }: IndexIdleLineProps & { ipc: IndexIpc }): ReactNode {
  const index = useWorkspaceIndex(ipc, workspaceId, { open: true });
  return children(indexLine(index.view));
}

export function IndexIdleLine({ workspaceId, children }: IndexIdleLineProps): ReactNode {
  const ipc = useIndexIpc();
  if (ipc === undefined) return children(null);
  return <Reader ipc={ipc} workspaceId={workspaceId}>{children}</Reader>;
}
