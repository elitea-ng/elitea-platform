/**
 * Settings › Agent workforce (ADR-0050 decisions 5c and 5k, issue 1617): the
 * selected project's automatic hiring and dismissal policy and the list of
 * what it did, and (ADR-0051 decision 6, issue 1632) the Marketplace
 * proposals of workforce managers. The feature owns the page body; this file
 * is the drawer page's frame and hands the hire wizard to the proposals.
 */
import { memo, useState } from 'react';

import Box from '@mui/material/Box';
import Paper from '@mui/material/Paper';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { MarketplaceProposals, WorkforcePolicyPanel, hireView, useWorkforcePolicy } from '@/features/agent-workforce';
import { HireProposalWizard, type HireProposalPrefill } from '@/pages/marketplace';
import type { AgentWorkforceProposal } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import { useSelectedProjectStore } from '@/widgets/app-shell';

interface OpenHire {
  readonly listingId: string;
  readonly prefill: HireProposalPrefill;
}

function openHireOf(proposal: AgentWorkforceProposal): OpenHire {
  const view = hireView(proposal);
  return {
    listingId: view.listingId,
    prefill: {
      proposalId: String(proposal.id),
      versionId: view.versionId,
      mode: view.mode,
      bindings: Object.fromEntries(view.hints.map((hint) => [hint.requirementKey, hint.toolkitId])),
    },
  };
}

export const AgentWorkforceContent = memo(function AgentWorkforceContent() {
  const projectId = String(useSelectedProjectStore((s) => s.project?.id ?? ''));
  const policy = useWorkforcePolicy(projectId);
  const [hire, setHire] = useState<OpenHire | undefined>(undefined);
  return (
    <Paper elevation={0} sx={styles.root}>
      <DrawerPageHeader title={t('pages.settings.agentWorkforce.title', 'Agent workforce')} />
      <Box sx={styles.content} data-testid="settings-agent-workforce">
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'pages.settings.agentWorkforce.description',
            'Let the project hire proposed agents and dismiss poorly performing ones by rule. Both are off until an agent manager turns them on, and every automatic action is audited and can be undone or objected to.',
          )}
        </Typography>
        <WorkforcePolicyPanel projectId={projectId} />
        <MarketplaceProposals
          projectId={projectId}
          canManage={policy.data?.can_manage === true}
          onOpenWizard={(proposal) => setHire(openHireOf(proposal))}
        />
        {hire !== undefined && (
          <HireProposalWizard
            open
            projectId={projectId}
            listingId={hire.listingId}
            proposal={hire.prefill}
            onClose={() => setHire(undefined)}
          />
        )}
      </Box>
    </Paper>
  );
});

const styles: Record<string, SxProps<Theme>> = {
  root: {
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    overflow: 'hidden',
    borderRadius: 'var(--el-shape-radiusSm, 0px)',
  },
  content: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
    flex: 1,
    minHeight: 0,
    padding: '0 1.5rem 1.5rem',
    overflow: 'auto',
  },
};
