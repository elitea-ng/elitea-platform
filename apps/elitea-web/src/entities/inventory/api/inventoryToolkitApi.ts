/**
 * Reading the Inventory toolkits a project holds, and one toolkit's settings.
 *
 * THE TOOLKIT IS THE GRAPH'S ADDRESS, the same way a wiki toolkit is a wiki's:
 * a project can hold several Inventory toolkits, each with its own bucket and
 * its own set of sources, so "this project's knowledge graph" is not something
 * that can be asked for without naming which toolkit.
 *
 * TWO TYPES ARE LISTED, not one. The descriptor advertises `inventory` and
 * `inventory_search`, and only the first owns a graph — the second REFERENCES
 * one that another toolkit built. Listing both would offer the user a
 * workspace that can read nothing and ingest nothing, so the listing keeps the
 * owning type and the reason is recorded here rather than as a bare constant.
 *
 * `eliteaFetch` RETURNS THE ENVELOPE, not the body (#132), so every read goes
 * through `unwrapBody` / `unwrapList`.
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody, unwrapList } from '@/shared/api/unwrap';

import type { InventorySettings, InventoryToolkitSummary } from '../model/types';

/**
 * The `type` an Inventory toolkit row carries: the provider's own toolkit
 * name, lowercased, which is how the SPI addresses it in
 * `/tools/{toolkit_name}/{tool_name}/invoke`.
 */
const INVENTORY_TOOLKIT_TYPE = 'inventory';

function toolkitUrl(projectId: string, toolkitId: string): string {
  return `/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`;
}

/** One toolkit row as the read returns it; only three fields are read. */
interface ToolkitRow {
  readonly id?: unknown;
  readonly name?: unknown;
  readonly type?: unknown;
  readonly settings?: unknown;
  readonly toolkit_config?: unknown;
}

/**
 * A primitive rendered as text; anything else is NOT text.
 *
 * `String(someObject)` yields "[object Object]", which would make a malformed
 * row match no type and render as a toolkit named after the failure.
 */
function scalar(value: unknown): string {
  if (typeof value === 'string') return value;
  if (typeof value === 'number' || typeof value === 'boolean' || typeof value === 'bigint') {
    return String(value);
  }
  return '';
}

/** `settings`, then `toolkit_config` — the two places a toolkit's values live. */
function settingsOf(row: ToolkitRow): InventorySettings {
  if (typeof row.settings === 'object' && row.settings !== null && !Array.isArray(row.settings)) {
    return row.settings as InventorySettings;
  }
  if (
    typeof row.toolkit_config === 'object' &&
    row.toolkit_config !== null &&
    !Array.isArray(row.toolkit_config)
  ) {
    return row.toolkit_config as InventorySettings;
  }
  return {};
}

/** One Inventory toolkit, resolved: the row it came from and its settings. */
export interface InventoryToolkitContext {
  readonly toolkit: Readonly<Record<string, unknown>>;
  readonly settings: InventorySettings;
  readonly name: string;
}

export async function fetchInventoryToolkit(
  projectId: string,
  toolkitId: string,
): Promise<InventoryToolkitContext> {
  const row = (unwrapBody(await eliteaFetch<unknown>(toolkitUrl(projectId, toolkitId))) ??
    {}) as ToolkitRow;
  const name = scalar(row.name);
  return {
    toolkit: row as Readonly<Record<string, unknown>>,
    settings: settingsOf(row),
    // The id is the address, so a row with no name is still openable. Hiding
    // it behind a missing label would make a graph unreachable.
    name: name === '' ? toolkitId : name,
  };
}

/**
 * One query-key namespace, declared once.
 *
 * ROOTED AT `inventory`, and everything that writes a toolkit invalidates the
 * same root. A read key rooted anywhere else is the read/write namespace split
 * of #132: the save succeeds, the screen keeps the previous values, and
 * nothing reports it.
 */
const inventoryToolkitKey = (projectId: string, toolkitId: string) =>
  ['inventory', 'toolkit', projectId, toolkitId] as const;

export function useInventoryToolkit(
  projectId: string,
  toolkitId: string,
): UseQueryResult<InventoryToolkitContext, Error> {
  return useQuery({
    queryKey: inventoryToolkitKey(projectId, toolkitId),
    // Both halves must be present. `enabled: false` keeps the query idle
    // rather than firing at `/elitea_core/tool/prompt_lib//`, a URL that 404s
    // and renders as a broken toolkit rather than as one not chosen yet.
    enabled: projectId !== '' && toolkitId !== '',
    queryFn: () => fetchInventoryToolkit(projectId, toolkitId),
  });
}

/** The project's Inventory toolkits. */
export async function listInventoryToolkits(
  projectId: string,
): Promise<readonly InventoryToolkitSummary[]> {
  const rows = unwrapList<ToolkitRow>(
    await eliteaFetch<unknown>(`/elitea_core/tools/prompt_lib/${projectId}?limit=100`),
    'inventoryToolkitList',
  );
  return rows
    .filter((row) => scalar(row.type).toLowerCase() === INVENTORY_TOOLKIT_TYPE)
    .map((row) => {
      const id = scalar(row.id);
      const name = scalar(row.name);
      return { id, name: name === '' ? id : name };
    })
    .filter((row) => row.id !== '');
}

const inventoryToolkitListKey = (projectId: string) =>
  ['inventory', 'toolkit', 'list', projectId] as const;

export function useInventoryToolkits(
  projectId: string,
): UseQueryResult<readonly InventoryToolkitSummary[], Error> {
  return useQuery({
    queryKey: inventoryToolkitListKey(projectId),
    enabled: projectId !== '',
    queryFn: () => listInventoryToolkits(projectId),
  });
}

/**
 * The names of the source toolkits a project holds, by id.
 *
 * A source is stored as a bare id in `sources`, and an id is not something a
 * user recognises. This resolves the listing ONCE for the whole panel rather
 * than fetching each source toolkit separately: a project with eight sources
 * would otherwise make eight requests to render eight labels.
 */
export async function fetchSourceToolkitNames(
  projectId: string,
): Promise<ReadonlyMap<string, { readonly name: string; readonly type: string }>> {
  const rows = unwrapList<ToolkitRow>(
    await eliteaFetch<unknown>(`/elitea_core/tools/prompt_lib/${projectId}?limit=100`),
    'inventorySourceToolkitList',
  );
  const names = new Map<string, { name: string; type: string }>();
  for (const row of rows) {
    const id = scalar(row.id);
    if (id === '') continue;
    const name = scalar(row.name);
    names.set(id, { name: name === '' ? id : name, type: scalar(row.type) });
  }
  return names;
}

const sourceToolkitNamesKey = (projectId: string) => ['inventory', 'sources', 'names', projectId] as const;

export function useSourceToolkitNames(
  projectId: string,
): UseQueryResult<ReadonlyMap<string, { readonly name: string; readonly type: string }>, Error> {
  return useQuery({
    queryKey: sourceToolkitNamesKey(projectId),
    enabled: projectId !== '',
    queryFn: () => fetchSourceToolkitNames(projectId),
  });
}
