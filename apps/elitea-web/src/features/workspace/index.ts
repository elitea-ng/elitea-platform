/**
 * Public API — desktop-only Workspace view building blocks (ADR-0029 D0):
 * the turn event reducer + hook, the approval dialog, the transcript, the
 * changed-files card, the workspace list and the composer's "@"/"/"
 * menu (tokens, local commands, the agent's skills, popup). Reached only from
 * `pages/workspace`, which is itself reached only in the `desktop` build.
 */
import { lazy } from 'react';

import { customCommandDetail, customCommandSource, customCommandToken, invokedPick, modelCommandName, slashMenu } from './model/composerCommands';
import { matchingSkills, skillToken } from './model/composerSkills';
import { activeToken, mentionText, referencedPaths } from './model/composerTokens';
import { readLastLocation, writeLastLocation } from './model/threads';
import { latestPlan } from './model/turnReducer';
import { ApprovalCard } from './ui/ApprovalCard';
import { ApprovalDialog } from './ui/ApprovalDialog';
import { ModelChip } from './ui/ModelChip';
import { PlanCard } from './ui/PlanCard';

export { ChangedFilesCard } from './ui/ChangedFilesCard';
export { TurnTranscript } from './ui/TurnTranscript';
export { WorkspaceList } from './ui/WorkspaceList';
export type { ProjectChoice } from './ui/WorkspaceList';
export { WorkspaceIpcProvider, useWorkspaceIpc } from './model/ipcContext';
export { useWorkspaceTurn } from './model/useWorkspaceTurn';
export { replayView } from './model/turnReducer';
export { describeWorkspaceError } from './model/describeWorkspaceError';
export { readThreads, recordThread, threadsQueryKey } from './model/threads';
export type { WorkspaceTurn } from './model/useWorkspaceTurn';
export { SuggestionMenu } from './ui/SuggestionMenu';
export type { SuggestionItem } from './ui/SuggestionMenu';
export type { WorkspaceCommandId } from './model/composerCommands';

/** The composer's text rules ("@"/"/" tokens, referenced paths), its "/" menu (local commands, the custom commands, the agent's skills) and its model chip, as one bundle. */
export const composer = {
  activeToken,
  mentionText,
  referencedPaths,
  slashMenu,
  modelCommandName,
  customCommandToken,
  customCommandSource,
  customCommandDetail,
  invokedPick,
  matchingSkills,
  skillToken,
  ModelChip,
};

/** The folder and thread the person was last in (desktop home), read and written as one pair. */
export const lastLocation = { read: readLastLocation, write: writeLastLocation };

/** A tool's approval: the dialog (local turns) and the inline card (cloud transcripts). */
export const approval = { Dialog: ApprovalDialog, Card: ApprovalCard };

/**
 * Plan mode and the agent's checklist (#1225): the proposed plan's card, the todo pane (the session's Plan tab,
 * loaded only when the tab is shown) and the latest plan of a thread's turns.
 */
export const plan = {
  Card: PlanCard,
  TodoPane: lazy(() => import('./ui/TodoPane').then((module) => ({ default: module.TodoPane }))),
  latest: latestPlan,
};
