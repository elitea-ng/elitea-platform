/**
 * What the workspace screens say for a rejected host command. The host's
 * `{code, message}` (`WorkspaceIpcError`) is branched on by `code` where the
 * person can do something about it; anything else shows the host's own
 * message, which is already written for a person.
 */
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

export function describeWorkspaceError(error: unknown): string {
  const { code, message } = toWorkspaceIpcError(error);
  switch (code) {
    case 'workspace_busy':
      return t('workspace.error.busy', 'An agent is still working in this folder. Wait for it to finish, or stop it, and try again.');
    case 'agent_not_in_conversation':
      return t('workspace.error.agentNotInConversation', 'This agent is not part of the selected conversation. Start a new conversation, or pick one with this agent.');
    case 'agent_version_mismatch':
      return t('workspace.error.versionMismatch', 'The selected conversation runs another version of this agent. Start a new conversation, or pick that version.');
    case 'local_work_disabled':
      return t('workspace.error.localWorkDisabled', 'Local work is turned off by your organisation’s policy.');
    case 'not_signed_in':
      return t('workspace.error.notSignedIn', 'Sign in again to run agents on this computer.');
    case 'no_checkpoint':
      return t('workspace.error.noCheckpoint', 'This turn cannot be undone: the folder was too large to checkpoint.');
    case 'turn_expired':
      return t('workspace.error.turnExpired', 'This turn is too old to review or undo: only the most recent turns of a folder are kept.');
    default:
      return message === '' ? t('workspace.failed', 'That did not work. Try again.') : message;
  }
}
