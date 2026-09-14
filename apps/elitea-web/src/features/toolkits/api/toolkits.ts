/**
 * Ported from `apps/elitea-ui/src/api/toolkits.js` (518 lines, RTK Query
 * `toolkitsApi` slice) — unit A4g.
 *
 * REAL, EXHAUSTIVELY VERIFIED BACKEND GAP (grepped `shared/api/
 * endpoints.manifest.json` for every `tool`/`toolkit` operation, and read
 * `shared/api/generated/toolkits/toolkits.ts` +
 * `shared/api/generated/applications/applications.ts` directly — not
 * assumed). Of the baseline's 14 exported RTK Query hooks, FOUR have a real
 * endpoint anywhere in this worktree:
 *
 *   - `useToolkitTypesQuery`     -> `useListToolkits`          (real,
 *     generated-client-backed)
 *   - `useToolkitsListQuery`     -> `useListToolkitInstances`  (real,
 *     generated-client-backed)
 *   - `useToolkitDeleteMutation` -> `useDeleteApplicationTool` (real,
 *     generated-client-backed — filed under the generated client's
 *     "applications" tag even though the baseline groups it with toolkits;
 *     both hit the SAME `DELETE /elitea_core/tool/prompt_lib/{project_id}/
 *     {tool_id}` path)
 *   - `toolkitsDetails`          -> `useToolkitDetail` (below) — real,
 *     HANDWRITTEN (not generated-client-backed; no orval hook exists for
 *     this one). CORRECTION to an earlier, FALSE claim this comment used to
 *     make ("no GET-single endpoint exists anywhere"): it does.
 *     `services/elitea-main/internal/api/router.go:460` registers
 *     `GET /tool/prompt_lib/{projectID}/{toolkitID} -> toolkitHandler.Get`,
 *     whose `GetToolkit` repo method (`internal/api/v2/toolkits/
 *     handler.go:940-970`) returns the exact single-toolkit record the
 *     baseline's `toolkitsDetails` endpoint used. This same endpoint is
 *     already registered in `shared/api/endpoints.manifest.json` as
 *     `toolkits.getIndexSchedule` (`source: "handwritten"`) and already
 *     called the same way — `eliteaFetch` against the literal path, no
 *     generated hook — by this very slice's sibling file `features/
 *     toolkits/indexes/api/indexesApi.ts`'s `getIndexSchedule`.
 *     `useToolkitDetail` below now calls it directly instead of
 *     approximating it by paging through the toolkit-instance list and
 *     doing a client-side `.find()`, which silently failed to resolve any
 *     toolkit past the first page.
 *
 * CORRECTION (Phase 1c, 2026-08-07) — the "10 remaining exports have NO
 * endpoint anywhere" claim this comment used to make was WRONG, and the
 * re-verification it asked for has now been done. Every one of the ten is
 * registered in `services/elitea-main/internal/api/router.go`:
 *
 *   toolkitCreate         POST   /tools/prompt_lib/{projectID}            :647
 *   toolkitEdit           PUT    /tool/prompt_lib/{projectID}/{toolkitID} :649
 *   toolkitFork           POST   /fork_toolkit/prompt_lib/{projectID}     :658
 *   toolkitExport         GET    /export_toolkit/...                      :661
 *   toolkitTest           POST   /test_tool/..., /test_toolkit_tool/...   :659-660
 *   mcpSyncTools          POST   /mcp_sync_tools/prompt_lib/{projectID}   :886
 *   discoverMcpTools      POST   /toolkit_discover_tools/...              :1914
 *   validateToolkit       GET+POST /toolkit_validator/...                 :656-657
 *   toolkitAvailableTools GET    /toolkit_available_tools/...             :1912
 *   listToolkitTypes      GET    /toolkit_types/prompt_lib/{projectID}    :653
 *
 * The gap was never in the BACKEND — it was in the OpenAPI spec, and hence
 * in the generated client. `create`/`update` (plus the single-toolkit GET)
 * were added to `api/openapi/v2.yaml` in Phase 1c and are now real generated
 * operations, consumed by `useToolkitCreate`/`useToolkitEdit` below.
 *
 * The other seven remain unspec'd. They are genuine follow-up work, but the
 * work is "add the operation to v2.yaml and regenerate", NOT "implement a
 * handler" — do not repeat this comment's original mistake of treating a
 * missing spec entry as a missing endpoint.
 *
 * UPDATE (#440): two of those seven now have a real, hand-written client.
 * `toolkit_available_tools` and `toolkit_discover_tools` are read by
 * `entities/toolkit`'s `api/toolkitToolsApi.ts` and are recorded in
 * `shared/api/endpoints.manifest.json` as `toolkits.availableTools` /
 * `toolkits.discoverTools`. They live in `entities/` rather than here
 * because `features/pipelines` needs them too and `no-sideways-features`
 * forbids it to import this slice. Adding the operations to v2.yaml and
 * moving both onto generated hooks remains the follow-up.
 *
 * Consistent with the established convention for exactly this situation
 * (`useValidateToolkit`'s injected `useValidateToolkitQuery`,
 * `entities/application-form`'s injected `ApplicationValidator.useValidate`),
 * every one of the 10 remaining gaps is exposed here as a TYPE ONLY — the
 * shape a real generated hook would need to satisfy — for a caller in this
 * same unit (`SaveToolkitButton`/`CreateToolkitButton`/`ToolkitEditor`/
 * `CreateToolkitToolTabBar`) to accept as an injected `deps` prop. No fake
 * network call is invented for any of them; each call site's own doc
 * comment repeats the specific gap it works around.
 *
 * `useToolkitDetail` below is NOT a baseline export by that name, but it IS
 * the real, direct replacement for the baseline's `toolkitsDetails` query —
 * see the correction above. It calls `GET /elitea_core/tool/prompt_lib/
 * {projectId}/{toolkitId}` via `eliteaFetch` (the same handwritten-endpoint
 * pattern `indexesApi.ts`'s `getIndexSchedule` already uses) and returns
 * the single-toolkit row directly, typed as `ToolkitInstance` — the real
 * response carries additional fields (`toolkit_name`/`author`/`agent_type`/
 * `online`) `ToolkitInstance` doesn't declare, which is fine: they're extra
 * properties on a non-literal value, not a mismatch.
 *
 * The baseline's two dead, already-commented-out blocks (`toolkitToolTest`
 * mutation — its own comment says it was "replaced with socket
 * implementation using test_toolkit_tool event"; an alternate `toolkitFork`
 * query body) are DROPPED, not carried forward as commented-out code (house
 * "no placeholder code" rule). `toolkitFork` itself (the LIVE export) is
 * also dropped: the baseline's own comment marks it "@todo: temporary
 * solution", it has no caller in this unit's owned files, and it has no
 * generated endpoint either.
 */
