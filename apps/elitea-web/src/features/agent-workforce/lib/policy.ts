/**
 * Pure helpers for the workforce policy page (issue 1617): the editable
 * form a policy becomes, the input it is saved as, and labels.
 */
import type {
  AgentAutomaticAction,
  AgentWorkforceCondition,
  AgentWorkforcePolicy,
  AgentWorkforcePolicyInput,
  AgentWorkforceRule,
} from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { stageLabel } from './workforce';

/** The bounds the server checks (domain/agentworkforce/policy.go). */
const POLICY_BOUNDS = {
  minSuccessfulRuns: [1, 100],
  maxAutoHired: [1, 50],
  graceDays: [1, 90],
} as const;

/** What the page edits: the input shape, numbers held as typed. */
export interface PolicyForm {
  readonly autoHireEnabled: boolean;
  readonly minSuccessfulRuns: string;
  readonly maxAutoHired: string;
  readonly autoDismissEnabled: boolean;
  readonly graceDays: string;
  readonly rules: readonly AgentWorkforceRule[];
}

export function formOf(policy: AgentWorkforcePolicy): PolicyForm {
  return {
    autoHireEnabled: policy.auto_hire_enabled,
    minSuccessfulRuns: String(policy.min_successful_runs),
    maxAutoHired: String(policy.max_auto_hired),
    autoDismissEnabled: policy.auto_dismiss_enabled,
    graceDays: String(policy.grace_days),
    rules: policy.auto_dismiss_rules,
  };
}

function bounded(value: string, [min, max]: readonly [number, number]): number | undefined {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed >= min && parsed <= max ? parsed : undefined;
}

/** The input to save, or undefined while a number is out of bounds. */
export function inputOf(form: PolicyForm): AgentWorkforcePolicyInput | undefined {
  const minSuccessfulRuns = bounded(form.minSuccessfulRuns, POLICY_BOUNDS.minSuccessfulRuns);
  const maxAutoHired = bounded(form.maxAutoHired, POLICY_BOUNDS.maxAutoHired);
  const graceDays = bounded(form.graceDays, POLICY_BOUNDS.graceDays);
  if (minSuccessfulRuns === undefined || maxAutoHired === undefined || graceDays === undefined) return undefined;
  if (form.rules.some((rule) => rule.conditions.some((condition) => !Number.isFinite(condition.value) || condition.value < 0))) return undefined;
  return {
    auto_hire_enabled: form.autoHireEnabled,
    min_successful_runs: minSuccessfulRuns,
    max_auto_hired: maxAutoHired,
    auto_dismiss_enabled: form.autoDismissEnabled,
    auto_dismiss_rules: form.rules.map((rule) => ({ id: rule.id, stage: rule.stage, conditions: rule.conditions.map((c) => ({ ...c })) })),
    grace_days: graceDays,
  };
}

/** A copy of the rules with one condition's threshold changed. */
export function withThreshold(rules: readonly AgentWorkforceRule[], ruleIndex: number, conditionIndex: number, value: number): AgentWorkforceRule[] {
  return rules.map((rule, r) =>
    r !== ruleIndex
      ? rule
      : { ...rule, conditions: rule.conditions.map((condition, c) => (c === conditionIndex ? { ...condition, value } : condition)) },
  );
}

/** Each scorecard metric a condition reads, in words. */
const METRIC_LABELS: Readonly<Record<string, () => string>> = {
  runs_30d: () => t('features.agentWorkforce.metric.runs', 'runs in 30 days'),
  failed_30d: () => t('features.agentWorkforce.metric.failed', 'failed runs in 30 days'),
  cap_reached_30d: () => t('features.agentWorkforce.metric.capReached', 'budget-cap stops in 30 days'),
  failure_rate_30d: () => t('features.agentWorkforce.metric.failureRate', 'failure rate (0 to 1)'),
  breaker_trips_7d: () => t('features.agentWorkforce.metric.breakerTrips', 'breaker trips in 7 days'),
  approval_rejections_30d: () => t('features.agentWorkforce.metric.rejections', 'approval rejections in 30 days'),
  guardrail_hits_30d: () => t('features.agentWorkforce.metric.guardrails', 'guardrail hits in 30 days'),
  spend_ratio: () => t('features.agentWorkforce.metric.spendRatio', 'spend against the project median'),
  thumbs_down_30d: () => t('features.agentWorkforce.metric.thumbsDown', 'negative feedback in 30 days'),
  unused_days: () => t('features.agentWorkforce.metric.unused', 'days unused'),
  auto_hired: () => t('features.agentWorkforce.metric.autoHired', 'hired automatically (1 or 0)'),
  probation_failed: () => t('features.agentWorkforce.metric.probationFailed', 'failed its probation (1 or 0)'),
};

function metricLabel(metric: string): string {
  const label = METRIC_LABELS[metric];
  return label === undefined ? metric : label();
}

export function conditionText(condition: AgentWorkforceCondition): string {
  return `${metricLabel(condition.metric)} ${condition.op}`;
}

export function actionKindLabel(action: AgentAutomaticAction): string {
  switch (action.kind) {
    case 'auto_hire':
      return t('features.agentWorkforce.automatic.kind.hire', 'Hired automatically');
    case 'orphan':
      return t('features.agentWorkforce.automatic.kind.orphan', 'Owner left the project');
    default:
      return t('features.agentWorkforce.automatic.kind.dismiss', '{{stage}} by rule {{rule}}', {
        stage: stageLabel(action.stage ?? ''),
        rule: action.rule,
      });
  }
}

export function actionStatusLabel(status: string): string {
  switch (status) {
    case 'pending':
      return t('features.agentWorkforce.automatic.status.pending', 'Waiting for the grace period');
    case 'flagged':
      return t('features.agentWorkforce.automatic.status.flagged', 'Needs a new owner');
    case 'applied':
      return t('features.agentWorkforce.automatic.status.applied', 'Applied');
    case 'held':
      return t('features.agentWorkforce.automatic.status.held', 'Held for a person');
    case 'objected':
      return t('features.agentWorkforce.automatic.status.objected', 'Objected');
    case 'undone':
      return t('features.agentWorkforce.automatic.status.undone', 'Undone');
    case 'kept':
      return t('features.agentWorkforce.automatic.status.kept', 'Kept');
    case 'resolved':
      return t('features.agentWorkforce.automatic.status.resolved', 'Resolved');
    case 'cancelled':
      return t('features.agentWorkforce.automatic.status.cancelled', 'Cancelled');
    default:
      return status;
  }
}
