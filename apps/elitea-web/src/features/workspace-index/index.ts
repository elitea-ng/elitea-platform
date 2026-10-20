/**
 * Public API — the desktop-only local code index of a workspace (ADR-0029
 * decision 7): its status chip with the settings dialog behind it, and where
 * it gets its host client. Reached only from `pages/workspace`, which is
 * itself reached only in the `desktop` build.
 */
export { WorkspaceIndexControl } from './ui/WorkspaceIndexControl';
export { IndexIpcProvider } from './model/ipcContext';
export { IndexPanelHost, useHasIndexHost } from './ui/IndexPanelHost';
export { useIndexIpc } from './model/ipcContext';
export { IndexIdleLine } from './ui/IndexIdleLine';
export { foldProgress } from './model/progress';
