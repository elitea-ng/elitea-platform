/**
 * The hire wizard opened from a workforce manager's hire proposal (ADR-0051
 * decision 6): it reads the proposal's listing, fills in the version, the mode
 * and the suggested bindings, and the hire request carries the proposal id so
 * the hire and its approval are one transaction. The person still clicks Hire.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import LinearProgress from '@mui/material/LinearProgress';

import { useQueryClient } from '@tanstack/react-query';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';

import { HireWizard } from '../hire/HireWizard';
import type { HireProposalPrefill } from '../hire/hireModel';
import { useMarketplaceListing } from '../useMarketplaceListings';

interface HireProposalWizardProps {
  readonly projectId: string | undefined;
  readonly listingId: string;
  readonly proposal: HireProposalPrefill;
  readonly open: boolean;
  readonly onClose: () => void;
}

export function HireProposalWizard({ projectId, listingId, proposal, open, onClose }: HireProposalWizardProps): ReactNode {
  const queryClient = useQueryClient();
  const { detail, isPending, isError, isNotFound } = useMarketplaceListing(open ? projectId : undefined, listingId);
  if (!open) return null;
  if (detail === undefined) {
    return (
      <BaseModal
        open
        onClose={onClose}
        title={t('marketplace.hire.proposalTitle', 'Hire from a proposal')}
        data-testid="hire-proposal-loading"
        actions={{
          node: (
            <BaseBtn variant="secondary" onClick={onClose}>
              {t('shared.ui.baseModal.cancel', 'Cancel')}
            </BaseBtn>
          ),
        }}
        content={
          isPending ? (
            <LinearProgress />
          ) : (
            <Alert severity="error">
              {isNotFound || isError
                ? t('marketplace.hire.proposalGone', 'The listing is not available any more. Discard the proposal.')
                : ''}
            </Alert>
          )
        }
      />
    );
  }
  return (
    <HireWizard
      open
      onClose={onClose}
      projectId={projectId}
      detail={detail}
      initialMode={proposal.mode}
      proposal={proposal}
      onHired={() =>
        void queryClient.invalidateQueries({
          predicate: (query) => query.queryKey.some((part) => typeof part === 'string' && part.includes('agent_workforce')),
        })
      }
    />
  );
}
