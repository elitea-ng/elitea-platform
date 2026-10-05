/**
 * The project-scoped usage read behind Settings › Usage — gap G13.
 *
 * `GET /elitea_core/usage/prompt_lib/{projectId}/usage` has existed since issue
 * #246 and had no caller. It is the PROJECT-scoped read, gated on
 * `models.project_context.view`, so an ordinary member reaches it; the
 * administration-mode twin that Admin › Budgets uses is a different route
 * behind a different permission.
 *
 * ## Redaction is a server decision, and it is visible
 *
 * A member who is neither a project admin nor the owner of a personal project
 * receives the same payload with `monthly_limit`, `effective_limit`, `spend`,
 * `remaining` and `currency` REMOVED, and `can_see_amounts: false`. The fields
 * are absent rather than zeroed, which is why every money field on this type is
 * optional and why the page reads `can_see_amounts` instead of inferring
 * redaction from a missing number.
 */
import { useCallback, useState } from 'react';

import { useQueryClient, useQuery, type UseQueryResult } from '@tanstack/react-query';

import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

export interface ProjectUsage {
  /** False means the five cost fields were removed from this payload. */
  readonly can_see_amounts?: boolean;
  readonly monthly_limit?: number | null;
  readonly effective_limit?: number | null;
  readonly limit_source?: string;
  readonly currency?: string;
  readonly enabled?: boolean;
  readonly warning_pct?: number;
  readonly spend?: number | null;
  readonly remaining?: number | null;
  /** Null when the scope is unlimited or the limit is zero — no denominator. */
  readonly percent_used?: number | null;
  /** Whether an accumulator row exists at all for this period. */
  readonly spend_available?: boolean;
  readonly period?: string;
  readonly period_start?: string;
  readonly period_end?: string;
  readonly resets_at?: string;
}

/**
 * `project` reads the project's shared budget; `user` reads the CALLER's own
 * member budget in that project (the server filters by the caller, never by a
 * parameter). A member budget refusal links to the `user` view (#6732).
 */
export type UsageScope = 'project' | 'user';

const projectUsageKeys = {
  all: ['settings', 'usage'] as const,
  project: (projectId: string, scope: UsageScope) =>
    ['settings', 'usage', scope, projectId] as const,
};

async function fetchProjectUsage(projectId: string, scope: UsageScope): Promise<ProjectUsage> {
  return unwrapBody(
    await eliteaFetch<unknown>(`/elitea_core/usage/prompt_lib/${projectId}/usage?scope=${scope}`),
  ) as ProjectUsage;
}

export function useProjectUsage(
  projectId: string | undefined,
  scope: UsageScope = 'project',
): UseQueryResult<ProjectUsage, Error> {
  return useQuery({
    queryKey: projectUsageKeys.project(projectId ?? '', scope),
    enabled: projectId !== undefined && projectId !== '',
    queryFn: () => fetchProjectUsage(projectId ?? '', scope),
  });
}

/**
 * The refresh's retry rule: only a SESSION failure (`kind: 'auth'`, the
 * re-auth race `app/providers/queryClient.ts` documents) is tried once more.
 * The app default also retries a 5xx after a 1s backoff, which for a click
 * the user is watching only delays the failure toast; the user can click again.
 */
function retryRefresh(failureCount: number, error: unknown): boolean {
  return failureCount < 1 && error instanceof EliteaApiError && error.failure.kind === 'auth';
}

export interface UsageRefresh {
  /** Resolves `true` when fresh data arrived, `false` when the refetch failed. */
  readonly refresh: () => Promise<boolean>;
  readonly isRefreshing: boolean;
}

/**
 * Settings › Usage's Refresh action (#6672).
 *
 * A FETCH of the active view's query, not an invalidation that a later
 * render picks up and not a re-render of the cache: the request goes out now
 * and the promise settles with its outcome. The query key is the same one
 * `useProjectUsage` reads, so the current scope (tab) and project are what is
 * refreshed and nothing else on the page is reset. A failed refetch leaves the
 * previous data in the cache — TanStack Query keeps `data` on a refetch error —
 * and reports `false` so the caller can say so.
 */
export function useRefreshProjectUsage(projectId: string | undefined, scope: UsageScope = 'project'): UsageRefresh {
  const queryClient = useQueryClient();
  const [isRefreshing, setIsRefreshing] = useState(false);
  const refresh = useCallback(async (): Promise<boolean> => {
    if (projectId === undefined || projectId === '') return false;
    setIsRefreshing(true);
    try {
      // `query` with `staleTime: 0` always goes to the network, and it
      // writes the same cache entry `useProjectUsage` reads — so the page
      // re-renders from it, and a failure leaves the previous `data` in place.
      await queryClient.query({
        queryKey: projectUsageKeys.project(projectId, scope),
        queryFn: () => fetchProjectUsage(projectId, scope),
        staleTime: 0,
        retry: retryRefresh,
      });
      return true;
    } catch {
      return false;
    } finally {
      setIsRefreshing(false);
    }
  }, [projectId, queryClient, scope]);
  return { refresh, isRefreshing };
}
