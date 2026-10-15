/**
 * What the handoff screens say for a rejected `handoff_*` command: by `code`
 * where the person can act on it, otherwise the host's own message (already
 * written for a person). The structured part (`details`: findings, the caps
 * breakdown, quota figures) is shown by the dialogs next to this text.
 */
import { toHandoffIpcError, type CapsBreakdown, type SecretFinding } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

/** One line per code the person can act on; the text is built when asked (the catalogue may not be loaded at import). */
const MESSAGES: Readonly<Record<string, () => string>> = {
  cloud_handoff_disabled: () => t('workspace.handoff.error.disabled', 'Sending threads to the cloud is turned off by your organisation’s policy.'),
  secret_found: () => t('workspace.handoff.error.secret', 'Some files look like they hold secrets, so nothing was sent. Leave them on your laptop, or edit them, and try again.'),
  handoff_too_large: () => t('workspace.handoff.error.tooLarge', 'This folder is too large to send. The limits it went over are listed below.'),
  workspace_quota_exceeded: () =>
    t('workspace.handoff.error.quota', 'This project’s cloud workspace storage is full. Erase another thread’s cloud copy, or ask a project admin to raise the quota.'),
  history_mode_not_allowed: () => t('workspace.handoff.error.history', 'This project does not allow sending the git history.'),
  local_turn_in_progress: () => t('workspace.handoff.error.localTurn', 'A turn in this thread has not been committed yet. Wait for it to finish, then send.'),
  workspace_busy: () => t('workspace.error.busy', 'An agent is still working in this folder. Wait for it to finish, or stop it, and try again.'),
  handoff_in_progress: () => t('workspace.handoff.error.inProgress', 'This thread is already being sent. Try again in a minute.'),
  thread_executor_is_cloud: () => t('workspace.handoff.error.isCloud', 'This thread already runs in the cloud.'),
  handoff_unsupported: () => t('workspace.handoff.error.unsupported', 'Only the top folder of a git repository (or a folder with no repository) can be sent.'),
  unsupported_object_format: () => t('workspace.handoff.error.sha256', 'Repositories that use SHA-256 object names cannot be sent yet.'),
  handoff_not_reclaimed: () => t('workspace.handoff.error.notReclaimed', 'The thread has not been handed back by the cloud yet. Try Take back again.'),
  handoff_unknown: () => t('workspace.handoff.error.unknown', 'This thread was not sent from this computer, so it can only be taken back there.'),
  integrity_failed: () =>
    t('workspace.handoff.error.integrity', 'The files that came back from the cloud failed their integrity check, so nothing was written to your folder.'),
  handoff_conflict: () => t('workspace.handoff.error.conflict', 'The cloud copy changed while taking it back. Try Take back again.'),
  folder_changed_since_preflight: () =>
    t('workspace.handoff.error.folderChanged', 'Your folder changed since it was checked. Take back again to check it once more.'),
  include_confirm_required: () =>
    t('workspace.handoff.error.includeConfirm', 'Some files you chose to include look like they hold secrets. Confirm sending them for this send, or remove the include.'),
  env_too_large: () =>
    t('workspace.handoff.error.envTooLarge', 'The environment variables you chose are over the limit. Choose fewer, or shorter ones, and try again.'),
  handoff_cancelled: () => t('workspace.handoff.cancelled', 'Sending was cancelled; the thread stays on this computer.'),
  env_harvest_disabled: () => t('workspace.handoff.error.envDisabled', 'Your organisation does not allow sending environment variables with a thread.'),
  include_not_allowed: () => t('workspace.handoff.error.includeNotAllowed', 'Your organisation does not allow including files that are left out by default.'),
  path_denied: () => t('workspace.handoff.error.pathDenied', 'That path is blocked by your organisation and can never be sent.'),
  env_name_denied: () => t('workspace.handoff.error.envNameDenied', 'Your organisation does not allow sending that environment variable.'),
  not_signed_in: () => t('workspace.error.notSignedIn', 'Sign in again to run agents on this computer.'),
};

export function describeHandoffError(error: unknown): string {
  const { code, message } = toHandoffIpcError(error);
  const known = MESSAGES[code];
  if (known !== undefined) return known();
  return message === '' ? t('workspace.failed', 'That did not work. Try again.') : message;
}

/** A send stopped by the person (`handoff_cancelled`): not an error, the thread stays here. */
export function isCancelled(error: unknown): boolean {
  return error !== null && error !== undefined && toHandoffIpcError(error).code === 'handoff_cancelled';
}

/** The text for a send refused for size, without an error to read it from (the preview already knows). */
export function describeTooLarge(): string {
  return MESSAGES['handoff_too_large']?.() ?? '';
}

/** The secret findings a rejection carries (`secret_found`), or none. */
export function findingsOf(error: unknown): SecretFinding[] {
  const { code, details } = toHandoffIpcError(error);
  if (code !== 'secret_found' || typeof details !== 'object' || details === null) return [];
  const list = (details as { findings?: unknown }).findings;
  return Array.isArray(list) ? (list as SecretFinding[]) : [];
}

/** The cap breakdown a rejection carries (`handoff_too_large`), or undefined. */
export function capsOf(error: unknown): CapsBreakdown | undefined {
  const { code, details } = toHandoffIpcError(error);
  if (code !== 'handoff_too_large' || typeof details !== 'object' || details === null) return undefined;
  const { violations, largest } = details as Partial<CapsBreakdown>;
  return { violations: Array.isArray(violations) ? violations : [], largest: Array.isArray(largest) ? largest : [] };
}

/** The quota figures a `workspace_quota_exceeded` rejection carries, as `name: value` pairs of numbers. */
export function quotaOf(error: unknown): { name: string; value: number }[] {
  const { code, details } = toHandoffIpcError(error);
  if (code !== 'workspace_quota_exceeded' || typeof details !== 'object' || details === null) return [];
  return Object.entries(details as Record<string, unknown>)
    .filter((entry): entry is [string, number] => typeof entry[1] === 'number')
    .map(([name, value]) => ({ name, value }));
}
