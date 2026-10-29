import { t } from '@/shared/i18n';

/**
 * Display names for the governance enums. The wire values stay the keys; an
 * unknown value (a newer server) shows as itself rather than as nothing.
 */

export const RISK_TIERS = ['low', 'medium', 'high', 'critical'] as const;

export function tierLabel(tier: string): string {
  switch (tier) {
    case 'low':
      return t('features.agentLifecycle.tier.low', 'Low');
    case 'medium':
      return t('features.agentLifecycle.tier.medium', 'Medium');
    case 'high':
      return t('features.agentLifecycle.tier.high', 'High');
    case 'critical':
      return t('features.agentLifecycle.tier.critical', 'Critical');
    default:
      return tier;
  }
}

/** True for a tier whose release goes through a change request. */
export function tierNeedsChangeRequest(tier: string | undefined): boolean {
  return tier !== undefined && tier !== 'low';
}

export function certificationStateLabel(state: string): string {
  switch (state) {
    case 'uncertified':
      return t('features.agentLifecycle.certification.uncertified', 'Uncertified');
    case 'certified':
      return t('features.agentLifecycle.certification.certified', 'Certified');
    case 'due':
      return t('features.agentLifecycle.certification.due', 'Recertification due');
    case 'lapsed':
      return t('features.agentLifecycle.certification.lapsed', 'Lapsed');
    case 'suspended':
      return t('features.agentLifecycle.certification.suspended', 'Suspended (lapsed)');
    default:
      return state;
  }
}

export function changeRequestStateLabel(state: string): string {
  switch (state) {
    case 'draft':
      return t('features.agentLifecycle.changeRequest.state.draft', 'Draft');
    case 'submitted':
      return t('features.agentLifecycle.changeRequest.state.submitted', 'Submitted');
    case 'approved':
      return t('features.agentLifecycle.changeRequest.state.approved', 'Approved');
    case 'rejected':
      return t('features.agentLifecycle.changeRequest.state.rejected', 'Rejected');
    case 'withdrawn':
      return t('features.agentLifecycle.changeRequest.state.withdrawn', 'Withdrawn');
    case 'published':
      return t('features.agentLifecycle.changeRequest.state.published', 'Published');
    default:
      return state;
  }
}

export function changeRequestKindLabel(kind: string): string {
  switch (kind) {
    case 'release':
      return t('features.agentLifecycle.changeRequest.kind.release', 'Release');
    case 'recertification':
      return t('features.agentLifecycle.changeRequest.kind.recertification', 'Recertification');
    case 'legacy_unreviewed':
      return t('features.agentLifecycle.changeRequest.kind.legacy', 'Earlier publish (not reviewed)');
    default:
      return kind;
  }
}

export const ATTESTATION_KINDS = ['architecture', 'legal', 'model_risk', 'data_owner', 'data_accuracy', 'custom'] as const;
export type AttestationKind = (typeof ATTESTATION_KINDS)[number];

export function attestationKindLabel(kind: string): string {
  switch (kind) {
    case 'architecture':
      return t('features.agentLifecycle.attestation.kind.architecture', 'Architecture review');
    case 'legal':
      return t('features.agentLifecycle.attestation.kind.legal', 'Legal review');
    case 'model_risk':
      return t('features.agentLifecycle.attestation.kind.modelRisk', 'Model risk review');
    case 'data_owner':
      return t('features.agentLifecycle.attestation.kind.dataOwner', 'Data owner sign-off');
    case 'data_accuracy':
      return t('features.agentLifecycle.attestation.kind.dataAccuracy', 'Data accuracy review');
    case 'custom':
      return t('features.agentLifecycle.attestation.kind.custom', 'Custom');
    default:
      return kind;
  }
}

/** A nullable timestamp, as one cell. */
export function formatWhen(value: string | null | undefined): string {
  return value === null || value === undefined || value === '' ? '—' : new Date(value).toLocaleString();
}

/** A nullable user id, as one cell. */
export function formatUser(id: number | null | undefined): string {
  return id === null || id === undefined ? '—' : t('features.agentLifecycle.user', 'user {{id}}', { id });
}
