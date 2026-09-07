/**
 * The sources screen's data: what this toolkit ingests from, and how each one
 * went.
 *
 * THREE READS, JOINED, and none of them is optional:
 *
 *   the toolkit's own settings   which sources exist at all, and their
 *                                per-source branch and patterns
 *   the project's toolkit list   what each source is CALLED; a bare id is not
 *                                something a user recognises
 *   `get_sources_status`         whether it has been ingested, and what came
 *                                out
 *
 * The JOIN itself is `lib/joinSources.ts`, deliberately outside this hook: a
 * row rendered with the wrong status, or a row silently dropped, is a screen
 * that reads perfectly and says something false, and a pure function with its
 * own tests is the way to hold that.
 */
import { useMemo } from 'react';

import {
  INVENTORY_FAMILY,
  inventoryConfig,
  inventoryDocuments,
  useInventoryTool,
  useSourceToolkitNames,
  type IngestionStatus,
  type InventorySource,
  type InventoryTarget,
} from '@/entities/inventory';

import { joinSources, type ConfiguredSource } from '../lib/joinSources';

/** What the sources panel renders. */
export interface InventorySourcesView {
  readonly sources: readonly InventorySource[];
  readonly ingestion: IngestionStatus;
  readonly isPending: boolean;
  /** The status read failed. The configured sources are still listed. */
  readonly statusError: string | null;
}

function messageOf(error: unknown): string | null {
  return error instanceof Error && error.message !== '' ? error.message : null;
}

export function useInventorySources(
  target: InventoryTarget,
  options: { readonly enabled?: boolean } = {},
): InventorySourcesView {
  const enabled = options.enabled ?? true;
  const names = useSourceToolkitNames(target.projectId);
  const statuses = useInventoryTool(target, INVENTORY_FAMILY, 'get_sources_status', {}, { enabled });
  const ingestion = useInventoryTool(target, INVENTORY_FAMILY, 'get_ingestion_status', {}, { enabled });

  const configured = useMemo<readonly ConfiguredSource[]>(() => {
    return inventoryConfig.sourceIds(target.settings).map((toolkitId) => {
      const known = names.data?.get(toolkitId);
      const config = inventoryConfig.sourceConfig(target.settings, toolkitId);
      return {
        toolkitId,
        name: known?.name ?? '',
        type: known?.type ?? '',
        branch: config.branch,
        filePatterns: config.filePatterns,
        excludePatterns: config.excludePatterns,
        preset: config.preset,
      };
    });
  }, [names.data, target.settings]);

  const sources = useMemo(
    () => joinSources(configured, inventoryDocuments.sourceStatuses(statuses.data?.document)),
    [configured, statuses.data],
  );

  return {
    sources,
    // A document that never arrived reads as `running: false` rather than as
    // an absent value: "no ingestion is running" is the honest reading of a
    // status nobody could obtain, and it is what the panel must render.
    ingestion: inventoryDocuments.ingestionStatus(ingestion.data?.document),
    // The NAMES query is not part of pending: a listing that is still loading
    // costs the rows their labels, not their existence, and waiting for it
    // would leave the panel blank while the ids are already known.
    isPending: enabled && statuses.isPending,
    statusError: messageOf(statuses.error),
  };
}
