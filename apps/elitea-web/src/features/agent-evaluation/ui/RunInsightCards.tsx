/**
 * The three insight cards of a run (issue 2011, step A): consumption,
 * efficiency, and the comparison with the previous run.
 *
 * Every card is a pure function of the values it is handed. A figure that was
 * not recorded reads "not recorded"; it is never printed as 0.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AnalyticsEvaluationRun, AnalyticsUsageFigures } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { formatCost, formatCount, formatDelta, formatMs } from '../lib/insightsFormat';
import type { Efficiency, RunComparison } from '../lib/runInsights';
import type { TrajectorySummary } from '../lib/trajectory';

const cardSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
  padding: '0.75rem',
  border: (theme) => `1px solid ${theme.vars.palette.divider}`,
  borderRadius: (theme) => theme.vars.shape.radiusMd,
};
const stripSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '1.5rem' };
const rowSx: SxProps<Theme> = { display: 'flex', gap: '1.5rem', flexWrap: 'wrap' };

function Stat({ label, value, testId }: { readonly label: string; readonly value: string; readonly testId: string }): ReactNode {
  return (
    <Box data-testid={testId}>
      <Typography variant="bodySmall" color="text.secondary" component="div">
        {label}
      </Typography>
      <Typography variant="labelMedium" component="div">
        {value}
      </Typography>
    </Box>
  );
}

function RoleRow({ role, figures, testId }: { readonly role: string; readonly figures: AnalyticsUsageFigures | undefined; readonly testId: string }): ReactNode {
  return (
    <Box sx={rowSx} data-testid={testId}>
      <Typography variant="bodyMedium" sx={{ minWidth: '4rem' }}>
        {role}
      </Typography>
      <Typography variant="bodyMedium">
        {t('features.agentEvaluation.insights.tokens', '{{tokens}} tokens', { tokens: formatCount(figures?.total_tokens) })}
      </Typography>
      <Typography variant="bodyMedium">{formatCost(figures?.total_cost)}</Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t('features.agentEvaluation.insights.calls', '{{calls}} call(s)', { calls: formatCount(figures?.llm_calls) })}
      </Typography>
    </Box>
  );
}

export interface ConsumptionCardProps {
  /** `undefined` while loading; a state, not an error, for the gaps below. */
  readonly analytics: AnalyticsEvaluationRun | undefined;
  /** Why there is nothing to show: no permission, or not on this deployment. */
  readonly gap: 'forbidden' | 'unavailable' | undefined;
}

function gapText(analytics: AnalyticsEvaluationRun | undefined, gap: ConsumptionCardProps['gap']): string | undefined {
  if (gap === 'forbidden') {
    return t('features.agentEvaluation.insights.forbidden', 'Consumption figures need the analytics permission.');
  }
  if (gap === 'unavailable') {
    return t('features.agentEvaluation.insights.unavailable', 'This deployment does not record consumption.');
  }
  if (analytics?.available === false) {
    return analytics.unavailable_reason === 'log_pruned'
      ? t('features.agentEvaluation.insights.pruned', 'The gateway no longer holds this run’s calls.')
      : t('features.agentEvaluation.insights.beforeAttribution', 'This run started before consumption was recorded.');
  }
  return undefined;
}

export function ConsumptionCard({ analytics, gap }: ConsumptionCardProps): ReactNode {
  const reason = gapText(analytics, gap);
  return (
    <Box sx={cardSx} data-testid="run-consumption">
      <Typography variant="labelMedium">{t('features.agentEvaluation.insights.consumption', 'Consumption')}</Typography>
      {reason !== undefined && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="run-consumption-gap">
          {reason}
        </Typography>
      )}
      {reason === undefined && analytics?.available === true && (
        <>
          <RoleRow role={t('features.agentEvaluation.insights.agent', 'Agent')} figures={analytics.agent} testId="run-consumption-agent" />
          <RoleRow role={t('features.agentEvaluation.insights.judge', 'Judge')} figures={analytics.judge} testId="run-consumption-judge" />
          <RoleRow role={t('features.agentEvaluation.insights.total', 'Total')} figures={analytics.totals} testId="run-consumption-total" />
          <Typography variant="bodySmall" color="text.secondary">
            {t('features.agentEvaluation.insights.estimate', 'Money is an estimate at the catalogue rate.')}
          </Typography>
        </>
      )}
    </Box>
  );
}

export interface EfficiencyStripProps {
  readonly efficiency: Efficiency;
  /** Present for a run whose cases were agent runs: the tool metrics join the strip. */
  readonly trajectory?: TrajectorySummary | undefined;
}

