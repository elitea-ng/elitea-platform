/**
 * The agent editor's Workforce tab (ADR-0050 decisions 5b and 5g-5m, issue
 * 1616): the agent's state on the dismissal ladder, its latest scorecard
 * and a 30-day trend, reinstatement of a retired agent, the proposals for
 * it and their decisions, and a Propose button for a manager or the owner.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import type { AgentScorecard, AgentWorkforceAgent, SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { t } from '@/shared/i18n';

import { agentStateLabel, canReinstate, workforceRefusal } from '../lib/workforce';
import { useAgentProposals, useScorecards, useWorkforceActions, useWorkforceAgent } from '../model/useAgentWorkforce';
import { ProposalRow } from './ProposalRow';
import { ProposeStageDialog } from './ProposeStageDialog';

export interface WorkforcePanelProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
}

function useCurrentUserId(): number | undefined {
  const author = useGetCurrentAuthor();
  const id = Number((author.data?.data as SocialAuthorProfile | undefined)?.id);
  return Number.isSafeInteger(id) && id > 0 ? id : undefined;
}

function percent(rate: number): string {
  return `${Math.round(rate * 1000) / 10}%`;
}

function ScorecardSummary({ card }: { readonly card: AgentScorecard }): ReactNode {
  const rows: readonly (readonly [string, string])[] = [
    [t('features.agentWorkforce.scorecard.runs', 'Runs (30 days)'), String(card.runs_30d)],
    [t('features.agentWorkforce.scorecard.failureRate', 'Failure rate'), percent(card.failure_rate_30d)],
    [t('features.agentWorkforce.scorecard.capReached', 'Budget-cap stops'), String(card.cap_reached_30d)],
    [t('features.agentWorkforce.scorecard.breakerTrips', 'Breaker trips (7 days)'), String(card.breaker_trips_7d)],
    [t('features.agentWorkforce.scorecard.rejections', 'Approval rejections'), String(card.approval_rejections_30d)],
    [t('features.agentWorkforce.scorecard.guardrails', 'Guardrail hits'), String(card.guardrail_hits_30d)],
    [
      t('features.agentWorkforce.scorecard.spend', 'Spend vs project median'),
      `$${card.spend_30d_usd.toFixed(2)} / $${card.project_median_spend_usd.toFixed(2)}`,
    ],
    [t('features.agentWorkforce.scorecard.feedback', 'Feedback'), `+${card.thumbs_up_30d} / -${card.thumbs_down_30d}`],
    [
      t('features.agentWorkforce.scorecard.unused', 'Days unused'),
      card.unused_days === null ? t('features.agentWorkforce.scorecard.neverUsed', 'never used') : String(card.unused_days),
    ],
    [t('features.agentWorkforce.scorecard.certification', 'Certification'), card.certification_state],
  ];
  return (
    <Box data-testid="agent-workforce-scorecard" sx={{ display: 'grid', gridTemplateColumns: 'max-content auto', columnGap: '1rem', rowGap: '0.25rem' }}>
      {rows.map(([label, value]) => (
        <Box key={label} sx={{ display: 'contents' }}>
          <Typography variant="bodySmall" color="text.secondary">
            {label}
          </Typography>
          <Typography variant="bodySmall">{value}</Typography>
        </Box>
      ))}
    </Box>
  );
}

function StateHeader({ projectId, state, onPropose }: { readonly projectId: string; readonly state: AgentWorkforceAgent; readonly onPropose: () => void }): ReactNode {
  const actions = useWorkforceActions(projectId, state.application_id);
  const [error, setError] = useState<string | undefined>(undefined);
  const mayPropose = (state.can_manage === true || state.is_owner === true) && state.lifecycle_state === 'active';
  const mayReinstate = state.can_manage === true && canReinstate(state, new Date());
  const reinstateNow = (): void => {
    setError(undefined);
    actions.reinstate.mutate('', {
      onError: (failure) => setError(workforceRefusal(failure, t('features.agentWorkforce.reinstateFailed', 'The agent could not be reinstated.')).message),
    });
  };
  return (
    <>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.75rem', flexWrap: 'wrap' }}>
        <Typography variant="headingSmall" data-testid="agent-workforce-state">
          {agentStateLabel(state)}
        </Typography>
        <StateBadges state={state} />
        {mayReinstate && (
          <Button size="small" variant="contained" data-testid="agent-workforce-reinstate" disabled={actions.reinstate.isPending} onClick={reinstateNow}>
            {t('features.agentWorkforce.action.reinstate', 'Reinstate')}
          </Button>
        )}
        {mayPropose && (
          <Button size="small" variant="outlined" onClick={onPropose} data-testid="agent-workforce-propose">
            {t('features.agentWorkforce.action.propose', 'Propose a stage')}
          </Button>
        )}
      </Box>
      {error !== undefined && (
        <Typography variant="bodySmall" color="error.main">
          {error}
        </Typography>
      )}
    </>
  );
}

function StateBadges({ state }: { readonly state: AgentWorkforceAgent }): ReactNode {
  const badges: readonly (readonly [boolean, string, string])[] = [
    [state.is_hr_agent, 'text.secondary', t('features.agentWorkforce.hrAgent', 'Reviewer (HR) agent')],
    [state.legal_hold, 'warning.main', t('features.agentWorkforce.legalHold', 'Legal hold')],
    [
      state.lifecycle_state === 'retired' && state.reinstate_until !== null,
      'text.secondary',
      t('features.agentWorkforce.reinstateUntil', 'Can be reinstated until {{date}}', {
        date: state.reinstate_until === null ? '' : new Date(state.reinstate_until).toLocaleDateString(),
      }),
    ],
  ];
  return (
    <>
      {badges
        .filter(([shown]) => shown)
        .map(([, color, label]) => (
          <Typography key={label} variant="bodySmall" color={color}>
            {label}
          </Typography>
        ))}
    </>
  );
}

function ScorecardSection({ card, trend }: { readonly card: AgentScorecard | null | undefined; readonly trend: readonly AgentScorecard[] }): ReactNode {
  return (
    <>
      {card !== null && card !== undefined ? (
        <ScorecardSummary card={card} />
      ) : (
        <Typography variant="bodySmall" color="text.secondary" data-testid="agent-workforce-no-scorecard">
          {t('features.agentWorkforce.noScorecard', 'No scorecard yet: it is computed once a day.')}
        </Typography>
      )}
      {trend.length > 1 && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="agent-workforce-trend">
          {t('features.agentWorkforce.trend', 'Failure rate over {{days}} days: {{rates}}', {
            days: trend.length,
            rates: [...trend].reverse().map((row) => percent(row.failure_rate_30d)).join(' → '),
          })}
        </Typography>
      )}
    </>
  );
}

function ProposalsSection({ projectId, state, callerId }: { readonly projectId: string; readonly state: AgentWorkforceAgent; readonly callerId: number | undefined }): ReactNode {
  const proposals = useAgentProposals(projectId, state.application_id, true);
  const rows = proposals.data?.rows ?? [];
  return (
    <Box>
      <Typography variant="labelMedium">{t('features.agentWorkforce.proposals', 'Proposals')}</Typography>
      {rows.length === 0 ? (
        <Typography variant="bodySmall" color="text.secondary">
          {t('features.agentWorkforce.noProposals', 'No proposals for this agent.')}
        </Typography>
      ) : (
        rows.map((proposal) => <ProposalRow key={proposal.id} projectId={projectId} agent={state} proposal={proposal} callerId={callerId} />)
      )}
    </Box>
  );
}

// The scorecards and the proposals are an agent manager's and the owner's
// (ADR-0050 5h); anyone else sees the agent's ladder state only.
function mayReadWorkforce(state: { can_manage?: boolean | undefined; is_owner?: boolean | undefined } | undefined): boolean {
  return state?.can_manage === true || state?.is_owner === true;
}

export function WorkforcePanel({ projectId, applicationId }: WorkforcePanelProps): ReactNode {
  const agent = useWorkforceAgent(projectId, applicationId);
  const mayRead = mayReadWorkforce(agent.data);
  const cards = useScorecards(projectId, applicationId, mayRead);
  const callerId = useCurrentUserId();
  const [proposing, setProposing] = useState(false);

  if (projectId === undefined || applicationId === undefined || applicationId <= 0) {
    return (
      <Typography variant="bodyMedium" data-testid="agent-workforce-unsaved">
        {t('features.agentWorkforce.unsaved', 'Save the agent first to see its scorecard.')}
      </Typography>
    );
  }
  if (agent.isError) {
    return (
      <Typography variant="bodyMedium" color="error.main" data-testid="agent-workforce-error">
        {workforceRefusal(agent.error, t('features.agentWorkforce.loadFailed', 'The workforce state could not be read.')).message}
      </Typography>
    );
  }
  const state = agent.data;
  if (state === undefined) return null;

  return (
    <Box data-testid="agent-workforce-panel" sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <StateHeader projectId={projectId} state={state} onPropose={() => setProposing(true)} />
      {mayRead && (
        <>
          <ScorecardSection card={state.scorecard} trend={cards.data?.rows ?? []} />
          <ProposalsSection projectId={projectId} state={state} callerId={callerId} />
        </>
      )}
      <ProposeStageDialog open={proposing} projectId={projectId} applicationId={applicationId} onClose={() => setProposing(false)} />
    </Box>
  );
}
