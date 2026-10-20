import { useQuery } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { queryKeys } from '@/api/queries/keys';

/**
 * Current-period usage against the budget (client contract 1.3,
 * getProjectUsage): the calendar month, the spend and the enforced limit,
 * and, once the per-request ledger holds rows, token and request totals.
 *
 * Two server rules shape the summary:
 * - a member who may not see costs gets the payload with the money fields
 *   REMOVED (`can_see_amounts: false`); percentages and the warning stay;
 * - the token and request totals are ABSENT, not zero, when the ledger has
 *   no row for the period (`usage_events_available: false`), so "no data" is
 *   never shown as "no calls";
 * - the same holds for the spend: `spend_available: false` means there is no
 *   accumulator row for the period at all, so the spend (sent as 0), the
 *   remainder and the percentage are unknown, not zero. The limit stays.
 *
 * Read online when Settings opens; nothing is stored on the device.
 */

export type UsageScope = 'project' | 'user';

export interface UsageSummary {
  scope: UsageScope;
  /** YYYY-MM-DD, the first and last day of the period (UTC). */
  periodStart: string;
  periodEnd: string;
  resetsAt: string;
  /** False: the server removed the money fields for this caller. */
  canSeeAmounts: boolean;
  /** No enforced limit (none set, or the scope is exempt). */
  unlimited: boolean;
  /** False: no accumulator row for the period, so spend, remaining and percentUsed are unknown (null). */
  spendAvailable: boolean;
  /** USD, when the caller may see amounts and the server has the period's row. */
  spend: number | null;
  limit: number | null;
  remaining: number | null;
  /** Null when unlimited or the limit is zero. */
  percentUsed: number | null;
  warning: boolean;
  /** The ledger has rows for the period: the totals below are real. */
  totals: { tokens: number; requests: number } | null;
}

const Money = z.number().finite().nonnegative().nullish();
const Count = z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER).optional();
const Report = z.looseObject({
  scope: z.enum(['project', 'user']),
  period_start: z.string().max(32),
  period_end: z.string().max(32),
  resets_at: z.string().max(64),
  limit_source: z.string().max(32),
  can_see_amounts: z.boolean().optional(),
  spend: Money,
  effective_limit: Money,
  remaining: Money,
  percent_used: z.number().finite().nonnegative().nullish(),
  warning_active: z.boolean(),
  usage_events_available: z.boolean(),
  // Required by the contract; read as available when absent so a looser body keeps its numbers.
  spend_available: z.boolean().optional(),
  total_tokens: Count,
  api_requests: Count,
});

export function usageOf(data: unknown): UsageSummary | null {
  const parsed = Report.safeParse(data);
  if (!parsed.success) return null;
  const r = parsed.data;
  const canSee = r.can_see_amounts !== false;
  const unlimited = r.limit_source === 'unlimited' || (canSee && r.effective_limit == null);
  const known = r.spend_available !== false;
  return {
    scope: r.scope,
    periodStart: r.period_start,
    periodEnd: r.period_end,
    resetsAt: r.resets_at,
    canSeeAmounts: canSee,
    unlimited,
    spendAvailable: known,
    spend: canSee && known ? (r.spend ?? 0) : null,
    limit: canSee && !unlimited ? (r.effective_limit ?? null) : null,
    remaining: canSee && known && !unlimited ? (r.remaining ?? null) : null,
    percentUsed: unlimited || !known ? null : (r.percent_used ?? null),
    warning: r.warning_active,
    totals:
      r.usage_events_available && r.total_tokens !== undefined && r.api_requests !== undefined
        ? { tokens: r.total_tokens, requests: r.api_requests }
        : null,
  };
}

/** The scope's usage, or null when the workspace does not answer it (an older server, no access, a bad body). */
export async function fetchUsage(projectId: number, scope: UsageScope, client: ApiClient = sessionApiClient()): Promise<UsageSummary | null> {
  const { data, response } = await client.GET('/elitea_core/usage/prompt_lib/{project_id}/usage', {
    params: { path: { project_id: String(projectId) }, query: { scope } },
  });
  if (!response.ok) return null;
  return usageOf(data);
}

export function useUsage(projectId: number | null, scope: UsageScope, enabled: boolean) {
  return useQuery({
    queryKey: [...queryKeys.usage(projectId ?? 0), scope],
    queryFn: () => fetchUsage(projectId!, scope),
    enabled: enabled && projectId !== null,
    staleTime: 60_000,
    gcTime: 0,
    retry: false,
  });
}

/** `$1.20` (the platform's budgets are USD only). */
export function formatUsd(amount: number): string {
  return `$${amount.toFixed(amount !== 0 && amount < 0.01 ? 4 : 2)}`;
}

/**
 * One line for a scope: "$1.20 of $10.00 (12%)", "45% of the budget", "$0.30, no limit", "No limit",
 * or "Limit $10.00, no usage data yet" when the server has no row for the period.
 */
export function usageLine(u: UsageSummary): string {
  if (!u.spendAvailable && !u.unlimited) {
    return u.canSeeAmounts && u.limit !== null ? `Limit ${formatUsd(u.limit)}, no usage data yet` : 'Budget set, no usage data yet';
  }
  const pct = u.percentUsed !== null ? `${Math.min(999, Math.round(u.percentUsed))}%` : null;
  if (u.canSeeAmounts && u.spend !== null) {
    if (u.unlimited) return u.spend > 0 ? `${formatUsd(u.spend)}, no limit` : 'No limit';
    if (u.limit !== null) return `${formatUsd(u.spend)} of ${formatUsd(u.limit)}${pct ? ` (${pct})` : ''}`;
  }
  if (u.unlimited) return 'No limit';
  return pct ? `${pct} of the budget` : 'Budget set';
}

export function totalsLine(u: UsageSummary): string | null {
  if (u.totals === null) return null;
  const n = (v: number) => v.toLocaleString('en-US');
  return `${n(u.totals.tokens)} tokens · ${n(u.totals.requests)} ${u.totals.requests === 1 ? 'request' : 'requests'}`;
}
