/**
 * Pure helpers for the workforce panel: labels, the server's refusal, what
 * the caller may do with a proposal, and the narrowing a demotion form
 * builds. Kept apart from the UI so the rules are unit-tested.
 */
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type { AgentWorkforceAgent, AgentWorkforceNarrowing, AgentWorkforceProposal } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

/** The ladder stages, in order. A person may also propose delete. */
export const STAGES = ['watch', 'demote', 'suspend', 'retire', 'delete'] as const;
export type Stage = (typeof STAGES)[number];

export function stageLabel(stage: string): string {
  switch (stage) {
    case 'watch':
      return t('features.agentWorkforce.stage.watch', 'Watch');
    case 'demote':
      return t('features.agentWorkforce.stage.demote', 'Demote');
    case 'suspend':
      return t('features.agentWorkforce.stage.suspend', 'Suspend');
    case 'retire':
      return t('features.agentWorkforce.stage.retire', 'Retire');
    case 'delete':
      return t('features.agentWorkforce.stage.delete', 'Delete');
    default:
      return stage;
  }
}

export function statusLabel(status: string): string {
  switch (status) {
    case 'proposed':
      return t('features.agentWorkforce.status.proposed', 'Proposed');
    case 'asked_owner':
      return t('features.agentWorkforce.status.askedOwner', 'Waiting for the owner');
    case 'approved':
      return t('features.agentWorkforce.status.approved', 'Approved');
    case 'rejected':
      return t('features.agentWorkforce.status.rejected', 'Rejected');
    default:
      return status;
  }
}

/** The agent's state in one phrase: the most severe that holds. */
export function agentStateLabel(agent: AgentWorkforceAgent): string {
  if (agent.lifecycle_state === 'deleted') return t('features.agentWorkforce.state.deleted', 'Deleted');
  if (agent.lifecycle_state === 'retired') return t('features.agentWorkforce.state.retired', 'Retired');
  if (agent.suspension_id > 0) return t('features.agentWorkforce.state.suspended', 'Suspended');
  if (agent.under_review) return t('features.agentWorkforce.state.watch', 'Under review');
  return t('features.agentWorkforce.state.active', 'Active');
}

/** Whether a retired agent can still be reinstated, at `now`. */
export function canReinstate(agent: AgentWorkforceAgent, now: Date): boolean {
  if (agent.lifecycle_state !== 'retired' || agent.reinstate_until === null) return false;
  return new Date(agent.reinstate_until).getTime() > now.getTime();
}

function isOpen(proposal: Pick<AgentWorkforceProposal, 'status'>): boolean {
  return proposal.status === 'proposed' || proposal.status === 'asked_owner';
}

export interface ProposalAbilities {
  readonly decide: boolean;
  readonly askOwner: boolean;
  readonly comment: boolean;
  readonly appeal: boolean;
  readonly decideAppeal: boolean;
}

function appealable(proposal: AgentWorkforceProposal): boolean {
  return proposal.status === 'approved' && proposal.appeal_state === null && proposal.stage !== 'delete' && proposal.stage !== 'watch';
}

/** A manager other than the one who decided, and not the owner, reviews an open appeal. */
function mayReviewAppeal(proposal: AgentWorkforceProposal, manager: boolean, owner: boolean, callerId: number | undefined): boolean {
  if (proposal.appeal_state !== 'open' || !manager || owner || callerId === undefined) return false;
  return proposal.decided_by_user_id !== callerId;
}

/**
 * What the caller may do with a proposal, mirroring the server's rules (the
 * server decides; this only hides buttons that would be refused).
 */
export function abilitiesFor(proposal: AgentWorkforceProposal, agent: AgentWorkforceAgent, callerId: number | undefined): ProposalAbilities {
  const manager = agent.can_manage === true;
  const owner = agent.is_owner === true;
  const open = isOpen(proposal);
  return {
    decide: open && (manager || owner),
    askOwner: open && manager && proposal.status === 'proposed',
    comment: open && owner && proposal.status === 'asked_owner',
    appeal: owner && appealable(proposal),
    decideAppeal: mayReviewAppeal(proposal, manager, owner, callerId),
  };
}

/** The server's refusal: its message and code, when it gave them. */
export function workforceRefusal(error: unknown, fallback: string): { readonly message: string; readonly code: string | undefined } {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const { body } = error.failure;
    if (typeof body === 'object' && body !== null) {
      const record = body as Record<string, unknown>;
      const message = typeof record['message'] === 'string' && record['message'] !== '' ? record['message'] : fallback;
      const code = typeof record['code'] === 'string' ? record['code'] : undefined;
      return { message, code };
    }
  }
  return { message: fallback, code: undefined };
}

export interface DemotionForm {
  readonly removeToolkitIds: string;
  readonly memoryReview: boolean;
  readonly proactivityOff: boolean;
  readonly removeLead: boolean;
  readonly removeProposeAgents: boolean;
  readonly removeReviewAgents: boolean;
}

export const EMPTY_DEMOTION: DemotionForm = {
  removeToolkitIds: '',
  memoryReview: false,
  proactivityOff: false,
  removeLead: false,
  removeProposeAgents: false,
  removeReviewAgents: false,
};

/** The narrowing a demotion form names, or undefined when it names nothing or a bad id. */
export function narrowingOf(form: DemotionForm): AgentWorkforceNarrowing | undefined {
  const ids = form.removeToolkitIds
    .split(',')
    .map((part) => part.trim())
    .filter((part) => part !== '');
  if (ids.some((id) => !/^[1-9]\d*$/.test(id))) return undefined;
  const capabilities = [
    ...(form.removeProposeAgents ? ['can_propose_agents'] : []),
    ...(form.removeReviewAgents ? ['can_review_agents'] : []),
  ];
  const narrowing: AgentWorkforceNarrowing = {
    ...(ids.length > 0 ? { remove_toolkit_ids: ids.map(Number) } : {}),
    ...(form.memoryReview ? { memory_review: true } : {}),
    ...(form.proactivityOff ? { proactivity_off: true } : {}),
    ...(form.removeLead ? { remove_lead: true } : {}),
    ...(capabilities.length > 0 ? { remove_capabilities: capabilities } : {}),
  };
  return Object.keys(narrowing).length === 0 ? undefined : narrowing;
}
