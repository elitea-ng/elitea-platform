/**
 * The platform's cost-budget switch, which gates Settings › Usage.
 *
 * ## What the flag means
 *
 * The reference gated the same tab on the same key: `platform_settings.py`'s
 * `_is_cost_budgets_enabled` asked the LiteLLM plugin for its budget mode and
 * returned true for "observe" and "enforce". This platform's equivalent is
 * whether the LLM gateway is composed at all — the gateway is what bills a
 * call, publishes the budget delta and fills
 * `gateway.llm_budget_accumulators`. With no gateway address every figure the
 * Usage tab renders is a structural zero, and a page of zeroes is not an empty
 * result: it is a claim that no calls were billed.
 *
 * ## Why this one defaults CLOSED
 *
 * Every other flag hook in this directory reads `!== false`, so an in-flight
 * query and an older backend both leave the feature VISIBLE. That is the safe
 * direction when the alternative is hiding a working feature.
 *
 * Here it is inverted, and deliberately. The failure this gate prevents is a
 * tab of invented zeroes shown as measurements, and defaulting open produces
 * exactly that on every deployment that has no gateway — including every
 * deployment older than the field. `=== true` is what makes the tab appear only
 * once the server has positively said there is cost data behind it.
 *
 * The server half of the gate is the endpoints themselves: the budget and usage
 * routes are permission-gated in `internal/api/router.go` independently of this
 * flag, so a client that ignores the key meets a 403 rather than an open
 * endpoint. This hook is only the half that decides what renders.
 */
import { useGetPlatformSettings } from '@/shared/api/generated/admin/admin';
import type { PlatformSettings } from '@/shared/api/generated/model';

export function useIsUsageVisible(): boolean {
  const query = useGetPlatformSettings();
  // `PlatformSettings` declares `additionalProperties: true` precisely so a
  // field can arrive on the wire without a spec edit, so this reads it through
  // an inline cast rather than widening the generated type.
  const settings = query.data?.data as
    | (PlatformSettings & { cost_budgets_enabled?: boolean })
    | undefined;
  return settings?.cost_budgets_enabled === true;
}
