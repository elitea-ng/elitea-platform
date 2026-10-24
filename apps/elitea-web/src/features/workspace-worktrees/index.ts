/**
 * Public API — the desktop-only parallel tasks of a git folder (#1235): its
 * task worktrees in the sidebar, with their diff, merge back (previewed,
 * checkpointed, undoable) and remove, and "New parallel task". Loaded only
 * when a git folder's threads are shown (`widgets/desktop-shell`), which is
 * itself reached only in the `desktop` build.
 */
import { lazy } from 'react';

export const WorktreesPanel = lazy(() => import('./ui/WorktreesPanel').then((module) => ({ default: module.WorktreesPanel })));
