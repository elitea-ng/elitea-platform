/**
 * The graph's statistics, its cache, and the two maintenance tools.
 *
 * `get_stats` IS READ WITH THE SAME ARGUMENTS THE BROWSER READS IT WITH, on
 * purpose: `useInventoryTool` keys a read by project, toolkit, family, tool and
 * arguments, so the browser's facets and this panel's counts are one
 * invocation. Adding an argument here — even a harmless one — would split the
 * key and double the load on the engine for no visible difference.
 *
 * MAINTENANCE IS A WRITE. `normalize_types` and `rebuild_indices` change the
 * stored graph, so every read of this toolkit is stale afterwards; the mutation
 * invalidates the whole `['inventory','tool',project,toolkit]` prefix rather
 * than the two queries this panel happens to hold. A panel that refreshed only
 * itself would leave the browser listing types that no longer exist.
 */
import { useCallback, useMemo, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import {
  INVENTORY_FAMILY,
  inventoryDocuments,
  inventoryToolkitToolsKey,
  runInventoryTool,
  useInventoryTool,
  type InventoryStats,
  type InventoryTarget,
} from '@/entities/inventory';

/** What the statistics panel renders. */
export interface InventoryStatsView {
  readonly stats: InventoryStats;
  readonly cachedGraphs: number | null;
  readonly cacheSizeBytes: number | null;
  readonly isPending: boolean;
  readonly error: string | null;
}

export function useInventoryStats(
  target: InventoryTarget,
  options: { readonly enabled?: boolean } = {},
): InventoryStatsView {
  const enabled = options.enabled ?? true;
  const stats = useInventoryTool(target, INVENTORY_FAMILY, 'get_stats', {}, { enabled });
  const cache = useInventoryTool(target, INVENTORY_FAMILY, 'get_cache_stats', {}, { enabled });

  const view = useMemo(() => {
    const cacheStats = inventoryDocuments.cacheStats(cache.data?.document);
    return {
      stats: inventoryDocuments.stats(stats.data?.document),
      cachedGraphs: cacheStats.cachedGraphs,
      cacheSizeBytes: cacheStats.sizeBytes,
    };
  }, [stats.data, cache.data]);

  return {
    ...view,
    isPending: enabled && stats.isPending,
    // The CACHE read failing does not hide the counts: the two are separate
    // facts and a graph's size is worth showing without its cache usage.
    error: stats.error instanceof Error ? stats.error.message : null,
  };
}

/** The two maintenance tools this panel offers, by the name the provider serves. */
export type MaintenanceTool = 'normalize_types' | 'rebuild_indices';

/** Running one maintenance tool, and what it said. */
export interface MaintenanceRun {
  readonly running: MaintenanceTool | null;
  readonly message: string | null;
  readonly error: string | null;
  readonly run: (tool: MaintenanceTool) => void;
}

export function useMaintenance(target: InventoryTarget): MaintenanceRun {
  const queryClient = useQueryClient();
  const [running, setRunning] = useState<MaintenanceTool | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const run = useCallback(
    (tool: MaintenanceTool) => {
      setRunning(tool);
      setMessage(null);
      setError(null);
      // `output_format: 'json'` is NOT sent: these two tools answer a sentence
      // ("Types are already normalised: 3 distinct types."), and that sentence
      // is what the panel shows. Asking for JSON would give a document whose
      // fields differ between the fixture and the engine, and the screen would
      // have to guess which.
      void runInventoryTool(target, INVENTORY_FAMILY, tool, {})
        .then((run) => {
          setMessage(run.text);
        })
        .catch((reason: unknown) => {
          setError(reason instanceof Error ? reason.message : 'The maintenance tool failed.');
        })
        .finally(() => {
          setRunning(null);
          // EVERY read of this toolkit, not just this panel's two. Normalising
          // the types rewrites the graph, so the entity lists and the facets
          // are stale as well.
          void queryClient.invalidateQueries({ queryKey: inventoryToolkitToolsKey(target) });
        });
    },
    [queryClient, target],
  );

  return { running, message, error, run };
}
