/**
 * Choosing a project in the sidebar's switcher. A Local work thread keeps the
 * app on its folder's project while it is on screen (`useFolderProject`,
 * `pages/workspace/useThreadSession.ts`), so switching to another project from
 * a folder that belongs to one would be put straight back: the folder's thread
 * is left first, for the start screen, and the new project is chosen there.
 * A folder with no project shows under every project and stays open.
 */
import { useNavigate } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';

import type { Workspace } from '@/shared/desktop/workspaceIpc';

import { WORKSPACE_LIST_KEY } from './useShellActions';

const FOLDER_PATH = /^\/workspaces\/([^/]+)/;

export function useSwitchProject(pathname: string, onSelectProject: (projectId: string, projectName: string) => void): (projectId: string, projectName: string) => void {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  return (projectId, projectName) => {
    const open = FOLDER_PATH.exec(pathname)?.[1];
    const folder = open === undefined ? undefined : queryClient.getQueryData<Workspace[]>(WORKSPACE_LIST_KEY)?.find((workspace) => workspace.id === open);
    const boundElsewhere = folder !== undefined && folder.project_id !== null && String(folder.project_id) !== projectId;
    if (!boundElsewhere) {
      onSelectProject(projectId, projectName);
      return;
    }
    void navigate({ to: '/workspaces' }).then(() => onSelectProject(projectId, projectName));
  };
}
