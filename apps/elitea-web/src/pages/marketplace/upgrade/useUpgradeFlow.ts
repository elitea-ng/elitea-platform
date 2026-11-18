/**
 * The state of the Upgrade to... dialog (ADR-0051 decision 4): the listing's
 * versions, the plan for the chosen one, the bindings the person still has to
 * choose, and the upgrade itself. Nothing is read while the dialog is closed.
 */
import { useEffect, useState } from 'react';

import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import {
  getMarketplaceListing,
  getMarketplaceUpgradePlan,
  upgradeMarketplaceAgent,
} from '@/shared/api/generated/marketplace/marketplace';
import type {
  MarketplaceListingDetail,
  MarketplaceUpgradePlan,
  MarketplaceUpgradeResult,
  MarketplaceVersion,
} from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';

import { applyChoice, bindingsOf, choicesOf, unboundRequirements, type BindingChoices } from '../hire/hireModel';
import { invalidateMarketplace } from '../invalidate';

export interface UpgradeFlowInput {
  readonly open: boolean;
  readonly projectId: string | undefined;
  readonly applicationId: number;
  readonly listingId: string;
  readonly pinnedVersionId: string;
  readonly initialVersionId: string | undefined;
}

export interface UpgradeFlow {
  readonly listing: UseQueryResult<MarketplaceListingDetail>;
  readonly versions: readonly MarketplaceVersion[];
  readonly versionId: string;
  readonly choose: (id: string) => void;
  readonly plan: UseQueryResult<MarketplaceUpgradePlan>;
  readonly choices: BindingChoices;
  readonly bind: (key: string, value: string) => void;
  readonly upgrade: UseMutationResult<MarketplaceUpgradeResult, Error, void>;
  readonly result: MarketplaceUpgradeResult | undefined;
  /** Why the upgrade cannot be confirmed yet; false when it can. */
  readonly blocked: boolean;
}

/** The listing's versions, minus the pinned one, and the one that is chosen (the first offered when the choice is not offered). */
function useOfferedVersions(input: UpgradeFlowInput) {
  const { open, projectId, listingId, pinnedVersionId, initialVersionId } = input;
  const [chosen, setChosen] = useState(initialVersionId ?? '');
  const listing = useQuery({
    queryKey: ['marketplace', 'upgrade-listing', projectId, listingId],
    enabled: open && projectId !== undefined,
    queryFn: async () =>
      unwrapBody(await getMarketplaceListing(projectId ?? '', listingId)) as MarketplaceListingDetail,
  });
  const versions = (listing.data?.versions ?? []).filter((version) => version.id !== pinnedVersionId);
  const offered = versions.some((version) => version.id === chosen);
  const versionId = listing.isPending || offered ? chosen : (versions[0]?.id ?? '');
  return { listing, versions, versionId, setChosen };
}

function useUpgradePlan(input: UpgradeFlowInput, versionId: string) {
  const { open, projectId, applicationId } = input;
  return useQuery({
    queryKey: ['marketplace', 'upgrade-plan', projectId, applicationId, versionId],
    enabled: open && projectId !== undefined && versionId !== '',
    retry: false,
    queryFn: async ({ signal }) =>
      unwrapBody(
        await getMarketplaceUpgradePlan(projectId ?? '', applicationId, { version_id: Number(versionId) }, { signal }),
      ) as MarketplaceUpgradePlan,
  });
}

export function useUpgradeFlow(input: UpgradeFlowInput): UpgradeFlow {
  const { projectId, applicationId } = input;
  const queryClient = useQueryClient();
  const { listing, versions, versionId, setChosen } = useOfferedVersions(input);
  const [choices, setChoices] = useState<BindingChoices>({});
  const [result, setResult] = useState<MarketplaceUpgradeResult>();
  const plan = useUpgradePlan(input, versionId);
  const planned = plan.data;
  // The choices start from the bindings the pinned version already covers for this target.
  useEffect(() => {
    if (planned !== undefined) setChoices(choicesOf(planned.bindings));
  }, [planned]);

  const upgrade = useMutation({
    mutationFn: async () => {
      if (planned === undefined) throw new Error('The plan did not load');
      const response = await upgradeMarketplaceAgent(projectId ?? '', applicationId, {
        version_id: versionId,
        bindings: bindingsOf(planned.requirements, choices),
      });
      return unwrapBody(response) as MarketplaceUpgradeResult;
    },
    onSuccess: (done) => {
      setResult(done);
      invalidateMarketplace(queryClient);
    },
  });
  return {
    listing,
    versions,
    versionId,
    choose: (id) => {
      setResult(undefined);
      setChosen(id);
    },
    plan,
    choices,
    bind: (key, value) => setChoices((now) => applyChoice(now, key, value)),
    upgrade,
    result,
    blocked: isBlocked(planned, choices, result),
  };
}

/** The upgrade cannot be confirmed: no plan yet, a version that cannot be used, a toolkit still unbound, or it is done. */
function isBlocked(
  planned: MarketplaceUpgradePlan | undefined,
  choices: BindingChoices,
  result: MarketplaceUpgradeResult | undefined,
): boolean {
  if (planned === undefined || !planned.hireable || result !== undefined) return true;
  return unboundRequirements({ ...planned.requirements, items: planned.new_requirements }, choices).length > 0;
}
