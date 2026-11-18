import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';

import type { DelegationGrant } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

/**
 * The banner on a calling agent's Tools panel that counts its delegations awaiting
 * review (ADR-0044 Amendment 1, A1.3; epic 2156 story S6). The migration of existing
 * compositions wrote a pending grant, flagged legacy, for each attached agent that
 * holds more than this one, and approved none: until an editor approves it, the
 * attached agent runs without what it holds beyond this agent's own (or is
 * refused, in enforce mode), and the row says "Review delegation". Nothing is
 * shown when nothing awaits review.
 */
export interface DelegationPendingBannerProps {
  readonly grants: readonly DelegationGrant[];
}

export function DelegationPendingBanner({ grants }: DelegationPendingBannerProps): ReactNode {
  const pending = grants.filter((grant) => grant.state === 'pending');
  if (pending.length === 0) return null;
  const legacy = pending.filter((grant) => grant.legacy).length;
  const governed = pending.some((grant) => grant.governed);
  return (
    <Alert severity="warning" data-testid="delegation-pending-banner" data-pending={pending.length} data-legacy={legacy}>
      {legacy === pending.length
        ? t(
            'agents.delegation.bannerLegacy',
            '{{count}} delegations from before this release await review. Nothing was approved for you, so the attached agents run without what they hold beyond this agent. Use Review delegation on each row.',
            { count: pending.length },
          )
        : t(
            'agents.delegation.bannerPending',
            '{{count}} delegations await review. Until one is approved, the attached agent runs without what it holds beyond this agent. Use Review delegation on each row.',
            { count: pending.length },
          )}{' '}
      {governed
        ? t('agents.delegation.bannerGoverned', 'A different approver decides each one in its change request.')
        : null}
    </Alert>
  );
}
