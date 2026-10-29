import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';

/**
 * The typed refusals of agent lifecycle governance (ADR-0040, #1497).
 *
 * The lifecycle routes answer `{error, code, message, failures?, missing?}`,
 * and a direct publish of a tier that needs approval answers the same shape
 * with 409 `change_request_required`. `error` repeats the CODE, so showing it
 * as text would print `change_request_required` to the author. Every code
 * below gets a sentence the reader can act on instead.
 */
export interface GovernanceRefusal {
  readonly status: number;
  readonly code: string;
  readonly message: string;
  readonly failures: readonly string[];
  readonly missing: readonly string[];
  readonly riskTier?: string;
  readonly requiredApprovals?: number;
}

function strings(value: unknown): readonly string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string') : [];
}

function text(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

/** The refusal an error carries, or undefined when it carries none. */
export function readGovernanceRefusal(error: unknown): GovernanceRefusal | undefined {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return undefined;
  const body = error.failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const record = body as Record<string, unknown>;
  const code = text(record['code']) || text(record['error']);
  if (code === '' || !(code in MESSAGES)) return undefined;
  const tier = record['risk_tier'];
  const approvals = record['required_approvals'];
  return {
    status: error.failure.status,
    code,
    message: text(record['message']),
    failures: strings(record['failures']),
    missing: strings(record['missing']),
    ...(typeof tier === 'string' ? { riskTier: tier } : {}),
    ...(typeof approvals === 'number' ? { requiredApprovals: approvals } : {}),
  };
}

/** True when a direct publish was refused because the tier needs a change request. */
export function isChangeRequestRequired(error: unknown): boolean {
  return readGovernanceRefusal(error)?.code === 'change_request_required';
}

const MESSAGES: Readonly<Record<string, () => string>> = {
  change_request_required: () =>
    t(
      'features.agentLifecycle.refusal.changeRequestRequired',
      'This agent’s risk tier needs an approved change request before it is published. Request publish instead.',
    ),
  change_request_not_required: () =>
    t(
      'features.agentLifecycle.refusal.changeRequestNotRequired',
      'This agent’s risk tier needs no approval. Publish it directly.',
    ),
  self_approval_refused: () =>
    t('features.agentLifecycle.refusal.selfApproval', 'You proposed this change, so someone else must approve it.'),
  approver_role_required: () =>
    t(
      'features.agentLifecycle.refusal.approverRole',
      'At this risk tier an approval must come from someone with the governance role.',
    ),
  change_request_proposer_only: () =>
    t('features.agentLifecycle.refusal.proposerOnly', 'Only the person who proposed this change can do that.'),
  registry_owner_required: () =>
    t(
      'features.agentLifecycle.refusal.ownerRequired',
      'Only an owner of this agent or someone with the governance role can edit its registry record.',
    ),
  tier_lowering_requires_governance: () =>
    t('features.agentLifecycle.refusal.tierLowering', 'Lowering the risk tier needs the governance role.'),
  governance_role_required: () =>
    t('features.agentLifecycle.refusal.governanceRole', 'This action needs the governance role.'),
  change_request_already_approved: () =>
    t('features.agentLifecycle.refusal.alreadyApproved', 'You have already approved this change request.'),
  release_gate_pending: () =>
    t(
      'features.agentLifecycle.refusal.gatePending',
      'The release gate is still evaluating. Approve once it has passed.',
    ),
  release_gate_failed: () =>
    t(
      'features.agentLifecycle.refusal.gateFailed',
      'The release gate failed. Fix the agent, or ask someone with the governance role to override the gate.',
    ),
  release_gate_not_overridable: () =>
    t(
      'features.agentLifecycle.refusal.gateNotOverridable',
      'Only a failed gate that has not been overridden can be overridden.',
    ),
  attestation_missing: () =>
    t(
      'features.agentLifecycle.refusal.attestationMissing',
      'A required attestation is missing, expired or revoked. Record it before approving.',
    ),
  change_request_state_conflict: () =>
    t(
      'features.agentLifecycle.refusal.stateConflict',
      'The change request is no longer in a state that allows this. Reload it and try again.',
    ),
  change_request_open: () =>
    t(
      'features.agentLifecycle.refusal.open',
      'This agent already has an open change request. Finish or withdraw it first (Governance › Change requests).',
    ),
  change_request_stale: () =>
    t(
      'features.agentLifecycle.refusal.stale',
      'The version changed after this request was submitted. Withdraw it and request publish again.',
    ),
  agent_not_certifiable: () =>
    t(
      'features.agentLifecycle.refusal.notCertifiable',
      'This agent has no published release to certify, or it is not certified yet.',
    ),
  agent_certification_lapsed: () =>
    t(
      'features.agentLifecycle.refusal.lapsed',
      'This agent’s certification has lapsed, so its runs are refused until it is recertified.',
    ),
};

/** The sentence for a refusal, with its detail list when the code carries one. */
export function governanceRefusalMessage(refusal: GovernanceRefusal): string {
  const sentence = MESSAGES[refusal.code]?.() ?? refusal.message;
  const detail = refusal.code === 'attestation_missing' ? refusal.missing : refusal.failures;
  return detail.length === 0 ? sentence : `${sentence} (${detail.join(', ')})`;
}
