/**
 * The dynamic tool catalogue of a toolkit — the two routes every tool picker
 * in this app needs, and until #440 none of them called.
 *
 *   GET  /elitea_core/toolkit_available_tools/prompt_lib/{projectId}/{toolkitId}
 *   POST /elitea_core/toolkit_discover_tools/prompt_lib/{projectId}/{toolkitType}
 *
 * Both are registered in `services/elitea-main/internal/api/router.go`
 * (`toolkitHandler.AvailableTools` / `toolkitHandler.DiscoverTools`). Neither
 * is in `api/openapi/v2.yaml`, so orval generates no hook for them. This
 * module is the hand-written client `features/toolkits/indexes/api/
 * indexesApi.ts` already sets the pattern for, and both routes are recorded
 * in `shared/api/endpoints.manifest.json` per R-A5.
 *
 * WHY `entities/`, NOT `features/toolkits/api/`. Tool pickers in BOTH
 * `features/toolkits` and `features/pipelines` need the same catalogue, and
 * `no-sideways-features` forbids one feature slice to import another.
 * `ui/test-tools/TestToolSettings.tsx` and `ui/select/LoopToolSelect.tsx`
 * read it today; the deprecated pipeline nodes still do not — see
 * `ui/nodes/deprecated/useToolNodeEditing.ts`'s own header.
 *
 * FAILURE IS NOT EMPTINESS (#381, #440). Both handlers used to answer a
 * failed database read with `200 {"tools":[],"total":0}`. They now answer
 * 500, and `eliteaFetch` throws on it. `isError` is therefore a real signal
 * and every caller must render it as its own state: an empty picker means
 * "this toolkit offers no tools", never "the read failed".
 *
 * ENVELOPE. `eliteaFetch<T>` resolves orval's `{data, status, headers}`
 * envelope, not the body. Typing the call as the BODY is the recurring
 * defect (#132): every field reads back `undefined` on a 200. `fetchBody`
 * below is the single unwrap point.
 */
import { useCallback, useMemo } from 'react';

import { useQuery } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';

/**
 * One row of `writeJSON(w, 200, {"tools": tools, "total": len(tools)})` —
 * `toolkits.Tool` in `internal/api/v2/toolkits/handler.go:21-28`.
 *
 * Intra-slice only. No caller outside this slice names the row type: pickers
 * read `useToolkitTools().tools` and let it infer. Export it, and add it to
 * `entities/toolkit/index.ts`, when a cross-slice caller needs the name.
 */
interface ToolkitTool {
  readonly id: string;
  readonly name: string;
  readonly type: string;
  readonly description?: string;
  readonly owner_id?: string;
  readonly settings?: Readonly<Record<string, unknown>>;
}

/** The body both handlers write. `total` is `len(tools)`, carried for parity; the pickers read `tools`. */
export interface ToolkitToolsPayload {
  readonly args_schemas?: Readonly<Record<string, Readonly<Record<string, unknown>>>>;
  readonly tools?: readonly ToolkitTool[];
  readonly total?: number;
}

/** The one place the `{data, status, headers}` envelope is opened — see the module header. */
async function fetchBody<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

const TOOLKIT_TOOLS_QUERY_ROOT = ['toolkits', 'tools'] as const;

/** A payload with no `tools` key is a real empty list, not a failure: the handler always writes the key, and a body that lost it still means "no tools". */
function readTools(payload: ToolkitToolsPayload | undefined): readonly ToolkitTool[] {
  return payload?.tools ?? [];
}

export interface AvailableToolsParams {
  readonly projectId: string;
  readonly toolkitId: string;
}

export async function fetchAvailableTools(params: AvailableToolsParams, signal?: AbortSignal): Promise<ToolkitToolsPayload> {
  return fetchBody<ToolkitToolsPayload>(
    `/elitea_core/toolkit_available_tools/prompt_lib/${params.projectId}/${params.toolkitId}`,
    signal ? { signal } : {},
  );
}

export interface DiscoverToolsParams {
  readonly projectId: string;
  readonly toolkitType: string;
}

export async function discoverToolkitTools(params: DiscoverToolsParams, signal?: AbortSignal): Promise<ToolkitToolsPayload> {
  return fetchBody<ToolkitToolsPayload>(
    `/elitea_core/toolkit_discover_tools/prompt_lib/${params.projectId}/${params.toolkitType}`,
    { method: 'POST', ...(signal ? { signal } : {}) },
  );
}

export interface UseToolkitToolsParams {
  /** The project that owns the toolkit. An empty value disables both queries. */
  readonly projectId: string | undefined;
  /** A saved toolkit instance. Present: read its own attached tools. */
  readonly toolkitId?: string | undefined;
  /** A toolkit type. Used when there is no instance id yet — a toolkit still being created. */
  readonly toolkitType?: string | undefined;
  /** Caller-side gate, ANDed with the checks above. */
  readonly enabled?: boolean;
}

export interface UseToolkitToolsResult {
  readonly tools: readonly ToolkitTool[];
  readonly toolNames: readonly string[];
  readonly isFetching: boolean;
  /** The read failed. Render this as its own state — never as an empty list. */
  readonly isError: boolean;
  /** The read ran, succeeded, and the toolkit offers no tools. Distinct from `isError`. */
  readonly isEmpty: boolean;
  readonly refetch: () => void;
}

/**
 * Reads the tool catalogue of one toolkit.
 *
 * The instance route wins when `toolkitId` is known, because the tools
 * attached to a saved toolkit are the exact set the user may run. The type
 * route is the fallback for a toolkit that has no id yet.
 */
export function useToolkitTools(params: UseToolkitToolsParams): UseToolkitToolsResult {
  const { projectId, toolkitId, toolkitType, enabled = true } = params;
  const byInstance = toolkitId !== undefined && toolkitId !== '';
  const key = byInstance ? `id:${toolkitId}` : `type:${toolkitType ?? ''}`;
  const isAddressable = projectId !== undefined && projectId !== '' && (byInstance || (toolkitType !== undefined && toolkitType !== ''));

  const query = useQuery({
    queryKey: [...TOOLKIT_TOOLS_QUERY_ROOT, projectId ?? '', key],
    queryFn: ({ signal }) =>
      byInstance
        ? fetchAvailableTools({ projectId: projectId ?? '', toolkitId: toolkitId ?? '' }, signal)
        : discoverToolkitTools({ projectId: projectId ?? '', toolkitType: toolkitType ?? '' }, signal),
    enabled: enabled && isAddressable,
    retry: false,
  });

  const tools = useMemo(() => readTools(query.data), [query.data]);
  const toolNames = useMemo(() => tools.map((tool) => tool.name).filter((name) => name !== ''), [tools]);

  // A stable identity: a caller passes this straight to a retry control, and
  // a fresh closure every render would invalidate any memo it lands in.
  const { refetch: refetchQuery } = query;
  const refetch = useCallback(() => {
    void refetchQuery();
  }, [refetchQuery]);

  return {
    tools,
    toolNames,
    isFetching: query.isFetching,
    isError: query.isError,
    isEmpty: query.isSuccess && tools.length === 0,
    refetch,
  };
}
