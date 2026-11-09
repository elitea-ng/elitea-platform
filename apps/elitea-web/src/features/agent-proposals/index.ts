/**
 * Agent proposals (ADR-0050 decision 5a, issue 1615): the chat's proposal
 * card, the agents registry's inbox and the review dialog; and Copy to
 * project for a personal agent (decision 1). A slice of its own: the agents
 * barrel is already over its budget.
 */
export { ProposalCards } from './ui/ProposalCards';
export type { ProposalStepBodyHook } from './ui/ProposalCards';
export { ProposalsInboxButton } from './ui/ProposalsInboxButton';
export { CopyToProjectButton } from './ui/CopyToProjectButton';
export type { CopyTargetProject } from './ui/CopyToProjectButton';
export type { CreatedAgent } from './model/useAgentProposals';
export type { ProposeToolAction } from './lib/proposedAgents';
