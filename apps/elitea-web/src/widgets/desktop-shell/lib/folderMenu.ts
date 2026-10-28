/**
 * A folder's native context menu, one builder for the sidebar's folder rows
 * and the folder list: the first action (New thread / Open), Reveal in
 * Finder, Copy Path, the Project submenu, Remove. The project submenu is
 * capped (the host refuses menus past its item limit): a few dozen projects
 * show the first of them and a "More projects…" item that opens the picker
 * sheet, which lists all of them.
 */
import type { AppIpc } from '@/shared/desktop/appEvents';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { ContextAction } from '../model/useContextMenu';
import { copyText } from './conversationLink';

/** Projects listed in the submenu before "More projects…". */
const SUBMENU_PROJECTS = 20;
const LABEL_CHARS = 60;

interface ProjectLike {
  id: number;
  name: string;
  suspended?: boolean;
}

export interface FolderMenuInput {
  workspace: Workspace;
  projects: readonly ProjectLike[];
  appIpc: AppIpc | undefined;
  /** The first action: "New thread" in the sidebar, "Open" in the list. */
  primary: { label: string; enabled?: boolean; run: () => void };
  /** A git folder's "New thread in a worktree". */
  worktree?: () => void;
  bind: (projectId: number) => void;
  remove: () => void;
  /** Open the picker sheet (every project). */
  choosePicker: () => void;
}

const clip = (text: string): string => (text.length <= LABEL_CHARS ? text : `${text.slice(0, LABEL_CHARS - 1)}…`);

/** The projects a folder may be bound to: not suspended. */
export const bindable = <P extends ProjectLike>(projects: readonly P[]): P[] => projects.filter((project) => project.suspended !== true);

export function folderContextActions({ workspace, projects, appIpc, primary, worktree, bind, remove, choosePicker }: FolderMenuInput): ContextAction[] {
  const choices = bindable(projects);
  const shown = choices.slice(0, SUBMENU_PROJECTS).map((project) => ({
    id: `project:${String(project.id)}`,
    label: `${workspace.project_id === project.id ? '✓ ' : ''}${clip(project.name)}`,
    enabled: workspace.project_id !== project.id,
    run: () => bind(project.id),
  }));
  const more = choices.length > SUBMENU_PROJECTS ? [{ id: 'project:more', label: t('desktop.menu.moreProjects', 'More projects…'), run: choosePicker }] : [];
  return [
    { id: 'primary', label: primary.label, ...(primary.enabled === undefined ? {} : { enabled: primary.enabled }), run: primary.run },
    ...(worktree === undefined ? [] : [{ id: 'worktree', label: t('desktop.shell.newWorktreeThread', 'New thread in a worktree'), run: worktree }]),
    { separator: true },
    { id: 'reveal', label: t('workspace.path.reveal', 'Reveal in Finder'), enabled: appIpc !== undefined, run: () => void appIpc?.revealPath(workspace.id, '').catch(() => undefined) },
    { id: 'copy', label: t('workspace.path.copy', 'Copy Path'), run: () => copyText(workspace.path) },
    { separator: true },
    { label: t('workspace.project', 'Project'), enabled: choices.length > 0, submenu: [...shown, ...more] },
    { separator: true },
    { id: 'remove', label: t('desktop.shell.removeFolder', 'Remove from sidebar'), run: remove },
  ];
}