import { useCallback, useMemo } from 'react';

import { useQuery, useQueryClient } from '@tanstack/react-query';

import { getDeleteApplicationToolQueryOptions } from '@/shared/api/generated/applications/applications';
import { eliteaFetch } from '@/shared/api/generated/mutator';
import {
  createToolkit,
  updateToolkit,
  useListToolkitInstances,
  useListToolkits,
} from '@/shared/api/generated/toolkits/toolkits';
import type { ToolkitInstance } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';

/* ── real: toolkit-type settings-schema catalogue ─────────────────────────── */

export interface UseToolkitTypesParams {
  readonly projectId: string | undefined;
}

export interface UseToolkitTypesResult {
  readonly toolkitTypes: Readonly<Record<string, unknown>> | undefined;
  readonly isFetching: boolean;
  readonly isError: boolean;
}

/** `GET /elitea_core/toolkits/prompt_lib/{projectId}` — same endpoint `features/toolkits/lib/hooks/useGetCurrentToolkitSchemas.hooks.ts` (A4b) already wraps for schema-catalogue callers; this is the raw, unenriched read the baseline's own `useToolkitTypesQuery` exposed. */
export function useToolkitTypes({ projectId }: UseToolkitTypesParams): UseToolkitTypesResult {
  const query = useListToolkits(projectId ?? '', { query: { enabled: projectId !== undefined } });
  // `.data.data`'s declared type includes the error-envelope variant — never
  // actually reachable here, since `eliteaFetch` throws instead of resolving
  // with it (mutator.ts's §3.6 unwrap contract).
  const toolkitTypes = query.data?.data as Readonly<Record<string, unknown>> | undefined;
  return { toolkitTypes, isFetching: query.isFetching, isError: query.isError };
}

/* ── real: paginated toolkit-instance list ────────────────────────────────── */

export interface UseToolkitsListParams {
  readonly projectId: string | undefined;
  readonly page: number;
  readonly pageSize: number;
}

export interface UseToolkitsListResult {
  readonly rows: readonly ToolkitInstance[];
  readonly total: number;
  readonly isFetching: boolean;
  readonly isError: boolean;
  readonly refetch: () => void;
}

