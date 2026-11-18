import { RefreshControl } from 'react-native';

import { proposalsErrorText, useMarketplaceProposals } from '@/api/queries/workforce';
import { useDesign } from '@/design/DesignProvider';
import { MarketplaceProposalsView } from '@/features/workforce/MarketplaceProposalsView';
import { useWorkforceAvailable } from '@/features/workforce/useWorkforceAvailable';
import { useShareOut } from '@/security/policy';
import { useSession } from '@/state/session';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/**
 * Settings > Workforce proposals: the open hire proposals and drafted reports of the project's
 * workforce managers (ADR-0051 decision 6, client contract 1.56). Read-only and online only; an
 * agent manager's list (the server answers 403 to anyone else).
 */
export default function WorkforceProposalsScreen() {
  const { colors } = useDesign();
  const projectId = useSession((s) => s.projectId);
  const workspace = useWorkspaceInfo();
  const supported = useWorkforceAvailable();
  const proposals = useMarketplaceProposals(projectId, supported);
  const selectable = useShareOut();

  if (!supported) {
    const why = workspace === null ? 'You appear to be offline.' : 'This workspace does not offer workforce proposals to mobile yet.';
    return <MarketplaceProposalsView status={{ kind: 'unavailable', message: why }} rows={[]} selectable={false} onRetry={() => {}} />;
  }
  return (
    <MarketplaceProposalsView
      status={proposals.isLoading ? { kind: 'loading' } : proposals.error ? { kind: 'error', message: proposalsErrorText(proposals.error) } : { kind: 'ready' }}
      rows={proposals.data ?? []}
      selectable={selectable}
      onRetry={() => void proposals.refetch()}
      refreshControl={<RefreshControl refreshing={proposals.isRefetching} onRefresh={() => void proposals.refetch()} tintColor={colors.accent} />}
    />
  );
}
