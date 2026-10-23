import { useQuery } from '@tanstack/react-query';

import { getAvailableConfigurationsType } from '../api/configurations';
import type { ConfigurationTypeDescriptor } from '../api/configurations';

/**
 * Ported from `apps/elitea-ui/src/hooks/useGetCurrentConfigurationAsSchemas.js`
 * (30 lines) — this slice's own copy of the per-type `config_schema`
 * catalogue `ToolkitForm.tsx`/`useCreateConfiguration.ts`'s
 * `configurationsAsSchema` param needs.
 *
 * DISCLOSED REDESIGN: the baseline reads the fetched data from
 * `useSelector(state => state.applications.configurationsAsSchema)` — a
 * Redux cache populated by a SEPARATE `useGetAvailableConfigurationsTypeQuery`
 * RTK-Query hook's `onQueryStarted` side effect (i.e. the query result and
 * the read are two different call sites). This app has no Redux; the query
 * result IS the returned data directly (`../api/configurations.ts`'s
 * `getAvailableConfigurationsType`, API-145, wrapped in a plain TanStack
 * `useQuery`) — one call site, same eventual shape.
 */
const ALL_SECTIONS = ['credentials', 'ai_credentials', 'llm', 'embedding', 'image_generation', 'storage', 'asr', 'tts'] as const;

export interface UseConfigurationsAsSchemaParams {
  readonly skip?: boolean;
}

export interface UseConfigurationsAsSchemaResult {
  readonly configurationsAsSchema: readonly ConfigurationTypeDescriptor[] | undefined;
  readonly isFetching: boolean;
  readonly isLoading: boolean;
}

export function useConfigurationsAsSchema({ skip = false }: UseConfigurationsAsSchemaParams = {}): UseConfigurationsAsSchemaResult {
  const query = useQuery({
    queryKey: ['toolkits', 'configurations', 'available', ALL_SECTIONS],
    queryFn: ({ signal }) => getAvailableConfigurationsType({ section: [...ALL_SECTIONS] }, signal),
    enabled: !skip,
  });

  return {
    configurationsAsSchema: query.data,
    isFetching: query.isFetching,
    isLoading: query.isLoading,
  };
}
