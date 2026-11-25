/**
 * A sidebar folder row's hover actions: "New thread in …" and the ⋯ menu
 * (a git folder's "New thread in a worktree", the project to bind it to,
 * trust, and "Remove from sidebar").
 */
import { useState } from 'react';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import AltRouteOutlinedIcon from '@mui/icons-material/AltRouteOutlined';
import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import Tooltip from '@mui/material/Tooltip';

import type { Project } from '@/entities/project';
import { preferWorktree } from '@/features/workspace-worktrees';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { MenuDivider, MenuHeader, MenuRow, MenuRowIcon, MenuRowText, pressToOpen } from '@/shared/ui/native-menu';

import type { ShellAction } from '../lib/shellActions';

type Run = (action: ShellAction) => void;

/** A new thread of the folder that will work in a new task worktree. */
export function newWorktreeThread(workspace: Workspace, run: Run): void {
  preferWorktree(workspace.id);
  run({ type: 'open_workspace', workspaceId: workspace.id });
}

export interface FolderMenuProps {
  workspace: Workspace;
  projects: readonly Project[];
  run: Run;
  onBind: (projectId: number) => void;
  onRemove: () => void;
  /** "Trust this folder…" or "Revoke trust"; absent outside the desktop host. */
  trust?: { label: string; run: () => void } | null | undefined;
}

export function FolderMenu({ workspace, projects, run, onBind, onRemove, trust }: FolderMenuProps): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const close = (): void => setAnchor(null);
  const pick = (action: () => void) => () => {
    close();
    action();
  };
  return (
    <>
      <Tooltip title={t('desktop.shell.newThreadIn', 'New thread in {{name}}', { name: workspace.name })}>
        <IconButton
          size="small"
          aria-label={t('desktop.shell.newThreadIn', 'New thread in {{name}}', { name: workspace.name })}
          onClick={() => run({ type: 'open_workspace', workspaceId: workspace.id })}
        >
          <AddOutlinedIcon fontSize="inherit" />
        </IconButton>
      </Tooltip>
      <IconButton
        size="small"
        aria-label={t('desktop.shell.folderMenu', 'More for {{name}}', { name: workspace.name })}
        aria-haspopup="menu"
        {...pressToOpen(setAnchor)}
      >
        <MoreHorizIcon fontSize="inherit" />
      </IconButton>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={close} slotProps={{ list: { dense: true }, paper: { sx: { minWidth: '14rem' } } }}>
        {workspace.is_git && (
          <MenuRow onClick={pick(() => newWorktreeThread(workspace, run))}>
            <MenuRowIcon>
              <AltRouteOutlinedIcon />
            </MenuRowIcon>
            <MenuRowText>{t('desktop.shell.newWorktreeThread', 'New thread in a worktree')}</MenuRowText>
          </MenuRow>
        )}
        {workspace.is_git && <MenuDivider />}
        <MenuHeader>{workspace.project_id === null ? t('desktop.shell.bindProject', 'Project for this folder') : t('desktop.shell.moveToProject', 'Move to project')}</MenuHeader>
        {projects
          .filter((project) => !project.suspended)
          .map((project) => (
            <MenuRow key={project.id} role="menuitemradio" aria-checked={workspace.project_id === project.id} onClick={pick(() => onBind(project.id))}>
              <MenuRowIcon>{workspace.project_id === project.id && <CheckOutlinedIcon />}</MenuRowIcon>
              <MenuRowText>{project.name}</MenuRowText>
            </MenuRow>
          ))}
        <MenuDivider />
        {trust !== undefined && trust !== null && (
          <MenuRow onClick={pick(trust.run)} data-testid="folder-menu-trust">
            <MenuRowIcon />
            <MenuRowText>{trust.label}</MenuRowText>
          </MenuRow>
        )}
        <MenuRow onClick={pick(onRemove)}>
          <MenuRowIcon />
          <MenuRowText>{t('desktop.shell.removeFolder', 'Remove from sidebar')}</MenuRowText>
        </MenuRow>
      </Menu>
    </>
  );
}
