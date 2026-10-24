/**
 * REST client for the admin LLM Proxy section's **Usage** tab —
 * `GET /api/v2/admin/gateway/usage`.
 *
 * ## What this restores
 *
 * LiteLLM's admin UI had a Usage page. ADR-0015 replaced LiteLLM with
 * `services/elitea-llm-gateway` and nothing replaced that page: migration 0084's
 * header records the gap in its own words — the budget accumulator holds money
 * per (scope, scope_id, period) and nothing else, so "the port of that page has a
 * meter and nothing else". `gateway.llm_usage_events`, the per-request ledger
 * 0084 added, is what this reads.
 *
 * ## A separate file from `./adminLlmProxyApi`, and a separate query namespace
 *
 * Usage is a REPORT over requests that have already been billed. A price
 * override changes what the NEXT call costs and never what a past one did, so
 * the catalogue's mutations deliberately do NOT invalidate this key: refetching
 * here after a save would redraw an unchanged screen and imply the edit had
 * reached the history.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes.
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

import type { UsageWindow } from './adminLlmProxyApi';

const USAGE_URL = '/admin/gateway/usage';

/**
 * Its own namespace under the section's. Declared here rather than at the call
 * site: a key built ad hoc is a cache nothing can ever invalidate deliberately —
 * the read/write namespace split that made saved data look absent in #132.
 */
const usageKeys = {
  report: (usageWindow: UsageWindow) => ['admin', 'llmProxy', 'usage', usageWindow] as const,
};

/**
 * The platform's totals for the window.
 *
 * `models` and `projects` are DISTINCT counts over the whole window, not the
 * lengths of the capped breakdowns below. They are separate fields for that
 * reason: "25 projects shown" against a true count of 300 is a different screen
 * from one where 25 is all there is.
 */
export interface LlmUsageTotals {
  readonly requests: number;
  readonly prompt_tokens: number;
  readonly completion_tokens: number;
  readonly total_tokens: number;
  readonly cost_usd: number;
  readonly models: number;
  readonly projects: number;
}
/** One UTC day of the series. Bucketed in UTC because billing periods are. */
export interface LlmUsageDay {
  readonly day: string;
  readonly requests: number;
  readonly total_tokens: number;
  readonly cost_usd: number;
}

/** One row of a breakdown — by model, by project or by member. */
export interface LlmUsageSlice {
  readonly key: string;
  readonly label: string;
  /** The provider for a model; the numeric id for a project or a member. */
  readonly detail: string;
  readonly requests: number;
  readonly prompt_tokens: number;
  readonly completion_tokens: number;
  readonly total_tokens: number;
  readonly cost_usd: number;
  /**
   * Model rows only: `tool` for a metered tool operation (#1238), whose label
   * is the operation and detail the vendor, with `units` of `unit` instead of
   * tokens; `llm` for a model call.
   */
  readonly operation?: 'llm' | 'tool';
  readonly units?: number;
  readonly unit?: string;
}

/**
 * `GET /admin/gateway/usage`'s body.
 *
 * Five error fields, not one, and that is the shape rather than an accident.
 * Each section is its own statement on the server, and a section that FAILED
 * renders exactly like a section with nothing in it — "no spend" being the
 * reassuring reading an operator would take from an empty table. `error` alone
 * refuses the whole report; the four others say which part did not run.
 */
export interface LlmUsageReport {
  readonly window: UsageWindow;
  readonly totals: LlmUsageTotals;
  readonly daily: readonly LlmUsageDay[];
  readonly models: readonly LlmUsageSlice[];
  readonly projects: readonly LlmUsageSlice[];
  readonly members: readonly LlmUsageSlice[];
  readonly models_truncated: boolean;
  readonly projects_truncated: boolean;
  readonly members_truncated: boolean;
  /** How many days of ledger survive the scheduler's prune. */
  readonly retention_days: number;
  readonly error?: string;
  readonly daily_error?: string;
  readonly models_error?: string;
  readonly projects_error?: string;
  readonly members_error?: string;
}

