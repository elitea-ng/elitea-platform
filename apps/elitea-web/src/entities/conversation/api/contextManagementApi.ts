/**
 * Hand-written REST layer for the 8 context-management endpoints of
 * `chat.api.js` (unit C1) — same handwritten-manifest rationale as
 * `./conversationApi.ts`'s module doc (read that first).
 *
 * Two distinct base paths, both real wired Go routes (confirmed against
 * `services/elitea-main/internal/api/router.go`):
 *  - `getContextStatus`/`updateContextStrategy` mount under `/elitea_core`
 *    (`router.go:421-422`, `GetContextAnalytics`/`UpdateContextStrategy`).
 *    NOTE the baseline's own naming mismatch, preserved: `getContextStatus`
 *    calls the `context_analytics` PATH (not `context_status`) —
 *    `chat.api.js:317-326`'s own `query` literal, byte-for-byte.
 *  - The other 6 (`optimizeContext`/`getContextAnalytics`/`generateSummary`/
 *    `getConversationSummaries`/`updateSummary`/`deleteSummary`) mount under
 *    a SIBLING top-level `/context_manager` route group
 *    (`router.go:610-619`, `r.Route("/context_manager", ...)`), NOT nested
 *    under `/elitea_core` — matching `chat.api.js:341-432`'s own
 *    `/context_manager/...` URLs, which never prepend `apiSlicePath`.
 */
