/**
 * What the workspace screens say for a rejected host command. The host's
 * `{code, message}` (`WorkspaceIpcError`) is branched on by `code` where the
 * person can do something about it; anything else shows the host's own
 * message, which is already written for a person.
 */
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

/** The codes a person can act on, and what to say for each. */
const ACTIONABLE: Readonly<Record<string, () => string>> = {
  workspace_busy: () => t('workspace.error.busy', 'An agent is still working in this folder. Wait for it to finish, or stop it, and try again.'),
  agent_not_in_conversation: () => t('workspace.error.agentNotInConversation', 'This agent is not part of the selected conversation. Start a new conversation, or pick one with this agent.'),
  agent_version_mismatch: () => t('workspace.error.versionMismatch', 'The selected conversation runs another version of this agent. Start a new conversation, or pick that version.'),
  local_work_disabled: () => t('workspace.error.localWorkDisabled', 'Local work is turned off by your organisation’s policy.'),
  not_signed_in: () => t('workspace.error.notSignedIn', 'Sign in again to run agents on this computer.'),
  turn_expired: () => t('workspace.error.turnExpired', 'This turn is too old to review or undo: only the most recent turns of a folder are kept.'),
  undo_not_latest: () => t('workspace.error.undoNotLatest', 'Only the newest turn of a folder can be undone. To go back further, restore the folder to before this turn.'),
  file_changed_since: () => t('workspace.error.fileChangedSince', 'This file changed after this turn (a later turn or your own edit), so it was not reverted.'),
  session_replaced: () => t('workspace.error.sessionReplaced', 'This folder’s checkpoints are kept another way now (it became, or stopped being, a git repository), so this turn cannot be undone here.'),
  pipeline_unsupported: () => t('workspace.error.pipelineUnsupported', 'Pipelines do not run on this computer yet. Run this one in the cloud: it opens as a chat.'),
  already_undone: () => t('workspace.error.alreadyUndone', 'This turn’s changes were already undone.'),
};

export function describeWorkspaceError(error: unknown): string {
  const { code, message } = toWorkspaceIpcError(error);
  const known = Object.hasOwn(ACTIONABLE, code) ? ACTIONABLE[code] : undefined;
  if (known !== undefined) return known();
  return message === '' ? t('workspace.failed', 'That did not work. Try again.') : message;
}