/** Tool calls per measured case, one decimal; `undefined` when no case was measured. */
function toolCallsPerCase(trajectory: TrajectorySummary): number | undefined {
  const measured = trajectory.cases.length;
  return measured === 0 ? undefined : Math.round((trajectory.toolCalls / measured) * 10) / 10;
}

function ToolMetrics({ trajectory }: { readonly trajectory: TrajectorySummary }): ReactNode {
  return (
    <>
      <Stat
        label={t('features.agentEvaluation.insights.toolCallsPerCase', 'Tool calls per case')}
        value={formatCount(toolCallsPerCase(trajectory))}
        testId="run-efficiency-tool-calls"
      />
      <Stat label={t('features.agentEvaluation.insights.toolErrors', 'Tool errors')} value={formatCount(trajectory.toolErrors)} testId="run-efficiency-tool-errors" />
      <Stat label={t('features.agentEvaluation.insights.retries', 'Retries')} value={formatCount(trajectory.retries)} testId="run-efficiency-retries" />
    </>
  );
}

export function EfficiencyStrip({ efficiency, trajectory }: EfficiencyStripProps): ReactNode {
  const partial = efficiency.measuredCases < efficiency.totalCases;
  return (
    <Box sx={cardSx} data-testid="run-efficiency">
      <Typography variant="labelMedium">{t('features.agentEvaluation.insights.efficiency', 'Efficiency')}</Typography>
      <Box sx={stripSx}>
        <Stat label={t('features.agentEvaluation.insights.p50', 'Latency p50')} value={formatMs(efficiency.p50Ms)} testId="run-efficiency-p50" />
        <Stat label={t('features.agentEvaluation.insights.p95', 'Latency p95')} value={formatMs(efficiency.p95Ms)} testId="run-efficiency-p95" />
        <Stat
          label={t('features.agentEvaluation.insights.timeouts', 'Timeouts')}
          value={formatCount(efficiency.measuredCases === 0 ? undefined : efficiency.timeouts)}
          testId="run-efficiency-timeouts"
        />
        <Stat label={t('features.agentEvaluation.insights.tokensPerCase', 'Tokens per case')} value={formatCount(efficiency.tokensPerCase)} testId="run-efficiency-tokens" />
        <Stat label={t('features.agentEvaluation.insights.costPerCase', 'Cost per case')} value={formatCost(efficiency.costPerCase)} testId="run-efficiency-cost" />
        {trajectory !== undefined && <ToolMetrics trajectory={trajectory} />}
      </Box>
      {partial && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="run-efficiency-partial">
          {t('features.agentEvaluation.insights.partial', 'Latency was recorded for {{measured}} of {{total}} case(s).', {
            measured: efficiency.measuredCases,
            total: efficiency.totalCases,
          })}
        </Typography>
      )}
    </Box>
  );
}

export interface ComparisonCardProps {
  readonly comparison: RunComparison | undefined;
  /** True when no earlier scored run of this dataset exists. */
  readonly noPrevious: boolean;
  /** Dimension names by id, for the rows. */
  readonly dimensionNames: ReadonlyMap<string, string>;
}

export function ComparisonCard({ comparison, noPrevious, dimensionNames }: ComparisonCardProps): ReactNode {
  return (
    <Box sx={cardSx} data-testid="run-comparison">
      <Typography variant="labelMedium">{t('features.agentEvaluation.insights.comparison', 'Compared with the previous run')}</Typography>
      {noPrevious && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="run-comparison-none">
          {t('features.agentEvaluation.insights.noPrevious', 'No earlier scored run on this dataset.')}
        </Typography>
      )}
      {comparison !== undefined && (
        <>
          <Typography variant="bodySmall" color="text.secondary">
            {t('features.agentEvaluation.insights.against', 'Against run {{id}}', { id: comparison.previousRunId })}
          </Typography>
          <Box sx={stripSx}>
            <Stat label={t('features.agentEvaluation.insights.headlineDelta', 'Overall score')} value={formatDelta(comparison.headline, (n) => n.toFixed(1))} testId="run-comparison-headline" />
            <Stat label={t('features.agentEvaluation.insights.latencyDelta', 'Latency p50')} value={formatDelta(comparison.p50Ms, formatMs)} testId="run-comparison-latency" />
            <Stat label={t('features.agentEvaluation.insights.costDelta', 'Cost per case')} value={formatDelta(comparison.costPerCase, formatCost)} testId="run-comparison-cost" />
          </Box>
          {comparison.dimensions.map((entry) => (
            <Typography key={entry.dimensionId} variant="bodySmall" data-testid={`run-comparison-dimension-${entry.dimensionId}`}>
              {`${dimensionNames.get(entry.dimensionId) ?? entry.dimensionId}: ${formatDelta(entry.delta, (n) => n.toFixed(1))}`}
            </Typography>
          ))}
        </>
      )}
    </Box>
  );
}
