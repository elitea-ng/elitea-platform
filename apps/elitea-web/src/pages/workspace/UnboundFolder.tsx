/**
 * A folder with no project yet: a thread needs one (its agents, and where the
 * conversation is saved), so the screen asks for it.
 */
import { useMutation, useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { describeWorkspaceError } from '@/features/workspace';
import type { Workspace, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { ShowSidebarButton, TitleBarSpacer, useDesktopLayout } from '@/widgets/desktop-shell';

import { COLUMN, FolderTitle } from './sessionParts';
import { useBindableProjects } from './useBindableProjects';

const LIST_KEY = ['workspace', 'list'] as const;

export function UnboundFolder({ ipc, workspace }: { ipc: WorkspaceIpc; workspace: Workspace }): React.JSX.Element {
  const projects = useBindableProjects();
  const queryClient = useQueryClient();
  const sidebarOpen = useDesktopLayout((state) => state.sidebarOpen);
  const bind = useMutation({
    mutationFn: (projectId: number) => ipc.bindProject(workspace.id, projectId),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: LIST_KEY }),
  });
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
      <Box sx={(theme: Theme) => ({ borderBottom: `1px solid ${theme.vars.palette.divider}` })}>
        <TitleBarSpacer leading={!sidebarOpen} logo={false}>
          <ShowSidebarButton />
          <FolderTitle workspace={workspace} />
        </TitleBarSpacer>
      </Box>
      <Box sx={{ ...COLUMN, paddingY: 6, display: 'flex', flexDirection: 'column', gap: 2, alignItems: 'flex-start' }}>
        <Alert severity="info">{t('workspace.needsProject', 'Bind a project to this folder first.')}</Alert>
        <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}>
          {t('workspace.bindExplain', 'The folder’s project decides which agents can work here and where the threads are saved.')}
        </Typography>
        <TextField
          select
          size="small"
          sx={{ minWidth: '16rem' }}
          label={t('workspace.project', 'Project')}
          value=""
          disabled={bind.isPending}
          onChange={(event) => bind.mutate(Number(event.target.value))}
        >
          {projects.map((project) => (
            <MenuItem key={project.id} value={project.id}>
              {project.name}
            </MenuItem>
          ))}
        </TextField>
        {bind.isError && <Alert severity="error">{describeWorkspaceError(bind.error)}</Alert>}
      </Box>
    </Box>
  );
}
