/**
 * The opened folders as a source-list table: name, path, project, index. A
 * row is selected with a click or the arrow keys and opened with a double
 * click or Return; everything else it can do (reveal, copy the path, bind a
 * project, remove) is in its native context menu, which the page owns.
 * Presentational — the page owns the IPC calls and the project list (the
 * picker's data comes from a widget this feature may not import).
 */
import { useState, type KeyboardEvent, type MouseEvent, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import type { Theme } from '@mui/material/styles';
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
  /** Open the folder's session (double click, Return); only offered for a folder with a project. */
  onSelect: (workspace: Workspace) => void;
  /** Right-click (or the keyboard's context menu key) on a row. */
  onContextMenu: (workspace: Workspace, event: MouseEvent) => void;
  /** The Index column's cell (the page's local-index chip, from a slice this one may not import). */
  indexCell?: (workspace: Workspace) => ReactNode;
}

const headSx = (theme: Theme) => ({
  textAlign: 'left',
  fontWeight: 500,
  fontSize: '0.6875rem',
  color: theme.vars.palette.text.metrics,
  padding: '4px 12px',
  borderBottom: `1px solid ${theme.vars.palette.divider}`,
  position: 'sticky',
  top: 0,
  background: theme.vars.palette.background.default,
});

const cellSx = { padding: '0 12px', height: 32, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', maxWidth: 0 } as const;

export function WorkspaceList({ workspaces, projects, onSelect, onContextMenu, indexCell }: WorkspaceListProps): React.JSX.Element {
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const projectName = (workspace: Workspace): string | undefined => projects.find((p) => p.id === workspace.project_id)?.name;

  const onKeyDown = (event: KeyboardEvent<HTMLTableRowElement>, workspace: Workspace, index: number): void => {
    if (event.key === 'Enter') {
      if (workspace.project_id !== null) onSelect(workspace);
      return;
    }
    const step = event.key === 'ArrowDown' ? 1 : event.key === 'ArrowUp' ? -1 : 0;
    if (step === 0) return;
    event.preventDefault();
    const next = workspaces[index + step];
    if (next === undefined) return;
    setSelectedId(next.id);
    (event.currentTarget.parentElement?.children[index + step] as HTMLElement | undefined)?.focus();
  };

  return (
    <Box component="table" aria-label={t('workspace.title', 'Local work')} sx={{ width: '100%', borderCollapse: 'collapse', tableLayout: 'fixed' }}>
      <Box component="thead">
        <Box component="tr">
          <Box component="th" scope="col" sx={headSx} style={{ width: '24%' }}>
            {t('workspace.col.name', 'Name')}
          </Box>
          <Box component="th" scope="col" sx={headSx}>
            {t('workspace.col.path', 'Path')}
          </Box>
          <Box component="th" scope="col" sx={headSx} style={{ width: '20%' }}>
            {t('workspace.project', 'Project')}
          </Box>
          {indexCell !== undefined && (
            <Box component="th" scope="col" sx={headSx} style={{ width: '24%' }}>
              {t('workspace.col.index', 'Index')}
            </Box>
          )}
        </Box>
      </Box>
      <Box component="tbody">
        {workspaces.map((workspace, index) => {
          const selected = workspace.id === selectedId;
          const project = projectName(workspace);
          return (
            <Box
              component="tr"
              key={workspace.id}
              data-testid="workspace-row"
              tabIndex={0}
              aria-selected={selected}
              onClick={() => setSelectedId(workspace.id)}
              onFocus={() => setSelectedId(workspace.id)}
              onDoubleClick={() => workspace.project_id !== null && onSelect(workspace)}
              onKeyDown={(event: KeyboardEvent<HTMLTableRowElement>) => onKeyDown(event, workspace, index)}
              onContextMenu={(event: MouseEvent) => onContextMenu(workspace, event)}
              sx={(theme: Theme) => ({
                cursor: 'default',
                outline: 'none',
                background: selected ? `color-mix(in srgb, ${theme.vars.palette.primary.main} 16%, transparent)` : index % 2 === 1 ? `color-mix(in srgb, ${theme.vars.palette.text.primary} 3%, transparent)` : 'transparent',
                '&:hover': selected ? {} : { background: `color-mix(in srgb, ${theme.vars.palette.text.primary} 6%, transparent)` },
                '&:focus-visible': { boxShadow: `inset 0 0 0 2px color-mix(in srgb, ${theme.vars.palette.primary.main} 55%, transparent)` },
              })}
            >
              <Box component="td" sx={cellSx}>
                <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, minWidth: 0 }}>
                  <Typography variant="labelMedium" component="span" sx={(theme: Theme) => ({ overflow: 'hidden', textOverflow: 'ellipsis', color: theme.vars.palette.text.primary })}>
                    {workspace.name}
                  </Typography>
                  {workspace.is_git && <Chip size="small" variant="outlined" label={t('workspace.git', 'Git')} />}
                </Box>
              </Box>
              <Box component="td" sx={cellSx} title={workspace.path}>
                <Typography variant="bodySmall" component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}>
                  {workspace.path}
                </Typography>
              </Box>
              <Box component="td" sx={cellSx}>
                <Typography variant="bodySmall" component="span" sx={(theme: Theme) => ({ color: project === undefined ? theme.vars.palette.warning.main : theme.vars.palette.text.primary })}>
                  {project ?? t('workspace.bindHint', 'Bind a project to work here')}
                </Typography>
              </Box>
              {indexCell !== undefined && (
                <Box component="td" sx={{ ...cellSx, maxWidth: 'none' }}>
                  {indexCell(workspace)}
                </Box>
              )}
            </Box>
          );
        })}
      </Box>
    </Box>
  );
}