import { useMutation, useQuery, type UseMutationResult, type UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';

async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

interface ConversationScopedParams {
  readonly projectId: string | number;
  readonly conversationId: string | number;
}

/* ── getContextStatus — GET elitea_core/context_analytics/prompt_lib/{projectId}/{conversationId} ── */
/* manifest: conversation.getContextStatus */

export type ContextStatusWire = Readonly<Record<string, unknown>>;

export async function getContextStatus(params: ConversationScopedParams, signal?: AbortSignal): Promise<ContextStatusWire> {
  return fetchData<ContextStatusWire>(
    `/elitea_core/context_analytics/prompt_lib/${String(params.projectId)}/${String(params.conversationId)}`,
    signal ? { signal } : {},
  );
}

/**
 * The cache key of the context-status query, exported so a WRITER can
 * invalidate it by the key the READER actually uses.
 *
 * DEFECT this closes. `widgets/context-budget`'s pencil invalidated
 * `['GET', '/elitea_core/context_analytics/prompt_lib']` — the URL-shaped
 * namespace a generated client would use, which nothing in this app has ever
 * registered a query under. TanStack Query matches prefixes structurally, so
 * that call matched zero queries and returned successfully: the panel kept
 * reporting the budget the reader had just replaced until a reload. Calling
 * this factory with no arguments yields the PREFIX (`['conversation',
 * 'contextStatus']`), which invalidates every conversation's status.
 */
export function contextStatusQueryKey(params?: ConversationScopedParams): readonly unknown[] {
  const root = ['conversation', 'contextStatus'] as const;
  return params === undefined ? root : [...root, params.projectId, params.conversationId];
}

export function useGetContextStatusQuery(params: ConversationScopedParams, options: { enabled?: boolean } = {}): UseQueryResult<ContextStatusWire> {
  return useQuery({
    queryKey: contextStatusQueryKey(params),
    queryFn: ({ signal }) => getContextStatus(params, signal),
    enabled: options.enabled ?? true,
  });
}

/* ── updateContextStrategy — PUT elitea_core/context_strategy/prompt_lib/{projectId}/{conversationId} ── */
/* manifest: conversation.updateContextStrategy */

export interface UpdateContextStrategyParams extends ConversationScopedParams {
  readonly [key: string]: unknown;
}

export async function updateContextStrategy(params: UpdateContextStrategyParams): Promise<unknown> {
  const { projectId, conversationId, ...body } = params;
  return fetchData<unknown>(`/elitea_core/context_strategy/prompt_lib/${String(projectId)}/${String(conversationId)}`, {
    method: 'PUT',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useUpdateContextStrategyMutation(): UseMutationResult<unknown, unknown, UpdateContextStrategyParams> {
  return useMutation({ mutationFn: updateContextStrategy });
}

/* ── optimizeContext — POST context_manager/optimize_context/{projectId}/{conversationId} ── */
/* manifest: conversation.optimizeContext */

export interface OptimizeContextParams extends ConversationScopedParams {
  readonly [key: string]: unknown;
}

export async function optimizeContext(params: OptimizeContextParams): Promise<unknown> {
  const { projectId, conversationId, ...body } = params;
  return fetchData<unknown>(`/context_manager/optimize_context/${String(projectId)}/${String(conversationId)}`, {
    method: 'POST',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useOptimizeContextMutation(): UseMutationResult<unknown, unknown, OptimizeContextParams> {
  return useMutation({ mutationFn: optimizeContext });
}

/* ── getContextAnalytics — GET context_manager/analytics/{projectId}/{conversationId} ── */
/* manifest: conversation.getContextAnalytics */

export type ContextAnalyticsWire = Readonly<Record<string, unknown>>;

export async function getContextAnalytics(params: ConversationScopedParams, signal?: AbortSignal): Promise<ContextAnalyticsWire> {
  return fetchData<ContextAnalyticsWire>(`/context_manager/analytics/${String(params.projectId)}/${String(params.conversationId)}`, signal ? { signal } : {});
}

export function useGetContextAnalyticsQuery(params: ConversationScopedParams, options: { enabled?: boolean } = {}): UseQueryResult<ContextAnalyticsWire> {
  return useQuery({
    queryKey: ['conversation', 'contextAnalytics', params.projectId, params.conversationId],
    queryFn: ({ signal }) => getContextAnalytics(params, signal),
    enabled: options.enabled ?? true,
  });
}

/* ── generateSummary — POST context_manager/summaries/{projectId}/{conversationId} ── */
/* manifest: conversation.generateSummary */

export type SummaryWire = Readonly<Record<string, unknown>>;

export interface GenerateSummaryParams extends ConversationScopedParams {
  readonly [key: string]: unknown;
}

export async function generateSummary(params: GenerateSummaryParams): Promise<SummaryWire> {
  const { projectId, conversationId, ...body } = params;
  return fetchData<SummaryWire>(`/context_manager/summaries/${String(projectId)}/${String(conversationId)}`, {
    method: 'POST',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useGenerateSummaryMutation(): UseMutationResult<SummaryWire, unknown, GenerateSummaryParams> {
  return useMutation({ mutationFn: generateSummary });
}

/* ── getConversationSummaries — GET context_manager/summaries/{projectId}/{conversationId} ── */
/* manifest: conversation.getConversationSummaries */

export interface GetConversationSummariesParams extends ConversationScopedParams {
  readonly limit?: number;
  readonly offset?: number;
}

export interface SummaryListWire {
  readonly summaries?: readonly SummaryWire[];
  readonly [key: string]: unknown;
}

export async function getConversationSummaries(params: GetConversationSummariesParams, signal?: AbortSignal): Promise<SummaryListWire> {
  const query = new URLSearchParams({ limit: String(params.limit ?? 10), offset: String(params.offset ?? 0) });
  return fetchData<SummaryListWire>(
    `/context_manager/summaries/${String(params.projectId)}/${String(params.conversationId)}?${query.toString()}`,
    signal ? { signal } : {},
  );
}

/** Baseline (`chat.api.js:379-406`) accumulates pages onto a running `summaries[]` when `offset > 0` — same page-orchestration scoping cut as `./messageApi.ts`'s `messageList` doc comment; this hook exposes the plain page fetch, the caller owns the accumulation. */
export function useGetConversationSummariesQuery(
  params: GetConversationSummariesParams,
  options: { enabled?: boolean } = {},
): UseQueryResult<SummaryListWire> {
  return useQuery({
    queryKey: ['conversation', 'summaries', params.projectId, params.conversationId, params.limit, params.offset],
    queryFn: ({ signal }) => getConversationSummaries(params, signal),
    enabled: options.enabled ?? true,
  });
}

/* ── updateSummary — PUT context_manager/summary/{projectId}/{conversationId}/{summaryId} ── */
/* manifest: conversation.updateSummary */

export interface UpdateSummaryParams extends ConversationScopedParams {
  readonly summaryId: string | number;
  readonly [key: string]: unknown;
}

export async function updateSummary(params: UpdateSummaryParams): Promise<SummaryWire> {
  const { projectId, conversationId, summaryId, ...body } = params;
  return fetchData<SummaryWire>(`/context_manager/summary/${String(projectId)}/${String(conversationId)}/${String(summaryId)}`, {
    method: 'PUT',
    body: JSON.stringify(body),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useUpdateSummaryMutation(): UseMutationResult<SummaryWire, unknown, UpdateSummaryParams> {
  return useMutation({ mutationFn: updateSummary });
}

/* ── deleteSummary — DELETE context_manager/summary/{projectId}/{conversationId}/{summaryId} ── */
/* manifest: conversation.deleteSummary */

export interface DeleteSummaryParams extends ConversationScopedParams {
  readonly summaryId: string | number;
}

export async function deleteSummary(params: DeleteSummaryParams): Promise<unknown> {
  return fetchData<unknown>(`/context_manager/summary/${String(params.projectId)}/${String(params.conversationId)}/${String(params.summaryId)}`, {
    method: 'DELETE',
  });
}

export function useDeleteSummaryMutation(): UseMutationResult<unknown, unknown, DeleteSummaryParams> {
  return useMutation({ mutationFn: deleteSummary });
}
