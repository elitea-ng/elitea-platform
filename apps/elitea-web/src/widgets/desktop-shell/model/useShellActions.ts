/**
 * Runs a `ShellAction`. The single dispatcher behind the shortcuts, the
 * palette, the sidebar and the host's `app://command`.
 */
import { useCallback } from 'react';

import { useNavigate, useRouter } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';

import { describeWorkspaceError, readLastLocation, useWorkspaceIpc } from '@/features/workspace';
import { t } from '@/shared/i18n';

import type { ShellAction } from '../lib/shellActions';
import { useDesktopLayout } from './desktopLayout.store';

export const WORKSPACE_LIST_KEY = ['workspace', 'list'] as const;

export function useShellActions(): (action: ShellAction) => void {
  const navigate = useNavigate();
  const router = useRouter();
  const ipc = useWorkspaceIpc();
  const queryClient = useQueryClient();

  const openWorkspace = useCallback(
    (workspaceId: string, conversationId?: string) => {
      void navigate({
        to: '/workspaces/$workspaceId',
        params: { workspaceId },
        search: conversationId === undefined || conversationId === '' ? {} : { conversation: conversationId },
      });
    },
    [navigate],
  );

  const refreshWorkspaces = useCallback(
    (): Promise<void> => queryClient.invalidateQueries({ queryKey: WORKSPACE_LIST_KEY }),
    [queryClient],
  );

  return useCallback(
    (action: ShellAction) => {
      const layout = useDesktopLayout.getState();
      // Every action but the palette toggle itself leaves the palette.
      if (action.type !== 'command_palette') layout.setPaletteOpen(false);
      const handlers: { [K in ShellAction['type']]: (a: Extract<ShellAction, { type: K }>) => void } = {
        command_palette: () => layout.setPaletteOpen(!layout.paletteOpen),
        toggle_sidebar: () => layout.setSidebarOpen(!layout.sidebarOpen),
        toggle_changes: () => layout.setChangesOpen(!layout.changesOpen),
        toggle_plan: () => layout.session?.togglePlanMode(),
        back: () => router.history.back(),
        forward: () => router.history.forward(),
        settings: () => void navigate({ to: '/settings' }),
        go: (a) => void navigate({ to: a.to }),
        folders: () => {
          const last = readLastLocation();
          if (last === null) void navigate({ to: '/workspaces' });
          else openWorkspace(last.workspaceId, last.conversationId);
        },
        open_workspace: (a) => openWorkspace(a.workspaceId, a.conversationId),
        new_thread: () => {
          if (layout.session !== null) {
            layout.session.newThread();
            return;
          }
          const last = readLastLocation();
          if (last !== null) openWorkspace(last.workspaceId);
          else void navigate({ to: '/workspaces' });
        },
        // The in-app button: the host's `workspace_open` runs the picker and answers here.
        open_folder: () => {
          ipc?.open().then(
            async (workspace) => {
              if (workspace === null) return;
              await refreshWorkspaces();
              openWorkspace(workspace.id);
            },
            (error: unknown) => layout.setNotice(describeWorkspaceError(error)),
          );
        },
        workspaces_changed: (a) => void refreshWorkspaces().then(() => openWorkspace(a.workspaceId)),
        notice: (a) => layout.setNotice(a.message === '' ? t('desktop.shell.openFolderFailed', 'That folder could not be opened.') : a.message),
        files_dropped: () => layout.setNotice(t('desktop.shell.dropFolder', 'Drop a folder, not files, to open it as a workspace.')),
      };
      (handlers[action.type] as (a: ShellAction) => void)(action);
    },
    [ipc, navigate, openWorkspace, refreshWorkspaces, router],
  );
}
