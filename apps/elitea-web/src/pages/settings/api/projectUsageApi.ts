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
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
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

const projectUsageKeys = {
  all: ['settings', 'usage'] as const,
  project: (projectId: string) => ['settings', 'usage', 'project', projectId] as const,
};

export function useProjectUsage(
  projectId: string | undefined,
): UseQueryResult<ProjectUsage, Error> {
  return useQuery({
    queryKey: projectUsageKeys.project(projectId ?? ''),
    enabled: projectId !== undefined && projectId !== '',
    queryFn: async (): Promise<ProjectUsage> =>
      unwrapBody(
        await eliteaFetch<unknown>(
          `/elitea_core/usage/prompt_lib/${projectId ?? ''}/usage?scope=project`,
        ),
      ) as ProjectUsage,
  });
}
