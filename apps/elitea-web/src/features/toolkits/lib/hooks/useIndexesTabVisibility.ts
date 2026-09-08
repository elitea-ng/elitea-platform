/**
 * `resolveIndexesTabVisibility` bound to the real toolkit-type schema map.
 *
 * Exists so `pages/toolkits/EditToolkit.tsx` needs ONE public symbol rather
 * than three: the type-schema lookup runs on `useGetCurrentToolkitSchemas`,
 * which is intra-slice here and unreachable from `pages/` (R-L3 —
 * `no-deep-slice-import`). See `../helpers/indexesTabVisibility.ts` for the
 * rule being applied and the baseline citation.
 */
import { useMemo } from 'react';

import { resolveIndexesTabVisibility } from '../helpers/indexesTabVisibility';
import type { IndexesTabVisibility } from '../helpers/indexesTabVisibility';
import { useGetCurrentToolkitSchemas } from './useGetCurrentToolkitSchemas.hooks';

export interface UseIndexesTabVisibilityParams {
  /** True on the `/mcps/:tab/:mcpId` route. */
  readonly isMCP: boolean;
  /** The toolkit instance's own `type` (e.g. `artifact`, `github`). */
  readonly toolkitType: string | undefined;
  /** The toolkit instance's saved `settings.selected_tools`. */
  readonly selectedTools: unknown;
}

export interface UseIndexesTabVisibilityResult extends IndexesTabVisibility {
  /** True while the type-schema map is still loading — the tab stays hidden until it settles, so it never flashes in and back out. */
  readonly isResolving: boolean;
}

export function useIndexesTabVisibility(params: UseIndexesTabVisibilityParams): UseIndexesTabVisibilityResult {
  const { isMCP, toolkitType, selectedTools } = params;

  // `skip` on the MCP route: the tab is hidden there regardless, so there is
  // no reason to issue the request at all.
  const { toolkitSchemas, isFetching } = useGetCurrentToolkitSchemas({ skip: isMCP });

  return useMemo(() => {
    const resolved = resolveIndexesTabVisibility({
      isMCP,
      toolkitTypeSchema: toolkitType === undefined ? undefined : toolkitSchemas?.[toolkitType],
      selectedTools,
    });
    const isResolving = !isMCP && (isFetching || toolkitSchemas === undefined || toolkitType === undefined);
    // `unavailableReason` passes through untouched: it is a property of the
    // TYPE, not of this screen, and the tab is deliberately still offered so
    // the reason has somewhere to be read (see the helper's own doc comment).
    return { ...resolved, hidden: resolved.hidden || isResolving, isResolving };
  }, [isMCP, toolkitType, toolkitSchemas, isFetching, selectedTools]);
}