const EMPTY_USAGE_TOTALS: LlmUsageTotals = {
  requests: 0,
  prompt_tokens: 0,
  completion_tokens: 0,
  total_tokens: 0,
  cost_usd: 0,
  models: 0,
  projects: 0,
};

/**
 * The zero report — what an absent body resolves to.
 *
 * Every collection is present and empty rather than absent, so the panel renders
 * an explained empty state instead of branching on undefined.
 */
function emptyUsage(usageWindow: UsageWindow): LlmUsageReport {
  return {
    window: usageWindow,
    totals: EMPTY_USAGE_TOTALS,
    daily: [],
    models: [],
    projects: [],
    members: [],
    models_truncated: false,
    projects_truncated: false,
    members_truncated: false,
    retention_days: 0,
  };
}

/**
 * The five error fields, carried through when present and omitted when absent.
 *
 * Split out because the distinction is the whole contract: `error` refuses the
 * report, each of the other four says one section did not run, and a section
 * with no error and no rows is the genuine "nothing was spent" answer. Copying
 * them as `undefined` instead of omitting them would type-check and would make
 * `exactOptionalPropertyTypes` a lie about which of the three states a field is
 * in.
 */
function usageErrorsOf(body: LlmUsageReport): Partial<LlmUsageReport> {
  const keys = ['error', 'daily_error', 'models_error', 'projects_error', 'members_error'] as const;
  const errors: Record<string, string> = {};
  for (const key of keys) {
    const value = body[key];
    if (value !== undefined) errors[key] = value;
  }
  return errors;
}

/**
 * Normalises the totals FIELD BY FIELD, not as a whole object.
 *
 * `body.totals ?? zero` looks equivalent and is not: it defends against the
 * object being absent and not against it being present with a field missing,
 * and the panel formats every one of these with `toFixed`/`toLocaleString`,
 * which throw on undefined. The whole tab then unmounts — including the `error`
 * alert that would have said what went wrong, so the operator gets a blank pane
 * instead of the server's own sentence.
 *
 * The server always sends all seven, so this is a boundary guard rather than a
 * shape this deployment produces. It is here because the client is where a
 * partial body from an older or a proxied server arrives, and a report screen
 * that crashes on one is strictly worse than one that shows a zero.
 */
function totalsOf(raw: Partial<LlmUsageTotals> | undefined): LlmUsageTotals {
  // Written as a fold over the key list rather than seven `?? 0` expressions:
  // each of those is a branch, and the complexity gate is right that seven of
  // them in one function is a shape where an eighth field gets forgotten.
  const totals = { ...EMPTY_USAGE_TOTALS };
  if (raw === undefined) return totals;
  for (const key of Object.keys(EMPTY_USAGE_TOTALS) as (keyof LlmUsageTotals)[]) {
    const value = raw[key];
    if (typeof value === 'number') totals[key] = value;
  }
  return totals;
}

/** Fills in what a partial body leaves out. */
function normaliseUsage(
  body: LlmUsageReport | undefined,
  usageWindow: UsageWindow,
): LlmUsageReport {
  if (body === undefined) return emptyUsage(usageWindow);
  const zero = emptyUsage(body.window ?? usageWindow);
  return {
    ...zero,
    totals: totalsOf(body.totals),
    daily: body.daily ?? zero.daily,
    models: body.models ?? zero.models,
    projects: body.projects ?? zero.projects,
    members: body.members ?? zero.members,
    models_truncated: body.models_truncated ?? false,
    projects_truncated: body.projects_truncated ?? false,
    members_truncated: body.members_truncated ?? false,
    retention_days: body.retention_days ?? 0,
    ...usageErrorsOf(body),
  };
}

/** `GET /admin/gateway/usage?window=`. */
export function useAdminLlmUsage(usageWindow: UsageWindow): UseQueryResult<LlmUsageReport, Error> {
  return useQuery({
    queryKey: usageKeys.report(usageWindow),
    queryFn: async (): Promise<LlmUsageReport> => {
      const query = new URLSearchParams({ window: usageWindow });
      const body = unwrapBody(await eliteaFetch<unknown>(`${USAGE_URL}?${query.toString()}`)) as
        LlmUsageReport | undefined;
      return normaliseUsage(body, usageWindow);
    },
  });
}
