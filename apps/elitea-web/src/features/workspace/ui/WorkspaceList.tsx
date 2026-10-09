/**
 * The opened folders: name, path, git badge, bound project, and the actions on
 * each. Presentational — the page owns the IPC calls and the project list
 * (the picker's data comes from a widget this feature may not import).
 */
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

export interface ProjectChoice {
  id: number;
  name: string;
}

export interface WorkspaceListProps {
  workspaces: readonly Workspace[];
  projects: readonly ProjectChoice[];
  onSelect: (workspace: Workspace) => void;
  onRemove: (workspace: Workspace) => void;
  onBind: (workspace: Workspace, projectId: number) => void;
}

export function WorkspaceList({ workspaces, projects, onSelect, onRemove, onBind }: WorkspaceListProps): React.JSX.Element {
  return (
    <Box component="ul" sx={{ margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 1 }}>
      {workspaces.map((workspace) => (
        <Box
          component="li"
          key={workspace.id}
          data-testid="workspace-row"
          sx={(theme: Theme) => ({
            listStyle: 'none',
            display: 'flex',
            alignItems: 'center',
            gap: 2,
            flexWrap: 'wrap',
            padding: 1.5,
            border: `1px solid ${theme.vars.palette.divider}`,
            borderRadius: theme.vars.shape.radiusMd,
          })}
        >
          <Box sx={{ flex: '1 1 16rem', minWidth: 0 }}>
            <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
              <Typography variant="headingSmall">{workspace.name}</Typography>
              {workspace.is_git && <Chip size="small" variant="outlined" label={t('workspace.git', 'Git')} />}
            </Box>
            <Typography variant="bodySmall" sx={{ color: (theme: Theme) => theme.vars.palette.text.secondary, wordBreak: 'break-all' }}>
              {workspace.path}
            </Typography>
          </Box>
          <TextField
            select
            size="small"
            sx={{ minWidth: '14rem' }}
            label={t('workspace.project', 'Project')}
            value={workspace.project_id !== null && projects.some((p) => p.id === workspace.project_id) ? workspace.project_id : ''}
            onChange={(event) => onBind(workspace, Number(event.target.value))}
            helperText={workspace.project_id === null ? t('workspace.bindHint', 'Bind a project to work here') : undefined}
          >
            {projects.map((project) => (
              <MenuItem key={project.id} value={project.id}>
                {project.name}
              </MenuItem>
            ))}
          </TextField>
          <Button variant="contained" disabled={workspace.project_id === null} onClick={() => onSelect(workspace)}>
            {t('workspace.openSession', 'Open')}
          </Button>
          <Button color="error" onClick={() => onRemove(workspace)} aria-label={t('workspace.removeNamed', 'Remove {{name}}', { name: workspace.name })}>
            {t('workspace.remove', 'Remove')}
          </Button>
        </Box>
      ))}
    </Box>
  );
}
