/**
 * One workforce proposal with the actions the caller may take: approve
 * (after the impact preview; retiring a nested agent needs a confirmation),
 * reject, ask the owner; the owner's comment and appeal; and the appeal's
 * review by a different manager.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import type { AgentWorkforceAgent, AgentWorkforceProposal } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { abilitiesFor, stageLabel, statusLabel, workforceRefusal, type ProposalAbilities } from '../lib/workforce';
import { useWorkforceActions } from '../model/useAgentWorkforce';
import { ApproveDialog, OwnerTextDialog, type OwnerTextAction } from './ProposalDialogs';

export interface ProposalRowProps {
  readonly projectId: string;
  readonly agent: AgentWorkforceAgent;
  readonly proposal: AgentWorkforceProposal;
  readonly callerId: number | undefined;
}

function ProposalSummary({ proposal }: { readonly proposal: AgentWorkforceProposal }): ReactNode {
  const by =
    proposal.source === 'hr_agent'
      ? t('features.agentWorkforce.proposal.byAgent', 'Proposed by a reviewer agent: {{rationale}}', { rationale: proposal.rationale })
      : t('features.agentWorkforce.proposal.byPerson', 'Proposed: {{rationale}}', { rationale: proposal.rationale });
  return (
    <>
      <Typography variant="labelMedium">
        {t('features.agentWorkforce.proposal.heading', '{{stage}} — {{status}}', { stage: stageLabel(proposal.stage), status: statusLabel(proposal.status) })}
      </Typography>
      <Typography variant="bodySmall" sx={{ overflowWrap: 'anywhere' }}>
        {by}
      </Typography>
      {proposal.owner_comment !== null && (
        <Typography variant="bodySmall" color="text.secondary">
          {t('features.agentWorkforce.proposal.ownerComment', 'Owner: {{comment}}', { comment: proposal.owner_comment })}
        </Typography>
      )}
      {proposal.appeal_state !== null && (
        <Typography variant="bodySmall" color="text.secondary" data-testid={`agent-workforce-appeal-${proposal.id}`}>
          {t('features.agentWorkforce.proposal.appeal', 'Appeal ({{state}}): {{reason}}', { state: proposal.appeal_state, reason: proposal.appeal_reason ?? '' })}
        </Typography>
      )}
    </>
  );
}

interface ActionBarProps {
  readonly id: number;
  readonly abilities: ProposalAbilities;
  readonly onApprove: () => void;
  readonly onReject: () => void;
  readonly onAskOwner: () => void;
  readonly onOwnerText: (action: OwnerTextAction) => void;
  readonly onAppealDecision: (grant: boolean) => void;
}

interface ActionSpec {
  readonly shown: boolean;
  readonly testId: string;
  readonly label: string;
  readonly onClick: () => void;
  readonly primary?: boolean;
}

function ActionBar({ id, abilities, onApprove, onReject, onAskOwner, onOwnerText, onAppealDecision }: ActionBarProps): ReactNode {
  const specs: readonly ActionSpec[] = [
    { shown: abilities.decide, testId: 'approve', label: t('features.agentWorkforce.action.approve', 'Approve'), onClick: onApprove, primary: true },
    { shown: abilities.decide, testId: 'reject', label: t('features.agentWorkforce.action.reject', 'Reject'), onClick: onReject },
    { shown: abilities.askOwner, testId: 'ask-owner', label: t('features.agentWorkforce.action.askOwner', 'Ask the owner'), onClick: onAskOwner },
    { shown: abilities.comment, testId: 'comment', label: t('features.agentWorkforce.action.comment', 'Comment'), onClick: () => onOwnerText('comment') },
    { shown: abilities.appeal, testId: 'appeal-button', label: t('features.agentWorkforce.action.appeal', 'Appeal'), onClick: () => onOwnerText('appeal') },
    { shown: abilities.decideAppeal, testId: 'grant', label: t('features.agentWorkforce.action.grant', 'Grant the appeal'), onClick: () => onAppealDecision(true) },
    { shown: abilities.decideAppeal, testId: 'uphold', label: t('features.agentWorkforce.action.uphold', 'Uphold'), onClick: () => onAppealDecision(false) },
  ];
  return (
    <Box sx={{ display: 'flex', gap: '0.5rem', flexWrap: 'wrap' }}>
      {specs
        .filter((spec) => spec.shown)
        .map((spec) => (
          <Button
            key={spec.testId}
            size="small"
            variant={spec.primary === true ? 'contained' : 'text'}
            onClick={spec.onClick}
            data-testid={`agent-workforce-${spec.testId}-${id}`}
          >
            {spec.label}
          </Button>
        ))}
    </Box>
  );
}

export function ProposalRow({ projectId, agent, proposal, callerId }: ProposalRowProps): ReactNode {
  const actions = useWorkforceActions(projectId, agent.application_id);
  const [approving, setApproving] = useState(false);
  const [textAction, setTextAction] = useState<OwnerTextAction | undefined>(undefined);
  const [error, setError] = useState<string | undefined>(undefined);
  const failed = (failure: unknown): void =>
    setError(workforceRefusal(failure, t('features.agentWorkforce.decision.failed', 'That did not go through.')).message);
  const simpleDecision = (decision: 'reject' | 'ask_owner'): void =>
    actions.decide.mutate({ proposalId: proposal.id, decision, note: '', confirmNested: false }, { onError: failed });

  return (
    <Box
      data-testid={`agent-workforce-proposal-${proposal.id}`}
      sx={{ display: 'flex', flexDirection: 'column', gap: '0.25rem', py: '0.5rem', borderBottom: 1, borderColor: 'divider' }}
    >
      <ProposalSummary proposal={proposal} />
      <ActionBar
        id={proposal.id}
        abilities={abilitiesFor(proposal, agent, callerId)}
        onApprove={() => setApproving(true)}
        onReject={() => simpleDecision('reject')}
        onAskOwner={() => simpleDecision('ask_owner')}
        onOwnerText={setTextAction}
        onAppealDecision={(grant) => actions.decideAppeal.mutate({ proposalId: proposal.id, grant, note: '' }, { onError: failed })}
      />
      {error !== undefined && (
        <Typography variant="bodySmall" color="error.main" data-testid={`agent-workforce-error-${proposal.id}`}>
          {error}
        </Typography>
      )}
      <ApproveDialog open={approving} projectId={projectId} agent={agent} proposal={proposal} onClose={() => setApproving(false)} />
      <OwnerTextDialog projectId={projectId} agent={agent} proposal={proposal} action={textAction} onClose={() => setTextAction(undefined)} />
    </Box>
  );
}
