/**
 * What the index UI says for a rejected `index_*` command: by `code` where
 * the person can act on it, otherwise the host's own message (already
 * written for a person).
 */
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

export function describeIndexError(error: unknown): string {
  const { code, message } = toWorkspaceIpcError(error);
  switch (code) {
    case 'local_index_disabled':
      return t('workspace.index.policyOff', 'The local code index is turned off by your organisation’s policy.');
    case 'index_busy':
      return t('workspace.index.error.busy', 'The index is already being built. Wait for it to finish, or cancel it, and try again.');
    case 'index_refused':
      return t('workspace.index.error.refused', 'The index’s storage on this computer cannot be trusted. Run Troubleshoot to move it aside.');
    case 'index_newer_schema':
      return t('workspace.index.error.newerSchema', 'This index was written by a newer version of Elitea. Update the app to use it.');
    case 'index_storage':
      return t('workspace.index.error.storage', 'The index could not be read or written on this computer. Run Troubleshoot.');
    default:
      return message === '' ? t('workspace.failed', 'That did not work. Try again.') : message;
  }
}
