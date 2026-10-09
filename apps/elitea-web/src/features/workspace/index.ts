/**
 * Public API — desktop-only Workspace view building blocks (ADR-0029 D0):
 * the turn event reducer + hook, the approval dialog, the transcript, the
 * changed-files card, the workspace list and the composer's "@"/"/"
 * menu (tokens, local commands, popup). Reached only from
 * `pages/workspace`, which is itself reached only in the `desktop` build.
 */
import { matchingCommands, workspaceCommands } from './model/composerCommands';
import { activeToken, mentionText, referencedPaths } from './model/composerTokens';
import { readLastLocation, writeLastLocation } from './model/threads';

export { ApprovalDialog } from './ui/ApprovalDialog';
export { ChangedFilesCard } from './ui/ChangedFilesCard';
export { TurnTranscript } from './ui/TurnTranscript';
export { WorkspaceList } from './ui/WorkspaceList';
export type { ProjectChoice } from './ui/WorkspaceList';
export { WorkspaceIpcProvider, useWorkspaceIpc } from './model/ipcContext';
export { useWorkspaceTurn } from './model/useWorkspaceTurn';
export { replayView } from './model/turnReducer';
export { describeWorkspaceError } from './model/describeWorkspaceError';
export { readThreads, recordThread, threadsQueryKey } from './model/threads';
export type { WorkspaceThread } from './model/threads';
export type { WorkspaceTurn } from './model/useWorkspaceTurn';
export { SuggestionMenu } from './ui/SuggestionMenu';
export type { SuggestionItem } from './ui/SuggestionMenu';
export type { WorkspaceCommandId } from './model/composerCommands';

/** The composer's text rules ("@"/"/" tokens, referenced paths) and its local "/" commands, as one bundle. */
export const composer = { activeToken, mentionText, referencedPaths, matchingCommands, workspaceCommands };

/** The folder and thread the person was last in (desktop home), read and written as one pair. */
export const lastLocation = { read: readLastLocation, write: writeLastLocation };
