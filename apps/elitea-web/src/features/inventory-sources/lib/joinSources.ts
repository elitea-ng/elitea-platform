/**
 * Joining the sources a toolkit CONFIGURES with the statuses the provider
 * REPORTS, as one pure function.
 *
 * It is pure, and separate from the hook, because it is the only part of the
 * sources screen that can be wrong without looking wrong: a row rendered with
 * the wrong status, or a row dropped, is a screen that reads perfectly and says
 * something false. A function with its own tests is the way to hold that.
 *
 * TWO DIRECTIONS, and both are load-bearing:
 *
 *  - a configured source with NO status is a row. It was added and has never
 *    been ingested — the state a user is in right after adding one — and
 *    dropping it would make the add appear to have failed.
 *  - a reported status with NO configured source is ALSO a row. Its entities
 *    are in the graph and `remove_source_entities` is the only thing that takes
 *    them out; hiding it leaves a user with entities they cannot account for.
 */
import type { InventorySource } from '@/entities/inventory';

import { statusForSource, type LabelledStatus } from './matchSource';

/** What is known about a source before its status is joined. */
export interface ConfiguredSource {
  readonly toolkitId: string;
  readonly name: string;
  readonly type: string;
  readonly branch: string;
  readonly filePatterns: string;
  readonly excludePatterns: string;
  readonly preset: string;
}

/** One reported status, in the shape `get_sources_status` answers. */
export interface ReportedStatus extends LabelledStatus {
  readonly status: string;
  readonly entityCount: number | null;
  readonly relationCount: number | null;
  readonly lastUpdated: string;
  readonly errorMessage: string;
}

/** A source the toolkit no longer names, seen only through its status. */
function orphanRow(status: ReportedStatus): InventorySource {
  return {
    // No toolkit id: nothing can be re-ingested for it, because there is no id
    // to send. The panel offers no ingest control on such a row.
    toolkitId: '',
    name: status.source,
    type: '',
    branch: '',
    filePatterns: '',
    excludePatterns: '',
    preset: '',
    status: status.status,
    entityCount: status.entityCount,
    relationCount: status.relationCount,
    lastUpdated: status.lastUpdated,
    errorMessage: status.errorMessage,
  };
}

function configuredRow(source: ConfiguredSource, status: ReportedStatus | undefined): InventorySource {
  return {
    toolkitId: source.toolkitId,
    // The id is the fallback label, never the empty string: a row a user
    // cannot name is still a row they must be able to act on.
    name: source.name === '' ? source.toolkitId : source.name,
    type: source.type,
    branch: source.branch,
    filePatterns: source.filePatterns,
    excludePatterns: source.excludePatterns,
    preset: source.preset,
    status: status?.status ?? '',
    entityCount: status?.entityCount ?? null,
    relationCount: status?.relationCount ?? null,
    lastUpdated: status?.lastUpdated ?? '',
    errorMessage: status?.errorMessage ?? '',
  };
}

/**
 * The rows the sources table renders: the configured ones first, in the order
 * they were saved, then whatever the provider reported that nothing claims.
 */
export function joinSources(
  configured: readonly ConfiguredSource[],
  statuses: readonly ReportedStatus[],
): readonly InventorySource[] {
  const claimed = new Set<string>();
  const rows: InventorySource[] = [];

  for (const source of configured) {
    const status = statusForSource(statuses, source);
    if (status !== undefined) claimed.add(status.source);
    rows.push(configuredRow(source, status));
  }
  for (const status of statuses) {
    if (!claimed.has(status.source)) rows.push(orphanRow(status));
  }
  return rows;
}
