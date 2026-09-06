/**
 * Local duplicate of `features/apps/api/useToolkitTypeSchemas.ts` (that
 * file's own doc comment: "Replaces the baseline's `useGetCurrentToolkitSchemas`
 * (`features/toolkits/lib/hooks` — that slice, unit A4, has not landed;
 * this hook calls the same underlying generated endpoint directly rather
 * than reaching into A4's ownership)"). Duplicated, not imported:
 * `no-sideways-features` forbids `features/pipelines` reaching into
 * `features/apps` either.
 *
 * Thin wrapper over the generated `useListToolkits` (GET
 * `/elitea_core/toolkits/prompt_lib/{projectId}` — despite the name, the
 * toolkit-TYPE settings-schema catalogue, not a list of instances; see the
 * generated file's own `NOTE(W2)` comment).
 *
 * **Real, disclosed gap this hook does NOT attempt to paper over:** the
 * baseline's `useGetCurrentToolkitSchemas` also refetches on the
 * `mcp_status` socket event (`sioEvents.mcp_status`) when `isMCP` is set.
 * No generated endpoint or socket-driven cache invalidation for that
 * exists here yet — `useFunctionInputMapping.ts`'s own header discloses
 * the follow-on consequence (dynamic MCP tool schemas cannot be kept live
 * this way either).
 */
import { useCallback } from 'react';

import type { ToolkitTypeSchemaMap } from '@/entities/toolkit';
import { useListToolkits } from '@/shared/api/generated/toolkits/toolkits';

export interface UseToolkitTypeSchemasResult {
  readonly toolkitTypeSchemas: ToolkitTypeSchemaMap | undefined;
  readonly isFetching: boolean;
  /**
   * The read failed (#440). Every node that builds a tool picker from these
   * schemas must render this as its own state: an empty picker means the
   * toolkit offers no tools, never that the read was lost.
   */
  readonly isError: boolean;
  /** Reads the schemas again. Connect it to the retry control of the error state. */
  readonly refetch: () => void;
}

export function useToolkitTypeSchemas(projectId: string | undefined): UseToolkitTypeSchemasResult {
  const query = useListToolkits(projectId ?? '', { query: { enabled: projectId !== undefined } });
  // `.data.data`'s declared type includes the error-envelope variant — never
  // actually reachable here since `eliteaFetch` throws instead of resolving
  // with it (mutator.ts's §3.6 unwrap contract).
  const toolkitTypeSchemas = query.data?.data as ToolkitTypeSchemaMap | undefined;

  // A stable identity: a caller hands this straight to a retry control, and a
  // fresh closure every render would invalidate any memo it lands in.
  const { refetch: refetchQuery } = query;
  const refetch = useCallback(() => {
    void refetchQuery();
  }, [refetchQuery]);

  return {
    toolkitTypeSchemas,
    isFetching: query.isFetching,
    isError: query.isError,
    refetch,
  };
}
