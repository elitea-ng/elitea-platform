/**
 * Public API — the desktop-only parallel tasks of a git folder (#1235): its
 * task worktrees in the sidebar, under the folder as threads, with their diff,
 * merge back (previewed, checkpointed, undoable) and remove; and a new
 * thread's "Work in › New worktree" (`useStartInWorktree`, `workIn`). The
 * panel is loaded only when a git folder's threads are shown
 * (`widgets/desktop-shell`), which is itself reached only in the `desktop`
 * build.
 */
import { lazy } from 'react';

export const WorktreesPanel = lazy(() => import('./ui/WorktreesPanel').then((module) => ({ default: module.WorktreesPanel })));
export type { WorktreeRowParts } from './ui/WorktreesPanel';
export { StartInWorktreeError, useStartInWorktree } from './model/useStartInWorktree';
export { holdFirstMessage, initialWorkIn, listenForWorktree, preferWorktree, takeFirstMessage, type WorkIn } from './model/workIn';
