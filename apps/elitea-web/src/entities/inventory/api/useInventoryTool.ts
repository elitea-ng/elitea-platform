/**
 * One Inventory read, as a react-query query.
 *
 * IT IS AN ENTITY AND NOT A FEATURE because four features need it and
 * `no-sideways-features` forbids one feature importing another. Every panel in
 * this application reads the graph the same way — invoke a tool, poll it,
 * parse the document — and a copy of that per panel is four places for the
 * `output_format` argument, the query key and the refusal handling to drift.
 *
 * `output_format: 'json'` IS SET HERE, for every read, and that is the whole
 * reason this wrapper exists rather than a bare `useQuery`. Every read tool the
 * provider serves answers MARKDOWN by default and a JSON document only when
 * asked (`fixtureAnswer`, internal/apps/inventory/run/fixture.go:700-712). A
 * panel that forgets the argument gets a formatted report where it expected
 * rows, renders nothing, and reports no error.
 *
 * THE QUERY KEY CARRIES THE PARAMETERS. Two panels reading
 * `list_entities_by_type` for two different types are two different reads; a
 * key that named only the tool would serve the second panel the first one's
 * rows. `JSON.stringify` is enough because every argument here is a scalar or
 * a short list, and the key must be a value react-query can compare.
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { runInventoryTool, type InventoryTarget, type ToolParams, type ToolRun } from './inventoryToolApi';

/**
 * How long one read's answer is reused before another invocation is made.
 *
 * Thirty seconds: long enough that switching tabs and coming back costs
 * nothing, short enough that a user who reruns an ingestion and looks again
 * sees the new graph without reloading the page.
 */
const STALE_MS = 30_000;

/** The argument every read tool carries, and the one no panel may forget. */
const JSON_OUTPUT: ToolParams = { output_format: 'json' };

export interface InventoryToolQueryOptions {
  /**
   * Whether to read at all. A panel that is not on screen must not invoke:
   * an invocation is a unit of work on the provider, not a cached GET, so a
   * hidden tab that polls is real load on the engine for nothing.
   */
  readonly enabled?: boolean;
  /** Poll interval for the underlying loop; the default suits a read. */
  readonly intervalMs?: number | undefined;
}

/**
 * The key one read is cached under. Exported so a write can invalidate exactly
 * the reads it invalidates — an ingestion changes the graph, so the sources,
 * the entity lists and the statistics are all stale, and they are stale
 * together because they share the `['inventory', 'tool', project, toolkit]`
 * prefix.
 */
function inventoryToolQueryKey(
  target: Pick<InventoryTarget, 'projectId' | 'toolkitId'>,
  family: string,
  tool: string,
  params: ToolParams,
): readonly unknown[] {
  return ['inventory', 'tool', target.projectId, target.toolkitId, family, tool, JSON.stringify(params)];
}

/** The prefix every read of one toolkit's graph shares. */
export function inventoryToolkitToolsKey(
  target: Pick<InventoryTarget, 'projectId' | 'toolkitId'>,
): readonly unknown[] {
  return ['inventory', 'tool', target.projectId, target.toolkitId];
}

/**
 * Read the graph with one tool.
 *
 * `retry: false`, deliberately. A refusal — an entity that is not in the
 * graph, a source this toolkit does not own — is a terminal answer, and
 * retrying it three times turns a two-second "no such entity" into a
 * ten-second one that says the same thing. A transport failure is retried by
 * the user reopening the panel, which is also what tells them it failed.
 */
export function useInventoryTool(
  target: InventoryTarget,
  family: string,
  tool: string,
  params: ToolParams,
  options: InventoryToolQueryOptions = {},
): UseQueryResult<ToolRun, Error> {
  const merged: ToolParams = { ...JSON_OUTPUT, ...params };
  return useQuery({
    queryKey: inventoryToolQueryKey(target, family, tool, merged),
    enabled: (options.enabled ?? true) && target.projectId !== '' && target.toolkitId !== '',
    retry: false,
    // A READ HERE IS AN INVOCATION ON SOMEONE ELSE'S ENGINE, not a cached GET,
    // and react-query's defaults are written for the second thing: data is
    // stale at once and a remount or a window refocus refetches. On this
    // screen a remount is a tab switch, so the defaults would run the whole
    // graph again every time the user looked at the Sources tab and came
    // back — and again every time they alt-tabbed to their editor and
    // returned. The window is short enough that a user who waits and looks
    // again gets a fresh answer, and every WRITE invalidates the whole
    // toolkit prefix explicitly, so nothing depends on it for correctness.
    staleTime: STALE_MS,
    refetchOnWindowFocus: false,
    queryFn: ({ signal }) =>
      runInventoryTool(target, family, tool, merged, {
        signal,
        ...(options.intervalMs === undefined ? {} : { intervalMs: options.intervalMs }),
      }),
  });
}
