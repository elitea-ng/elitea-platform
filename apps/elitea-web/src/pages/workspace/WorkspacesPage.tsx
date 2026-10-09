/**
 * Desktop-only `/workspaces`: the folders the user has opened, each bindable
 * to one project (whose agents the session view then offers).
 */
import { useMemo } from 'react';

import { useNavigate } from '@tanstack/react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';

import { WorkspaceList, describeWorkspaceError, useWorkspaceIpc } from '@/features/workspace';
import { toWorkspaceIpcError, type Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';

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

  if (ipc === undefined) {
    return (
      <Box sx={{ p: '1.5rem' }}>
        <Alert severity="info">{t('workspace.noHost', 'Workspaces need the Elitea desktop app.')}</Alert>
      </Box>
    );
  }

  const failed = list.isError || open.isError || remove.isError || bind.isError;

  return (
    <Box data-testid="workspaces-page" sx={{ p: '1.5rem', display: 'flex', flexDirection: 'column', gap: 2 }}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 2 }}>
        <Typography component="h1" variant="headingMedium">
          {t('workspace.title', 'Workspaces')}
        </Typography>
        <Button sx={{ marginLeft: 'auto' }} variant="contained" disabled={open.isPending} onClick={() => open.mutate()}>
          {t('workspace.openFolder', 'Open folder')}
        </Button>
      </Box>
      {failed && (
        <Alert severity="error">
          {remove.isError && toWorkspaceIpcError(remove.error).code === 'workspace_busy'
            ? describeWorkspaceError(remove.error)
            : t('workspace.failed', 'That did not work. Try again.')}
        </Alert>
      )}
      {list.isPending && <CircularProgress aria-label={t('workspace.loading', 'Loading workspaces')} />}
      {!list.isPending && workspaces.length === 0 && (
        <NoResultsMessage
          title={t('workspace.emptyTitle', 'No folders yet')}
          description={t('workspace.empty', 'Open a folder to let an agent work on it.')}
        />
      )}
      {workspaces.length > 0 && (
        <WorkspaceList
          workspaces={workspaces}
          projects={projects}
          onSelect={(workspace) => void navigate({ to: '/workspaces/$workspaceId', params: { workspaceId: workspace.id } })}
          onRemove={(workspace) => remove.mutate(workspace)}
          onBind={(workspace, projectId) => bind.mutate({ workspace, projectId })}
        />
      )}
    </Box>
  );
}