/**
 * `GET /elitea_core/tools/prompt_lib/{projectId}` — same endpoint
 * `features/toolkits/lib/hooks/useLoadToolkits.ts` (A4b) already wraps with
 * richer pagination/tag/icon orchestration for the browse list; this is the
 * raw row read this unit's own `useToolkitDetail` (below) is built on. The
 * baseline's `query`/`sort_by`/`toolkit_type`/etc. filter params have no
 * generated-endpoint equivalent (only `limit`/`offset` exist — see
 * `useLoadToolkits.ts`'s own doc comment, deviation 1) and are not
 * reproduced here either.
 */
export function useToolkitsList({ projectId, page, pageSize }: UseToolkitsListParams): UseToolkitsListResult {
  const query = useListToolkitInstances(
    projectId ?? '',
    { limit: pageSize, offset: page * pageSize },
    { query: { enabled: projectId !== undefined } },
  );
  // Unwrapped by the one helper (R-A6, #132) instead of a per-call-site cast:
  // the cast asserted `{rows,total}` rather than checking it, so any other
  // shape read as an empty page with a 200 in the network tab. Memoised on
  // `query.data` because `unwrapListPage` returns fresh arrays.
  const data = useMemo(() => unwrapListPage<ToolkitInstance>(query.data, 'listToolkitInstances'), [query.data]);

  const refetch = useCallback(() => {
    void query.refetch();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `query.refetch` is a stable TanStack Query identity per query key
  }, [query.refetch]);

  return { rows: data.rows, total: data.total, isFetching: query.isFetching, isError: query.isError, refetch };
}

/* ── real: single-toolkit detail (GET /elitea_core/tool/prompt_lib/{projectId}/{toolkitId}) ── */

export interface UseToolkitDetailParams {
  readonly projectId: string | undefined;
  readonly toolkitId: string | undefined;
}

export interface UseToolkitDetailResult {
  readonly detail: ToolkitInstance | undefined;
  readonly isFetching: boolean;
  readonly isError: boolean;
}

const TOOLKIT_DETAIL_QUERY_ROOT = ['toolkits', 'detail'] as const;

/**
 * `GET /elitea_core/tool/prompt_lib/{projectId}/{toolkitId}` — the baseline
 * `toolkitsDetails` endpoint's direct equivalent (see module doc comment's
 * correction). Handwritten, not generated-client-backed (no orval hook
 * exists for this operation — same situation `indexesApi.ts`'s
 * `getIndexSchedule` is already in for the identical path), so this calls
 * `eliteaFetch` directly rather than an `useXxx` generated hook.
 * `eliteaFetch<T>` always resolves the mutator's own `{data, status,
 * headers}` envelope (mutator.ts's documented contract) regardless of
 * whether the backend's own response body is itself enveloped — the Go
 * handler (`toolkitHandler.Get`) writes the single-toolkit row directly at
 * the top level, so `envelope.data` IS that row.
 */
async function fetchToolkitDetail(projectId: string, toolkitId: string, signal?: AbortSignal): Promise<ToolkitInstance> {
  const envelope = await eliteaFetch<{ data: ToolkitInstance }>(
    `/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`,
    signal ? { signal } : {},
  );
  return envelope.data;
}

export function useToolkitDetail({ projectId, toolkitId }: UseToolkitDetailParams): UseToolkitDetailResult {
  const query = useQuery({
    queryKey: [...TOOLKIT_DETAIL_QUERY_ROOT, projectId, toolkitId],
    queryFn: ({ signal }) => fetchToolkitDetail(projectId as string, toolkitId as string, signal),
    enabled: projectId !== undefined && toolkitId !== undefined,
  });
  return { detail: query.data, isFetching: query.isFetching, isError: query.isError };
}

/* ── real: delete ──────────────────────────────────────────────────────────── */

export interface UseToolkitDeleteResult {
  readonly deleteToolkit: (args: { readonly projectId: string; readonly toolkitId: string }) => Promise<void>;
  readonly isDeleting: boolean;
}

/**
 * `DELETE /elitea_core/tool/prompt_lib/{projectId}/{toolkitId}` —
 * `useDeleteApplicationTool` (generated, `applications.ts`). Orval generated
 * this as a `useQuery`-shaped hook, not `useMutation` (same finding
 * `entities/application-form/model/mutations.ts`'s own doc comment made for
 * the applications endpoints) — the established imperative-trigger
 * convention is `queryClient.query(getXQueryOptions(...))`, used here
 * via the standalone `useDeleteToolkit` hook rather than the query-shaped
 * `useDeleteApplicationTool` directly, so callers get a plain
 * loading/imperative-call surface instead of having to gate a `useQuery` on
 * a not-yet-clicked button.
 */
