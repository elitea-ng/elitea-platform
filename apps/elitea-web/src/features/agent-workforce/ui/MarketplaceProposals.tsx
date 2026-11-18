/**
 * Settings › Agent workforce › Marketplace proposals (ADR-0051 decision 6):
 * the hire and report proposals of the project's workforce managers, for an
 * agent manager. The hire wizard belongs to the Marketplace page cluster, so
 * the page that mounts this hands the open action in (`onOpenWizard`).
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import Typography from '@mui/material/Typography';

import type { AgentWorkforceProposal } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useMarketplaceProposals } from '../model/useMarketplaceProposals';
import { HireProposalCard, ReportProposalCard } from './MarketplaceProposalCards';

interface MarketplaceProposalsProps {
  readonly projectId: string;
  readonly canManage: boolean;
  readonly onOpenWizard: (proposal: AgentWorkforceProposal) => void;
}

export function MarketplaceProposals({ projectId, canManage, onOpenWizard }: MarketplaceProposalsProps): ReactNode {
  const query = useMarketplaceProposals(projectId, canManage);
  if (!canManage || projectId === '') return null;
  const rows = query.data?.rows ?? [];
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }} data-testid="agent-workforce-marketplace">
      <Typography variant="headingSmall">{t('features.agentWorkforce.marketplace.title', 'Marketplace proposals')}</Typography>
      {query.isLoading && <LinearProgress />}
      {query.isError && <Alert severity="error">{t('features.agentWorkforce.marketplace.loadFailed', 'Failed to load the Marketplace proposals.')}</Alert>}
      {query.isSuccess && rows.length === 0 && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="agent-workforce-marketplace-empty">
          {t('features.agentWorkforce.marketplace.empty', 'No workforce manager has proposed a hire or drafted a report.')}
        </Typography>
      )}
      {rows.map((proposal) =>
        proposal.kind === 'hire_listing' ? (
          <HireProposalCard key={proposal.id} projectId={projectId} proposal={proposal} onOpenWizard={onOpenWizard} />
        ) : (
          <ReportProposalCard key={proposal.id} projectId={projectId} proposal={proposal} />
        ),
      )}
    </Box>
  );
}
