import type {
  AnalyticsEstimateModel,
  AnalyticsEstimateUser,
  AnalyticsUsageEstimate,
} from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import type { EstimateRow } from '../ui/components/EstimateTables';

/**
 * Row mappers shared by the Costs and Tokens tabs.
 *
 * Both tabs read ONE payload — `/analytics_costs`'s `estimate` block — and draw
 * two projections of it. Keeping the mapping here is what stops the two tabs
 * from disagreeing about which model a row is, or about whether a row without a
 * price has a cost.
 */

/** A model row. The key carries the provider; the label does not, as the reference screen shows the model alone. */
export function modelRow(model: AnalyticsEstimateModel): EstimateRow {
  return {
    key: `${model.provider}/${model.model}`,
    label: model.model,
    promptTokens: model.prompt_tokens,
    completionTokens: model.completion_tokens,
    totalTokens: model.total_tokens,
    inputCost: model.input_cost,
    outputCost: model.output_cost,
    totalCost: model.total_cost,
  };
}

/**
 * A caller row.
 *
 * The label falls back to the numeric id when the identity corpus is not on the
 * database. That is the honest label for a caller the platform can count and
 * cannot name — a blank cell would read as a row with no owner.
 */
export function userRow(user: AnalyticsEstimateUser): EstimateRow {
  const label =
    user.email !== '' ? user.email : t('analytics.estimate.unnamedUser', 'User {{id}}', { id: user.user_id });
  return {
    key: String(user.user_id),
    label,
    promptTokens: user.prompt_tokens,
    completionTokens: user.completion_tokens,
    totalTokens: user.total_tokens,
    inputCost: user.input_cost,
    outputCost: user.output_cost,
    totalCost: user.total_cost,
  };
}

/**
 * The denominator for a SHARE column.
 *
 * The window total, never the sum of the rows on screen. They differ whenever
 * the server capped the list, and normalising by the visible rows would turn
 * "the top 100 models" into "every model" with every percentage inflated.
 */
export function tokenShareTotal(estimate: AnalyticsUsageEstimate): number {
  return estimate.totals.total_tokens;
}

export function costShareTotal(estimate: AnalyticsUsageEstimate): number {
  return estimate.totals.total_cost ?? 0;
}