export function useToolkitDelete(): UseToolkitDeleteResult {
  const queryClient = useQueryClient();
  const deleteToolkit = useCallback(
    async ({ projectId, toolkitId }: { readonly projectId: string; readonly toolkitId: string }): Promise<void> => {
      const options = getDeleteApplicationToolQueryOptions(projectId, Number(toolkitId));
      await queryClient.query(options);
    },
    [queryClient],
  );
  // No generated-endpoint-backed loading flag exists for an imperative
  // `fetchQuery` call (unlike a real `useMutation`'s `isPending`) — callers
  // that need a spinner track it themselves around the returned promise,
  // same shape `entities/application-form/model/mutations.ts`'s
  // `isCreating`/`isSaving` local `useState` establishes; this hook stays a
  // pure network primitive.
  return { deleteToolkit, isDeleting: false };
}

/* ── type-only: the 10 remaining CRUD/utility gaps (see module doc comment) ── */

/** Shape a real `POST /elitea_core/tools/prompt_lib/{projectId}` mutation would need to satisfy. Injected by `CreateToolkitButton`/`CreateToolkitToolTabBar` callers — no generated endpoint exists yet. */
export interface ToolkitWriteBody {
  readonly type: string;
  readonly name?: string | undefined;
  readonly description?: string | undefined;
  readonly settings?: Readonly<Record<string, unknown>> | undefined;
  readonly meta?: Readonly<Record<string, unknown>> | undefined;
}

export interface ToolkitWriteResult {
  readonly id: string;
  readonly type: string;
  readonly name: string;
  readonly is_mcp?: boolean;
  readonly version_details?: {
    readonly id?: string | number;
    readonly variables?: readonly unknown[];
    readonly meta?: { readonly icon_meta?: unknown };
  };
}

export type UseToolkitCreateMutation = (args: { readonly projectId: string } & ToolkitWriteBody) => Promise<ToolkitWriteResult>;

export type UseToolkitEditMutation = (
  args: { readonly projectId: string; readonly toolId: string } & ToolkitWriteBody,
) => Promise<ToolkitWriteResult>;

/* ── real: create / edit (Phase 1c — see the CORRECTION in the module doc) ── */

/**
 * `POST /elitea_core/tools/prompt_lib/{projectId}` — generated `createToolkit`.
 *
 * Returns the SAME `UseToolkitCreateMutation` shape the injected stub used, so
 * callers that already thread `deps.createToolkit` keep working unchanged;
 * they can now simply stop injecting and take this default instead.
 *
 * Metadata includes the saved MCP sharing option. Main persists it in the existing toolkit row.
 */
export function useToolkitCreate(): UseToolkitCreateMutation {
  return useCallback(async ({ projectId, type, name, description, settings, meta }) => {
    const response = await createToolkit(projectId, {
      type,
      ...(name === undefined ? {} : { name }),
      ...(description === undefined ? {} : { description }),
      ...(settings === undefined ? {} : { settings }),
      ...(meta === undefined ? {} : { meta }),
    });
    return response.data as ToolkitWriteResult;
  }, []);
}

/**
 * `PUT /elitea_core/tool/prompt_lib/{projectId}/{toolId}` — generated
 * `updateToolkit`. PATCH hits the same handler; only PUT is spec'd.
 *
 * NEVER send `has_relation`: that key makes the handler dispatch to
 * `updateToolRelation` instead of updating the toolkit at all (issue #38).
 * `ToolkitWriteBody` cannot express it, which is the intended guard.
 */
export function useToolkitEdit(): UseToolkitEditMutation {
  return useCallback(async ({ projectId, toolId, type, name, description, settings, meta }) => {
    const response = await updateToolkit(projectId, Number(toolId), {
      type,
      ...(name === undefined ? {} : { name }),
      ...(description === undefined ? {} : { description }),
      ...(settings === undefined ? {} : { settings }),
      ...(meta === undefined ? {} : { meta }),
    });
    return response.data as ToolkitWriteResult;
  }, []);
}

// `UseToolkitExportQuery` (the injected-`deps` shape this comment block
// originally anticipated for the export gap — see the module doc comment's
// "type only" convention) was REMOVED: `ExportToolkitButton.tsx` (this
// unit's own real file) did not end up needing dependency injection for
// this one gap after all — its own `fetchToolkitExport` calls `eliteaFetch`
// against `GET /elitea_core/export_toolkit/prompt_lib/{projectId}/{id}`
// directly (see that file's own doc comment), since the endpoint IS
// reachable via the shared HTTP primitive even without a generated,
// orval-typed hook. No caller anywhere in this worktree ever referenced
// this type by name.
