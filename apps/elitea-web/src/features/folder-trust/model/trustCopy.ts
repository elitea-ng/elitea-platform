/**
 * The trust dialog's fixed product copy (ADR-0061 decision 5a). The four
 * statements are the ADR's wording, kept in one key group
 * (`desktop.trust.v1.*`); `TRUST_CONFIRMATION_VERSION` is the version of this
 * text that `trust_grant` sends and the host records in its ledger. Changing a
 * statement means new keys and a new version: the host refuses an outdated
 * one (`confirmation_outdated`).
 */
import type { TrustPolicyBlock, TrustStatus } from '@/shared/desktop/trustIpc';
import { t } from '@/shared/i18n';

export const TRUST_CONFIRMATION_VERSION = 1;

/** The four statements, in the ADR's order. */
export const trustStatements = (): string[] => [
  t('desktop.trust.v1.files', 'Commands will be able to read and change any file your user account can, including keys, cloud logins and this app’s own data.'),
  t('desktop.trust.v1.network', 'They can reach any network host.'),
  t('desktop.trust.v1.steer', 'Content in this folder, and anything an agent reads, can steer what the agent asks to run.'),
  t('desktop.trust.v1.advice', 'Trust only folders whose content you trust, and keep approvals on for anything you would not run yourself.'),
];

/** Why the organisation's policy does not let this folder be trusted, one line per reason. */
export function blockReason(block: TrustPolicyBlock): string {
  switch (block) {
    case 'local_work':
      return t('desktop.trust.blockLocalWork', 'Your organisation has turned Local work off.');
    case 'shell':
      return t('desktop.trust.blockShell', 'Your organisation does not let agents run commands.');
    case 'network':
      return t('desktop.trust.blockNetwork', 'Your organisation does not let commands reach the network.');
    case 'ceiling':
      return t('desktop.trust.blockCeiling', 'Your organisation keeps every command inside the sandbox (its ceiling is below full access).');
    case 'folder_trust':
      return t('desktop.trust.blockFolderTrust', 'Your organisation has turned trusted folders off.');
  }
}

/** Why a folder's trust is paused: by the organisation, or a named file changed outside the app. */
export function pausedReason(status: Pick<TrustStatus, 'suspended_by' | 'tampered_file'>): string {
  if (status.suspended_by === 'tamper') {
    return t('desktop.trust.pausedTamper', 'Trust paused: {{file}} changed outside the app. Confirm again to resume.', { file: status.tampered_file ?? '' });
  }
  return t('desktop.trust.pausedPolicy', 'Trust paused by your organisation');
}

/** A host time ("2026-11-25T10:00:00Z") as a local date and time, or "Never". */
export function whenText(value: string | null): string {
  if (value === null) return t('desktop.trust.never', 'Never');
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

export function platformName(platform: TrustStatus['platform']): string {
  if (platform === 'macos') return 'macOS';
  if (platform === 'windows') return 'Windows';
  return 'Linux';
}
