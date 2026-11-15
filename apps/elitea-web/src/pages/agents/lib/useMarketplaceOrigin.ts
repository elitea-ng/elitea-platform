import { useQuery } from '@tanstack/react-query';

import { getMarketplaceOrigin } from '@/shared/api/generated/marketplace/marketplace';
import type { MarketplaceOrigin } from '@/shared/api/generated/model';

/**
 * Where a hired agent came from (ADR-0051): the listing, the pinned version
 * and the states a banner reports. An agent the project owns has no origin
 * and the server answers 404, which reads as "none" here and is not retried.
 */
export function useMarketplaceOrigin(
  projectId: string | undefined,
  applicationId: number | undefined,
): MarketplaceOrigin | undefined {
  const query = useQuery({
    queryKey: ['marketplace', 'origin', projectId, applicationId],
    queryFn: async ({ signal }) => {
      const envelope = await getMarketplaceOrigin(projectId ?? '', applicationId ?? 0, { signal });
      return envelope.data as MarketplaceOrigin;
    },
    enabled: projectId !== undefined && projectId !== '' && applicationId !== undefined,
    retry: false,
    staleTime: 30_000,
  });
  return query.data;
}

/** An installed agent: its definition is the listing version's snapshot and cannot be edited. */
export function isInstalledOrigin(origin: MarketplaceOrigin | undefined): boolean {
  return origin?.mode === 'installed';
}
