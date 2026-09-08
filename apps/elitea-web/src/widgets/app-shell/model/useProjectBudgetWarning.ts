/**
 * The project budget-threshold read behind the warning banner (issue 312).
 *
 * `GET /elitea_core/project_budget/prompt_lib/{projectId}/budget` is the
 * member-reachable half of the budgets family. It has existed since #322 with
 * NOTHING calling it — `internal/api/v2/admin/config_schemas.go` says so in its
 * own words — so a project heading for its ceiling got no warning at all, only
 * a hard refusal at 100% from the gateway. This hook is the caller.
 *
 * ## The server decides whether to warn
 *
 * `warning_active` is answered by the server, not derived here. The threshold a
 * scope resolves is the project's own `soft_alert_pct`, else the platform value
 * an operator set through `PUT /admin/gateway/budget-alerts`, else 80 — a
 * three-step COALESCE this client cannot see. Comparing `percent_used` to
 * `warning_pct` here would also have to pick `>` or `>=` at the boundary and
 * would pick differently from the gateway's own soft-alert path.
 *
 * `percent_used` is null for an unlimited or disabled scope, and comparing null
 * to a number in JavaScript answers false by coercion rather than by decision —
 * the right answer for the wrong reason, which stops being right as soon as the
 * field changes shape.
 *
 * ## Polling
 *
 * The banner has to appear during a session, not only on a reload: a long chat
 * is exactly how a project crosses its threshold. `refetchInterval` is a minute
 * — the accumulator is written back per request, and a warning that is a minute
 * late is still a warning, while a per-turn read would put an extra request
 * behind every message.
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

/** One minute. See the header for why it is not shorter. */
const BUDGET_POLL_MS = 60_000;

/**
 * The fields this widget reads. The response carries more (the authored limit,
 * the remaining amount, the fail mode); none of them belongs in a banner, and
 * the money fields are REDACTED for a member who may not see them — which is
 * why the banner is built from the percentage and the threshold, the two the
 * server never removes.
 */
export interface ProjectBudgetWarning {
  readonly warning_active?: boolean;
  readonly warning_pct?: number;
  readonly percent_used?: number | null;
  readonly period_end?: string;
}

const projectBudgetKeys = {
  all: ['app-shell', 'project-budget'] as const,
  project: (projectId: string) => ['app-shell', 'project-budget', projectId] as const,
};

export function useProjectBudgetWarning(
  projectId: string | undefined,
): UseQueryResult<ProjectBudgetWarning, Error> {
  return useQuery({
    queryKey: projectBudgetKeys.project(projectId ?? ''),
    enabled: projectId !== undefined && projectId !== '',
    refetchInterval: BUDGET_POLL_MS,
    // A budget read that fails must not retry the shell into a request loop:
    // the banner is an extra, and its absence is not worth the traffic.
    retry: false,
    queryFn: async (): Promise<ProjectBudgetWarning> =>
      unwrapBody(
        await eliteaFetch<unknown>(`/elitea_core/project_budget/prompt_lib/${projectId ?? ''}/budget`),
      ) as ProjectBudgetWarning,
  });
}
