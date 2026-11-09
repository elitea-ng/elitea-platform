/**
 * The agents registry's proposals inbox (ADR-0050 decision 5a): a toolbar
 * button with the number of open proposals, and a dialog listing them. Each
 * row opens the review dialog.
 *
 * Shown only when the project HAS open proposals: the list route answers for
 * anyone who may list agents, and an empty inbox is not an affordance worth a
 * toolbar slot. The page mounts it only for a person who may create agents,
 * since Create is the inbox's point.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentProposal } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { CreatedAgent } from '../model/useAgentProposals';
import { useOpenProposals } from '../model/useAgentProposals';
import { ProposalReviewDialog } from './ProposalReviewDialog';

export interface ProposalsInboxButtonProps {
  readonly projectId: string | undefined;
  /** Called once a reviewed proposal became an agent (the page opens it). */
  readonly onCreated?: ((created: CreatedAgent) => void) | undefined;
}

export function ProposalsInboxButton({ projectId, onCreated }: ProposalsInboxButtonProps): ReactNode {
  const open = useOpenProposals(projectId);
  const [listOpen, setListOpen] = useState(false);
  const [reviewing, setReviewing] = useState<AgentProposal | undefined>(undefined);
  const rows = open.data?.rows ?? [];
  if (projectId === undefined || rows.length === 0) return null;

  return (
    <>
      <Button variant="outlined" size="small" onClick={() => setListOpen(true)} data-testid="agent-proposals-inbox">
        {t('features.agentProposals.inbox.button', 'Proposals ({{count}})', { count: open.data?.total ?? rows.length })}
      </Button>
      <BaseModal
        open={listOpen && reviewing === undefined}
        data-testid="agent-proposals-inbox-dialog"
        title={t('features.agentProposals.inbox.title', 'Proposed agents')}
        onClose={() => setListOpen(false)}
        content={
          <Box sx={listSx}>
            {rows.map((proposal) => (
              <Box key={proposal.id} sx={rowSx} data-testid={`agent-proposal-row-${proposal.id}`}>
                <Box sx={{ flex: '1 1 auto', minWidth: 0 }}>
                  <Typography variant="labelMedium">{proposalName(proposal)}</Typography>
                  <Typography variant="bodySmall" color="text.secondary" sx={{ overflowWrap: 'anywhere' }}>
                    {t('features.agentProposals.inbox.from', 'From {{name}}: {{rationale}}', { name: proposal.proposer_name, rationale: proposal.rationale })}
                  </Typography>
                </Box>
                <Button size="small" onClick={() => setReviewing(proposal)} data-testid={`agent-proposal-review-${proposal.id}`}>
                  {t('features.agentProposals.inbox.review', 'Review')}
                </Button>
              </Box>
            ))}
          </Box>
        }
      />
      <ProposalReviewDialog
        projectId={projectId}
        proposal={reviewing}
        onClose={() => setReviewing(undefined)}
        onCreated={(created) => {
          setListOpen(false);
          onCreated?.(created);
        }}
      />
    </>
  );
}

export function proposalName(proposal: Pick<AgentProposal, 'draft'>): string {
  const name = proposal.draft['name'];
  return typeof name === 'string' && name.trim() !== '' ? name : t('features.agentProposals.inbox.unnamed', 'Proposed agent');
}

const listSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem', minWidth: '28rem' };
const rowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.75rem' };
