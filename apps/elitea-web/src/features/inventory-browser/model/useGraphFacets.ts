/**
 * The values the filters can be set to: the types, layers and sources this
 * graph actually holds.
 *
 * IT READS `get_stats`, WHICH THE STATISTICS PANEL ALSO READS, and that is one
 * network request rather than two. `useInventoryTool` keys a read by
 * project, toolkit, family, tool and arguments, so two features asking the same
 * question share one cache entry and one invocation. Passing the facets down
 * from a common parent would have been the alternative, and it would have made
 * the browser unusable on its own.
 *
 * A FILTER MUST NOT OFFER A VALUE THE GRAPH DOES NOT HOLD. Hard-coding the
 * legacy type list — `class`, `function`, `service`, … — would offer a user
 * fourteen choices of which two return rows, and the twelve empty ones look
 * like a broken graph rather than an inapplicable filter.
 */
import { useMemo } from 'react';

import {
  INVENTORY_FAMILY,
  inventoryDocuments,
  useInventoryTool,
  type InventoryTarget,
} from '@/entities/inventory';

/** What the filter controls offer. */
export interface GraphFacets {
  readonly types: readonly string[];
  readonly layers: readonly string[];
  readonly sources: readonly string[];
  readonly isPending: boolean;
}

export function useGraphFacets(
  target: InventoryTarget,
  options: { readonly enabled?: boolean } = {},
): GraphFacets {
  const query = useInventoryTool(target, INVENTORY_FAMILY, 'get_stats', {}, {
    enabled: options.enabled ?? true,
  });

  return useMemo(() => {
    const stats = inventoryDocuments.stats(query.data?.document);
    return {
      types: stats.byType.map((entry) => entry.name),
      layers: stats.byLayer.map((entry) => entry.name),
      sources: stats.sourceToolkits,
      isPending: query.isPending && query.fetchStatus !== 'idle',
    };
  }, [query.data, query.isPending, query.fetchStatus]);
}
