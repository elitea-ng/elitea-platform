/** What a workspace's folder mostly holds: `git` inside a git work tree, else from a sample of its names. */
export type FolderKind = 'git' | 'code' | 'documents' | 'mixed';

/** A folder opened as a workspace (IPC.md, `workspace_list`), and the folders a person opened. */
export interface Workspace {
  id: string;
  path: string;
  name: string;
  project_id: number | null;
  is_git: boolean;
  /** What the folder mostly holds (IPC.md, "Workspaces"); absent from an older host. */
  kind?: FolderKind;
  /** A task worktree's workspace (IPC.md, "Parallel agents"): the folder whose repository it is a worktree of. */
  parent_id?: string;
  /** The thread the task worktree was made for. */
  thread_uuid?: string;
}

/** The folders a person opened: a task worktree is listed under its folder, never on its own. */
export function openedFolders(list: readonly Workspace[]): Workspace[] {
  return list.filter((workspace) => workspace.parent_id === undefined);
}
