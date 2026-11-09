/**
 * "Mira proposes saving a sub-agent" under an answer whose agent called
 * `propose_agent` (ADR-0050 decision 5a), with Review.
 *
 * WHERE THE PROPOSAL COMES FROM. The answer's own `propose_agent` tool calls:
 * each completed call whose output names a `proposal_id` is one card. A
 * reloaded trace step carries no body; it is fetched once through the chat's
 * step-body loader, injected so this slice does not import the chat.
 *
 * WHAT IT SHOWS is the proposal as the server holds it now, so a reload
 * shows a proposal already created or dismissed (from the inbox, or by
 * someone else) as decided. A 404 (another person's proposal) shows nothing.
 */
import type { ReactNode } from 'react';
import { useMemo, useState } from 'react';

import LightbulbOutlinedIcon from '@mui/icons-material/LightbulbOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { proposalIdOf, proposeActions, type ProposeToolAction } from '../lib/proposedAgents';
import { useProposal } from '../model/useAgentProposals';
import { proposalName } from './ProposalsInboxButton';
import { ProposalReviewDialog } from './ProposalReviewDialog';

/** Loads a reloaded step's body (`useTraceStepDetail`), injected by the chat. */
export type ProposalStepBodyHook = (
  step: { readonly traceStepId?: number; readonly traceMessageGroupId?: number } | undefined,
  active: boolean,
) => { readonly output?: unknown } | undefined;

export interface ProposalCardsProps {
  readonly toolActions: readonly ProposeToolAction[] | undefined;
  readonly projectId: string | undefined;
  /** The proposing agent's name as the chat shows it. */
  readonly agentName: string | undefined;
  readonly useStepBody: ProposalStepBodyHook;
}

const cardSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexWrap: 'wrap',
  alignItems: 'center',
  gap: '0.5rem',
  padding: '0.5rem 0.75rem',
  marginBottom: '0.75rem',
  borderRadius: theme.vars.shape.radiusMd,
  background: theme.vars.palette.background.secondary,
  border: `0.0625rem solid ${theme.vars.palette.border.cardsOutlines}`,
});

export function ProposalCards({ toolActions, projectId, agentName, useStepBody }: ProposalCardsProps): ReactNode {
  const actions = useMemo(() => proposeActions(toolActions), [toolActions]);
  if (projectId === undefined || projectId === '' || actions.length === 0) return null;
  return (
    <>
      {actions.map((action, index) => (
        <ProposalCallCard
          key={typeof action.id === 'string' ? action.id : index}
          action={action}
          projectId={projectId}
          agentName={agentName}
          useStepBody={useStepBody}
        />
      ))}
    </>
  );
}

interface ProposalCallCardProps {
  readonly action: ProposeToolAction;
  readonly projectId: string;
  readonly agentName: string | undefined;
  readonly useStepBody: ProposalStepBodyHook;
}

function ProposalCallCard({ action, projectId, agentName, useStepBody }: ProposalCallCardProps): ReactNode {
  const needsFetch = !action.toolOutputs && !action.content && action.traceStepId !== undefined;
  const fetched = useStepBody(action, needsFetch);
  const proposalId = proposalIdOf(fetched?.output ?? action.toolOutputs ?? action.content);
  if (proposalId === undefined) return null;
  return <ProposalCard proposalId={proposalId} projectId={projectId} agentName={agentName} />;
}

function ProposalCard({ proposalId, projectId, agentName }: { readonly proposalId: number; readonly projectId: string; readonly agentName: string | undefined }): ReactNode {
  const proposal = useProposal(projectId, proposalId);
  const [reviewing, setReviewing] = useState(false);
  const testId = `agent-proposal-card-${proposalId}`;
  if (proposal.data === undefined) return null;
  const name = agentName ?? proposal.data.proposer_name;
  const status = proposal.data.status;
  return (
    <Box sx={cardSx} data-testid={testId} data-state={status}>
      <LightbulbOutlinedIcon fontSize="small" color="action" />
      <Typography variant="bodySmall" sx={{ flex: '1 1 12rem', minWidth: 0, overflowWrap: 'anywhere' }} data-testid={`${testId}-text`}>
        {cardText(name, proposalName(proposal.data), status)}
      </Typography>
      {status === 'proposed' && (
        <Button size="small" onClick={() => setReviewing(true)} data-testid={`${testId}-review`}>
          {t('features.agentProposals.inbox.review', 'Review')}
        </Button>
      )}
      <ProposalReviewDialog projectId={projectId} proposal={reviewing ? proposal.data : undefined} onClose={() => setReviewing(false)} />
    </Box>
  );
}

function cardText(name: string, agent: string, status: string): string {
  const text = t('features.agentProposals.card.proposed', '{{name}} proposes saving a sub-agent as a new agent: {{agent}}', { name, agent });
  if (status === 'created') return `${text} (${t('features.agentProposals.card.created', 'created')})`;
  if (status === 'dismissed') return `${text} (${t('features.agentProposals.card.dismissed', 'dismissed')})`;
  return text;
}
