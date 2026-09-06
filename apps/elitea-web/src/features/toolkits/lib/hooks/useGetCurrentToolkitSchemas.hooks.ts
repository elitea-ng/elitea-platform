/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/lib/hooks/
 * useGetCurrentToolkitSchemas.hooks.js` (46 lines, Wave-2 unit A4b).
 *
 * DEVIATIONS FROM BASELINE (both disclosed):
 *  1. `useLazyToolkitTypesQuery` (RTK Query, global `state.applications.
 *     toolkitSchemas` Redux cache, a module-level `pendingFetches` Map
 *     hand-deduping concurrent fetches for the same `projectId`) is
 *     replaced with the generated `useListToolkits` (`shared/api/generated/
 *     toolkits/toolkits.ts`, GET `/elitea_core/toolkits/prompt_lib/
 *     {projectId}` — the toolkit-TYPE settings-schema catalogue, see that
 *     file's own `NOTE(W2)`). TanStack Query dedupes concurrent requests to
 *     the same query key itself (its cache IS keyed by `projectId`), so the
 *     baseline's hand-rolled `pendingFetches`/`hasFetchedRef` bookkeeping
 *     has no work left to do and is dropped entirely — this is the SAME
 *     substitution `features/agents/api/useToolkitTypeSchemas.ts` and
 *     `features/pipelines/lib/flow-editor/hooks/useToolkitTypeSchemas.ts`
 *     already made for their own local duplicates of this exact baseline
 *     hook (both of those files' own doc comments name this hook as what
 *     they replace, confirming the same generated endpoint is the right
 *     target — this file is simply the REAL owner finally landing it,
 *     unit A4, rather than a third local duplicate).
 *  2. `useSocket(sioEvents.mcp_status, handler)` (a raw socket.io context
 *     hook) is replaced with unit S5's typed `useSocketClient().on(...)`/
 *     `.off(...)` pair, matching `features/agents/lib/
 *     useAgentMCPToolsStatusMonitor.ts`'s own substitution for the exact
 *     same event (that file's doc comment cites THIS hook by name as a
 *     fellow `mcp_status` consumer per `shared/api/socket/events.ts`'s own
 *     evidence trail).
 *
 * `projectId` is read via this slice's own `useSelectedProjectId` (not a
 * parameter) — the baseline hook does the same (an internal
 * `useSelectedProjectId()` call, not a prop), so no redesign is needed here.
 */
import { useCallback, useEffect } from 'react';

import type { ToolkitTypeSchemaMap } from '@/entities/toolkit';
import { useSocketClient } from '@/shared/api/socket/client';
import { useListToolkits } from '@/shared/api/generated/toolkits/toolkits';

import { useSelectedProjectId } from './useSelectedProjectId';

export interface UseGetCurrentToolkitSchemasParams {
  readonly skip?: boolean;
  readonly isMCP?: boolean;
}

export interface UseGetCurrentToolkitSchemasResult {
  readonly toolkitSchemas: ToolkitTypeSchemaMap | undefined;
  readonly isFetching: boolean;
  /**
   * The read failed (#440). It used to be absent, so every caller rendered a
   * failed read as an empty schema map — the same empty screen a toolkit
   * with no schemas produces. A caller must keep the two apart.
   */
  readonly isError: boolean;
  /** Reads the schemas again. Connect it to the retry control of the error state. */
  readonly refetch: () => void;
}

export function useGetCurrentToolkitSchemas(params: UseGetCurrentToolkitSchemasParams = {}): UseGetCurrentToolkitSchemasResult {
  const { skip = false, isMCP = false } = params;
  const projectId = useSelectedProjectId();
  const socket = useSocketClient();

  const query = useListToolkits(projectId ?? '', { query: { enabled: !skip && projectId !== undefined } });
  // `.data.data`'s declared type includes the error-envelope variant — never
  // actually reachable here, since `eliteaFetch` throws instead of resolving
  // with it (mutator.ts's §3.6 unwrap contract).
  const toolkitSchemas = query.data?.data as ToolkitTypeSchemaMap | undefined;

  const handleMcpStatusEvent = useCallback(() => {
    if (isMCP) void query.refetch();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `query.refetch` is a stable TanStack Query identity per query key; re-subscribing on every render would thrash the socket listener for no behavioural gain
  }, [isMCP, query.refetch]);

  useEffect(() => {
    socket.on('mcp_status', handleMcpStatusEvent);
    return () => socket.off('mcp_status', handleMcpStatusEvent);
  }, [socket, handleMcpStatusEvent]);

  // A stable identity: a caller hands this straight to a retry control, and a
  // fresh closure every render would invalidate any memo it lands in.
  const { refetch: refetchQuery } = query;
  const refetch = useCallback(() => {
    void refetchQuery();
  }, [refetchQuery]);

  return {
    toolkitSchemas,
    isFetching: query.isFetching,
    isError: query.isError,
    refetch,
  };
}
