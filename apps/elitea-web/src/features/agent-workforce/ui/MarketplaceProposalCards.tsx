/**
 * The two cards of a workforce manager's Marketplace proposals (ADR-0051
 * decision 6): a hire proposal opens the hire wizard filled in (the person
 * clicks Hire; nothing is created before), a drafted report is edited and
 * published as "Workforce report from a hiring project", or discarded.
 *
 * Text an agent wrote (the rationale, the draft) is untrusted: it is shown as
 * plain text in these cards and never interpreted.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { AgentWorkforceProposal } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { hireView, REPORT_TEXT_MAX, reportView } from '../lib/marketplaceProposal';
import { stageLabel, workforceRefusal } from '../lib/workforce';
import { useMarketplaceProposalActions } from '../model/useMarketplaceProposals';

const failed = (): string => t('features.agentWorkforce.decision.failed', 'That did not go through.');

const cardSx = { display: 'flex', flexDirection: 'column', gap: '0.5rem', border: '1px solid', borderColor: 'divider', borderRadius: 1, p: '0.75rem' } as const;

function figure(label: string, value: number | undefined, digits = 1): string {
  return value === undefined ? '' : `${label} ${value.toFixed(digits)}`;
}

function Evidence({ proposal }: { readonly proposal: AgentWorkforceProposal }): ReactNode {
  const view = hireView(proposal);
  const parts = [
    view.reviews === undefined ? '' : t('features.agentWorkforce.marketplace.reviews', '{{count}} reviews', { count: view.reviews }),
    figure(t('features.agentWorkforce.marketplace.rating', 'rating'), view.rating),
    figure(t('features.agentWorkforce.marketplace.dismissal', 'dismissal rate'), view.dismissalRate, 2),
    view.hires === undefined ? '' : t('features.agentWorkforce.marketplace.hires', '{{count}} active hires', { count: view.hires }),
  ].filter((part) => part !== '');
  if (parts.length === 0) return null;
  return (
    <Typography variant="bodySmall" color="text.secondary" data-testid={`agent-workforce-hire-evidence-${proposal.id}`}>
      {parts.join(' · ')}
    </Typography>
  );
}

interface HireProposalCardProps {
  readonly projectId: string;
  readonly proposal: AgentWorkforceProposal;
  readonly onOpenWizard: (proposal: AgentWorkforceProposal) => void;
}

export function HireProposalCard({ projectId, proposal, onOpenWizard }: HireProposalCardProps): ReactNode {
  const { discard } = useMarketplaceProposalActions(projectId);
  const [error, setError] = useState<string | undefined>(undefined);
  const view = hireView(proposal);
  const modeLabel = view.mode === 'fork' ? t('features.agentWorkforce.marketplace.fork', 'Fork') : t('features.agentWorkforce.marketplace.install', 'Install');
  const discardIt = (): void => {
    setError(undefined);
    discard.mutate(proposal.id, { onError: (failure) => setError(workforceRefusal(failure, failed()).message) });
  };
  return (
    <Box sx={cardSx} data-testid={`agent-workforce-hire-${proposal.id}`}>
      <Typography variant="labelMedium">
        {t('features.agentWorkforce.marketplace.hireHeading', 'Hire {{listing}} {{version}} ({{mode}}, {{tier}} tier)', {
          listing: view.listingName,
          version: view.versionName,
          mode: modeLabel,
          tier: view.tier,
        })}
      </Typography>
      <Typography variant="bodySmall" sx={{ overflowWrap: 'anywhere' }}>
        {t('features.agentWorkforce.marketplace.byAgent', 'A workforce manager proposed it: {{rationale}}', { rationale: proposal.rationale })}
      </Typography>
      <Evidence proposal={proposal} />
      <Box sx={{ display: 'flex', gap: '0.5rem' }}>
        <Button size="small" variant="contained" disabled={discard.isPending} onClick={() => onOpenWizard(proposal)} data-testid={`agent-workforce-hire-open-${proposal.id}`}>
          {t('features.agentWorkforce.marketplace.openWizard', 'Open hire wizard')}
        </Button>
        <Button size="small" variant="outlined" disabled={discard.isPending} onClick={discardIt} data-testid={`agent-workforce-hire-discard-${proposal.id}`}>
          {t('features.agentWorkforce.marketplace.discard', 'Discard')}
        </Button>
      </Box>
      {error !== undefined && (
        <Typography variant="bodySmall" color="error.main">
          {error}
        </Typography>
      )}
    </Box>
  );
}

interface ReportProposalCardProps {
  readonly projectId: string;
  readonly proposal: AgentWorkforceProposal;
}

export function ReportProposalCard({ projectId, proposal }: ReportProposalCardProps): ReactNode {
  const { discard, publish } = useMarketplaceProposalActions(projectId);
  const view = reportView(proposal);
  const [draft, setDraft] = useState(proposal.rationale);
  const [error, setError] = useState<string | undefined>(undefined);
  const busy = discard.isPending || publish.isPending;
  const valid = draft.trim() !== '' && draft.length <= REPORT_TEXT_MAX;
  const onError = (failure: Error): void => setError(workforceRefusal(failure, failed()).message);
  const run = (action: 'publish' | 'discard'): void => {
    setError(undefined);
    if (action === 'discard') discard.mutate(proposal.id, { onError });
    else publish.mutate({ proposal, text: draft === proposal.rationale ? undefined : draft.trim() }, { onError });
  };
  return (
    <Box sx={cardSx} data-testid={`agent-workforce-report-${proposal.id}`}>
      <Typography variant="labelMedium">
        {t('features.agentWorkforce.marketplace.reportHeading', 'Workforce report on {{listing}}: {{outcome}} at {{stage}}', {
          listing: view.listingName,
          outcome: view.outcome,
          stage: stageLabel(view.stage),
        })}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t('features.agentWorkforce.marketplace.reportHelp', 'A workforce manager drafted this. It publishes to the listing as "Workforce report from a hiring project", with the platform’s summary and never a transcript. Edit it first if you wish.')}
      </Typography>
      {view.scorecard.length > 0 && (
        <Typography variant="bodySmall" color="text.secondary" data-testid={`agent-workforce-report-scorecard-${proposal.id}`}>
          {view.scorecard.map(([key, value]) => `${key.replace('_', ' ')} ${Number.isInteger(value) ? value : value.toFixed(2)}`).join(' · ')}
        </Typography>
      )}
      <TextField
        multiline
        minRows={3}
        size="small"
        value={draft}
        disabled={busy}
        onChange={(event) => setDraft(event.target.value)}
        error={draft.length > REPORT_TEXT_MAX}
        helperText={t('features.agentWorkforce.marketplace.reportCount', '{{count}} of {{max}} characters', { count: draft.length, max: REPORT_TEXT_MAX })}
        slotProps={{ htmlInput: { 'data-testid': `agent-workforce-report-text-${proposal.id}` } }}
      />
      <Box sx={{ display: 'flex', gap: '0.5rem' }}>
        <Button size="small" variant="contained" disabled={busy || !valid} onClick={() => run('publish')} data-testid={`agent-workforce-report-publish-${proposal.id}`}>
          {t('features.agentWorkforce.marketplace.publish', 'Publish')}
        </Button>
        <Button size="small" variant="outlined" disabled={busy} onClick={() => run('discard')} data-testid={`agent-workforce-report-discard-${proposal.id}`}>
          {t('features.agentWorkforce.marketplace.discard', 'Discard')}
        </Button>
      </Box>
      {error !== undefined && (
        <Alert severity="error" data-testid={`agent-workforce-report-error-${proposal.id}`}>
          {error}
        </Alert>
      )}
    </Box>
  );
}
