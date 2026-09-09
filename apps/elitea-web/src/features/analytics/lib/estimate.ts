import type {
  AnalyticsEstimateAgent,
  AnalyticsEstimateModel,
  AnalyticsEstimateTool,
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
 * An agent row (issue #875).
 *
 * The label falls back to the numeric application id when the tenant chat
 * projection carries no display name — a real state (agentUsage's own
 * `nameSelect` falls back to `''` for a Go-bootstrapped database with no
 * pylon-owned `applications` table), and a blank cell would read as a row
 * with no owner.
 */
export function agentRow(agent: AnalyticsEstimateAgent): EstimateRow {
  const label =
    agent.name !== ''
      ? agent.name
      : t('analytics.estimate.unnamedAgent', 'Agent {{id}}', { id: agent.application_id });
  return {
    key: agent.application_id,
    label,
    promptTokens: agent.prompt_tokens,
    completionTokens: agent.completion_tokens,
    totalTokens: agent.total_tokens,
    inputCost: agent.input_cost,
    outputCost: agent.output_cost,
    totalCost: agent.total_cost,
  };
}

/**
 * A tool row (issue #875).
 *
 * The key combines toolkit id and tool name: two toolkits can expose a tool of
 * the same name, and a bare `tool_name` key would collapse two different rows
 * into one in React's reconciliation. The label carries the toolkit name
 * beside the tool name for the same reason ToolAnalytics reports both.
 */
export function toolRow(tool: AnalyticsEstimateTool): EstimateRow {
  const label = tool.toolkit_name !== '' ? `${tool.toolkit_name} / ${tool.tool_name}` : tool.tool_name;
  return {
    key: `${tool.toolkit_id}/${tool.tool_name}`,
    label,
    promptTokens: tool.prompt_tokens,
    completionTokens: tool.completion_tokens,
    totalTokens: tool.total_tokens,
    inputCost: tool.input_cost,
    outputCost: tool.output_cost,
    totalCost: tool.total_cost,
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
