/**
 * The agent workforce (ADR-0050 decisions 5b and 5g-5m, issue 1616), over
 * the generated fetchers. Each call unwraps the transport envelope once, so
 * no caller reads a field of the envelope as if it were the answer (#132).
 */
import {
  appealAgentWorkforceProposal,
  approveAgentWorkforceProposal,
  askOwnerAgentWorkforceProposal,
  commentAgentWorkforceProposal,
  decideAgentWorkforceAppeal,
  getAgentWorkforce,
  getAgentWorkforceImpact,
  getAgentWorkforcePolicy,
  keepAgentAutoHire,
  listAgentAutomaticActions,
  objectAgentAutomaticAction,
  setAgentWorkforcePolicy,
  undoAgentAutoHire,
  listAgentScorecards,
  listAgentWorkforceProposals,
  proposeAgentWorkforceStage,
  rejectAgentWorkforceProposal,
  reinstateAgent,
} from '@/shared/api/generated/applications/applications';
import type {
  AgentAutomaticAction,
  AgentWorkforceAgent,
  AgentWorkforceDecisionBody,
  AgentWorkforceImpact,
  AgentWorkforcePolicy,
  AgentWorkforcePolicyInput,
  AgentWorkforceProposal,
  ListAgentAutomaticActions200,
  ListAgentAutomaticActionsParams,
  ListAgentScorecards200,
  ListAgentWorkforceProposals200,
  ListAgentWorkforceProposalsParams,
  ProposeAgentWorkforceStageBody,
} from '@/shared/api/generated/model';

function bodyOf<T>(envelope: unknown): T {
  return (envelope as { readonly data: T }).data;
}

/* manifest: agentWorkforce.agent */
export async function fetchWorkforceAgent(projectId: string, applicationId: number): Promise<AgentWorkforceAgent> {
  return bodyOf<AgentWorkforceAgent>(await getAgentWorkforce(projectId, applicationId));
}

/* manifest: agentWorkforce.scorecards */
export async function fetchScorecards(projectId: string, applicationId: number, days: number): Promise<ListAgentScorecards200> {
  return bodyOf<ListAgentScorecards200>(await listAgentScorecards(projectId, applicationId, { days }));
}

/* manifest: agentWorkforce.impact */
export async function fetchImpact(projectId: string, applicationId: number): Promise<AgentWorkforceImpact> {
  return bodyOf<AgentWorkforceImpact>(await getAgentWorkforceImpact(projectId, applicationId));
}

/* manifest: agentWorkforce.reinstate */
export async function reinstate(projectId: string, applicationId: number, note: string): Promise<AgentWorkforceAgent> {
  return bodyOf<AgentWorkforceAgent>(await reinstateAgent(projectId, applicationId, note === '' ? {} : { note }));
}

/* manifest: agentWorkforce.list */
export async function fetchProposals(projectId: string, params: ListAgentWorkforceProposalsParams): Promise<ListAgentWorkforceProposals200> {
  return bodyOf<ListAgentWorkforceProposals200>(await listAgentWorkforceProposals(projectId, params));
}

/* manifest: agentWorkforce.propose */
export async function propose(projectId: string, body: ProposeAgentWorkforceStageBody): Promise<AgentWorkforceProposal> {
  return bodyOf<AgentWorkforceProposal>(await proposeAgentWorkforceStage(projectId, body));
}

/** The three decisions that take a note (and, for approve, the nesting confirmation). */
export type WorkforceDecision = 'approve' | 'reject' | 'ask_owner';

/*
 * manifest: agentWorkforce.approve
 * manifest: agentWorkforce.reject
 * manifest: agentWorkforce.askOwner
 */
export async function decide(projectId: string, proposalId: number, decision: WorkforceDecision, body: AgentWorkforceDecisionBody): Promise<AgentWorkforceProposal> {
  const call =
    decision === 'approve'
      ? approveAgentWorkforceProposal
      : decision === 'reject'
        ? rejectAgentWorkforceProposal
        : askOwnerAgentWorkforceProposal;
  return bodyOf<AgentWorkforceProposal>(await call(projectId, proposalId, body));
}

/* manifest: agentWorkforce.comment */
export async function comment(projectId: string, proposalId: number, text: string): Promise<AgentWorkforceProposal> {
  return bodyOf<AgentWorkforceProposal>(await commentAgentWorkforceProposal(projectId, proposalId, { text }));
}

/* manifest: agentWorkforce.appeal */
export async function appeal(projectId: string, proposalId: number, text: string): Promise<AgentWorkforceProposal> {
  return bodyOf<AgentWorkforceProposal>(await appealAgentWorkforceProposal(projectId, proposalId, { text }));
}

/* manifest: agentWorkforce.decideAppeal */
export async function decideAppeal(projectId: string, proposalId: number, grant: boolean, note: string): Promise<AgentWorkforceProposal> {
  return bodyOf<AgentWorkforceProposal>(await decideAgentWorkforceAppeal(projectId, proposalId, note === '' ? { grant } : { grant, note }));
}

/* manifest: agentWorkforce.policy */
export async function fetchPolicy(projectId: string): Promise<AgentWorkforcePolicy> {
  return bodyOf<AgentWorkforcePolicy>(await getAgentWorkforcePolicy(projectId));
}

/* manifest: agentWorkforce.setPolicy */
export async function savePolicy(projectId: string, body: AgentWorkforcePolicyInput): Promise<AgentWorkforcePolicy> {
  return bodyOf<AgentWorkforcePolicy>(await setAgentWorkforcePolicy(projectId, body));
}

/* manifest: agentWorkforce.automaticActions */
export async function fetchAutomaticActions(projectId: string, params: ListAgentAutomaticActionsParams): Promise<ListAgentAutomaticActions200> {
  return bodyOf<ListAgentAutomaticActions200>(await listAgentAutomaticActions(projectId, params));
}

/** What a person can do with an automatic action. */
export type AutomaticActionDecision = 'undo' | 'keep' | 'object';

/*
 * manifest: agentWorkforce.undoAutoHire
 * manifest: agentWorkforce.keepAutoHire
 * manifest: agentWorkforce.objectAction
 */
export async function decideAutomaticAction(
  projectId: string,
  actionId: number,
  decision: AutomaticActionDecision,
  body: AgentWorkforceDecisionBody,
): Promise<AgentAutomaticAction> {
  const call = decision === 'undo' ? undoAgentAutoHire : decision === 'keep' ? keepAgentAutoHire : objectAgentAutomaticAction;
  return bodyOf<AgentAutomaticAction>(await call(projectId, actionId, body));
}
