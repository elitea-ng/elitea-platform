/**
 * `/admin/gateway/metered-prices` — the price list of metered tool operations
 * (#1238): one USD price per unit (request, credit or page) for each
 * (provider, operation), read by the gateway before it admits a web search,
 * a web fetch or a host lookup. An operation with no row is refused, so free
 * operations carry an explicit 0. Nothing price-syncs this list: a save is the
 * price until the next save.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

import type { MeteredUnit } from '../meteredToolLabels';

const METERED_PRICES_URL = '/admin/gateway/metered-prices';

export interface MeteredPriceRow {
  readonly provider: string;
  readonly operation: string;
  readonly unit: MeteredUnit;
  readonly price_per_unit_usd: number;
  readonly source?: string;
  readonly price_overridden: boolean;
  readonly price_overridden_at?: string;
  /** The author's user id. */
  readonly price_overridden_by?: string;
  readonly updated_at?: string;
}

export interface MeteredPriceDraft {
  readonly provider: string;
  readonly operation: string;
  readonly unit: MeteredUnit;
  readonly price_per_unit_usd: number;
}

const meteredPriceKeys = { all: ['admin', 'llmProxy', 'meteredPrices'] as const };

export function useAdminMeteredPrices(): UseQueryResult<readonly MeteredPriceRow[], Error> {
  return useQuery({
    queryKey: meteredPriceKeys.all,
    queryFn: async (): Promise<readonly MeteredPriceRow[]> => {
      const body = unwrapBody(await eliteaFetch<unknown>(METERED_PRICES_URL)) as
        { readonly prices?: readonly MeteredPriceRow[] } | undefined;
      return body?.prices ?? [];
    },
  });
}

/** `PUT /admin/gateway/metered-prices` — the body carries the identity, the unit and the price only. */
export function useSaveAdminMeteredPrice(): UseMutationResult<void, Error, MeteredPriceDraft> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (draft: MeteredPriceDraft) => {
      await eliteaFetch<unknown>(METERED_PRICES_URL, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(draft),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: meteredPriceKeys.all }),
  });
}
