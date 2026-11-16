/**
 * The state of the Upgrade to... dialog (ADR-0051 decision 4): the listing's
 * versions, the plan for the chosen one, the bindings the person still has to
 * choose, and the upgrade itself. Nothing is read while the dialog is closed.
 */
import { useEffect, useState } from 'react';

import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from '@tanstack/react-query';

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

export function useUpgradeFlow(input: UpgradeFlowInput): UpgradeFlow {
  const { open, projectId, applicationId, listingId, pinnedVersionId, initialVersionId } = input;
  const queryClient = useQueryClient();
  const [versionId, setVersionId] = useState(initialVersionId ?? '');
  const [choices, setChoices] = useState<BindingChoices>({});
  const [result, setResult] = useState<MarketplaceUpgradeResult>();

  const listing = useQuery({
    queryKey: ['marketplace', 'upgrade-listing', projectId, listingId],
    enabled: open && projectId !== undefined,
    queryFn: async () => unwrapBody(await getMarketplaceListing(projectId ?? '', listingId)) as MarketplaceListingDetail,
  });
  const versions = (listing.data?.versions ?? []).filter((version) => version.id !== pinnedVersionId);
  const firstId = versions[0]?.id;
  useEffect(() => {
    if (open && versionId === '' && firstId !== undefined) setVersionId(initialVersionId ?? firstId);
  }, [open, versionId, firstId, initialVersionId]);

  const plan = useQuery({
    queryKey: ['marketplace', 'upgrade-plan', projectId, applicationId, versionId],
    enabled: open && projectId !== undefined && versionId !== '',
    retry: false,
    queryFn: async ({ signal }) =>
      unwrapBody(
        await getMarketplaceUpgradePlan(projectId ?? '', applicationId, { version_id: Number(versionId) }, { signal }),
      ) as MarketplaceUpgradePlan,
  });
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

  const unbound = planned === undefined ? [] : unboundRequirements({ ...planned.requirements, items: planned.new_requirements }, choices);
  return {
    listing,
    versions,
    versionId,
    choose: (id) => {
      setResult(undefined);
      setVersionId(id);
    },
    plan,
    choices,
    bind: (key, value) => setChoices((now) => applyChoice(now, key, value)),
    upgrade,
    result,
    blocked: planned === undefined || !planned.hireable || unbound.length > 0 || result !== undefined,
  };
}
