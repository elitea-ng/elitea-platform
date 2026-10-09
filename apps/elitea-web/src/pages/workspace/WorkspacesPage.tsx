/**
 * Desktop-only `/workspaces`: the start screen of the workspace-first window.
 * Without folders it is "Open a folder to start" (the button, or a folder
 * dropped on the window — the host handles the drop and opens it); with
 * folders it lists them, each bindable to one project (whose agents its
 * threads then offer). After sign-in the desktop opens the last thread
 * instead (`routes/_shell/index.tsx`), so this is mostly the first run.
 */
import { useMemo } from 'react';

import { useNavigate } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import CreateNewFolderOutlinedIcon from '@mui/icons-material/CreateNewFolderOutlined';

import { WorkspaceList, describeWorkspaceError, useWorkspaceIpc } from '@/features/workspace';
import { toWorkspaceIpcError, type Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { ShowSidebarButton, TitleBarSpacer, useDesktopLayout } from '@/widgets/desktop-shell';

import { useBindableProjects } from './useBindableProjects';

const LIST_KEY = ['workspace', 'list'] as const;

export default function WorkspacesPage(): React.JSX.Element {
  const ipc = useWorkspaceIpc();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const projects = useBindableProjects();

  const list = useQuery({ queryKey: LIST_KEY, queryFn: () => ipc?.list() ?? Promise.resolve([]), enabled: ipc !== undefined });
  const refresh = (): Promise<void> => queryClient.invalidateQueries({ queryKey: LIST_KEY });
  const open = useMutation({ mutationFn: () => ipc?.open() ?? Promise.resolve(null), onSuccess: refresh });
  const remove = useMutation({ mutationFn: (workspace: Workspace) => ipc?.remove(workspace.id) ?? Promise.resolve(), onSuccess: refresh });
  const bind = useMutation({
    mutationFn: (args: { workspace: Workspace; projectId: number }) => ipc?.bindProject(args.workspace.id, args.projectId) ?? Promise.resolve(),
    onSuccess: refresh,
  });

  const workspaces = useMemo(() => list.data ?? [], [list.data]);
  const sidebarOpen = useDesktopLayout((state) => state.sidebarOpen);

  if (ipc === undefined) {
    return (
      <Box sx={{ p: '1.5rem' }}>
        <Alert severity="info">{t('workspace.noHost', 'Workspaces need the Elitea desktop app.')}</Alert>
      </Box>
    );
  }

  const failed = list.isError || open.isError || remove.isError || bind.isError;
  // A folder with a running turn can be neither removed nor re-bound (the host answers workspace_busy).
  const busyError = [remove.error, bind.error].find((error) => error !== null && toWorkspaceIpcError(error).code === 'workspace_busy');

  const openButton = (
    <Button variant="contained" disabled={open.isPending} onClick={() => open.mutate()} startIcon={<CreateNewFolderOutlinedIcon />}>
      {t('workspace.openFolder', 'Open folder')}
    </Button>
  );

  return (
    <Box data-testid="workspaces-page" sx={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
      <TitleBarSpacer leading={!sidebarOpen} logo={false}>
        <ShowSidebarButton />
      </TitleBarSpacer>
      <Box sx={{ width: '100%', maxWidth: '46rem', marginX: 'auto', paddingX: 3, boxSizing: 'border-box', display: 'flex', flexDirection: 'column', gap: 2 }}>
        {failed && (
          <Alert severity="error">
            {busyError === undefined ? t('workspace.failed', 'That did not work. Try again.') : describeWorkspaceError(busyError)}
          </Alert>
        )}
        {list.isPending && <CircularProgress aria-label={t('workspace.loading', 'Loading workspaces')} />}
        {!list.isPending && workspaces.length === 0 && (
          <Box
            data-testid="workspaces-empty"
            sx={(theme: Theme) => ({
              marginTop: '12vh',
              paddingY: 6,
              paddingX: 3,
              display: 'flex',
              flexDirection: 'column',
              alignItems: 'center',
              gap: 1.5,
              textAlign: 'center',
              border: `1px dashed ${theme.vars.palette.divider}`,
              borderRadius: theme.vars.shape.radiusMd,
            })}
          >
            <CreateNewFolderOutlinedIcon sx={(theme: Theme) => ({ width: '2.5rem', height: '2.5rem', color: theme.vars.palette.text.metrics })} />
            <Typography component="h1" variant="headingMedium">
              {t('workspace.start.title', 'Open a folder to start')}
            </Typography>
            <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, maxWidth: '26rem' })}>
              {t('workspace.start.body', 'An agent works on the files of a folder on this computer. You approve what it runs, and you can review and undo every change.')}
            </Typography>
            {openButton}
            <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
              {t('workspace.start.drop', 'Or drop a folder on this window.')}
            </Typography>
          </Box>
        )}
        {workspaces.length > 0 && (
          <>
            <Box sx={{ display: 'flex', alignItems: 'center', gap: 2, paddingTop: 3 }}>
              <Typography component="h1" variant="headingMedium">
                {t('workspace.title', 'Local work')}
              </Typography>
              <Box sx={{ marginLeft: 'auto' }}>{openButton}</Box>
            </Box>
            <WorkspaceList
              workspaces={workspaces}
              projects={projects}
              onSelect={(workspace) => void navigate({ to: '/workspaces/$workspaceId', params: { workspaceId: workspace.id } })}
              onRemove={(workspace) => remove.mutate(workspace)}
              onBind={(workspace, projectId) => bind.mutate({ workspace, projectId })}
            />
          </>
        )}
      </Box>
    </Box>
  );
}
