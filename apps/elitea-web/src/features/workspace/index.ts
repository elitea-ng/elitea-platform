/**
 * Public API — desktop-only Workspace view building blocks (ADR-0029 D0):
 * the turn event reducer + hook, the approval dialog, the transcript, the
 * changed-files card and the workspace list. Reached only from
 * `pages/workspace`, which is itself reached only in the `desktop` build.
 */
export { ApprovalDialog } from './ui/ApprovalDialog';
export { ChangedFilesCard } from './ui/ChangedFilesCard';
export { TurnTranscript } from './ui/TurnTranscript';
export { WorkspaceList } from './ui/WorkspaceList';
export type { ProjectChoice } from './ui/WorkspaceList';
export { WorkspaceIpcProvider, useWorkspaceIpc } from './model/ipcContext';
export { useWorkspaceTurn } from './model/useWorkspaceTurn';
export type { WorkspaceTurn } from './model/useWorkspaceTurn';
