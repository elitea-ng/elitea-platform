import { memo, useMemo } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import { useTheme } from '@mui/material/styles';
import { Bar, BarChart, ResponsiveContainer, Tooltip as RechartsTooltip, XAxis, YAxis } from 'recharts';

import { t } from '@/shared/i18n';

import { useProjectCostsQuery } from '../api/useAnalytics';
import { costShareTotal, modelRow, userRow } from '../lib/estimate';
import { fmtNum, fmtUsd } from '../lib/format';
import { ChartTooltip } from './components/ChartTooltip';
import { AnalyticsLoadError } from './components/DetailStatus';
import { EstimateCard, EstimateNotice, EstimateTiles, EstimateUnavailable } from './components/EstimateShell';
import type { EstimateTile } from './components/EstimateShell';
import { CostTable } from './components/EstimateTables';

/**
 * The Costs tab.
 *
 * ── AN ESTIMATE, AND IT SAYS SO ──
 *
 * Every figure here is tokens from `gateway.llm_request_logs` multiplied by the
 * price catalogue in `gateway.gateway_models`. It is NOT what the billing path
 * accounted: that figure is `kpis.total_cost`, from the budget accumulators,
 * and it is what the Overview COST tile shows. The two can differ, and the
 * notice at the top of this tab is what keeps a reader from treating this one
 * as an invoice.
 *
 * ── WHEN IT CANNOT ANSWER ──
 *
 * Two states, and they are different. No `estimate` block means this database
 * has no request log. `cost_dimension_available: false` means the log is there
 * and the price catalogue prices nothing that was called in the window — an
 * operator fixes that on the LLM Proxy screen, so the message points there
 * rather than at the deployment.
 *
 * A model the catalogue does not carry keeps its token counts in the tables and
 * shows "not priced" in its money cells. Pricing it at zero would make an
 * under-priced deployment look cheap, which is the failure this whole screen is
 * arranged to avoid.
 */
export interface AnalyticsCostsProps {
  readonly projectId: string | undefined;
  readonly dateFrom: string;
  readonly dateTo: string;
}

function AnalyticsCostsImpl({ projectId, dateFrom, dateTo }: AnalyticsCostsProps): ReactNode {
  const theme = useTheme();
  const { data, isFetching, isError, error } = useProjectCostsQuery(projectId, { dateFrom, dateTo }, true);
  const estimate = data?.estimate;

  const tiles: readonly EstimateTile[] = useMemo(() => {
    if (estimate === undefined || !estimate.cost_dimension_available) return [];
    const caption = t('analytics.costs.tileCaption', 'estimated USD cost');
    return [
      {
        key: 'total',
        label: t('analytics.costs.totalLabel', 'TOTAL COST'),
        value: fmtUsd(estimate.totals.total_cost),
        caption,
      },
      {
        key: 'input',
        label: t('analytics.costs.inputLabel', 'INPUT TOKEN COST'),
        value: fmtUsd(estimate.totals.input_cost),
        caption,
      },
      {
        key: 'output',
        label: t('analytics.costs.outputLabel', 'OUTPUT TOKEN COST'),
        value: fmtUsd(estimate.totals.output_cost),
        caption,
      },
      {
        key: 'priced',
        label: t('analytics.costs.pricedLabel', 'PRICED CALLS'),
        value: `${fmtNum(estimate.priced_calls)} / ${fmtNum(estimate.totals.calls)}`,
        caption: t('analytics.costs.pricedCaption', 'calls the catalogue prices'),
      },
    ];
  }, [estimate]);

  const modelRows = useMemo(() => (estimate?.by_model ?? []).map(modelRow), [estimate]);
  const userRows = useMemo(() => (estimate?.by_user ?? []).map(userRow), [estimate]);

  if (isFetching) {
    return (
      <Box sx={{ display: 'flex', justifyContent: 'center', padding: theme.spacing(8) }}>
        <CircularProgress size={32} />
      </Box>
    );
  }
  if (isError) {
    return <AnalyticsLoadError error={error} />;
  }
  if (estimate === undefined) {
    return (
      <EstimateUnavailable
        detail={t(
          'analytics.costs.noSource',
          'This deployment carries no gateway request log, which is what a cost estimate is computed from.',
        )}
      />
    );
  }
  if (!estimate.cost_dimension_available) {
    return (
      <EstimateUnavailable
        detail={t(
          'analytics.costs.noPrice',
          'No model called in this window has a price in the model catalogue. Set a price on the LLM Proxy screen to see estimated cost here.',
        )}
      />
    );
  }

  const axisStroke = theme.vars.palette.text.primary;
  const axisTickStyle = { fill: axisStroke, fontSize: theme.typography.labelSmall.fontSize };
  const shareTotal = costShareTotal(estimate);

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: theme.spacing(2) }}>
      <EstimateNotice>
        {t(
          'analytics.costs.estimateNotice',
          'Costs are estimated from the local model-price table. The accounted figure is on the Overview tab; actual provider invoices may differ.',
        )}
      </EstimateNotice>
      <EstimateTiles tiles={tiles} />
      {estimate.unpriced_calls > 0 && (
        <EstimateNotice>
          {t(
            'analytics.costs.unpricedNotice',
            // Phrased so the count is not the subject of a verb. The i18n shim
            // has no plural rule, so "{{count}} calls" prints "1 calls".
            'Calls that used a model the catalogue does not price: {{count}}. Their tokens are counted and their cost is not.',
            { count: estimate.unpriced_calls },
          )}
        </EstimateNotice>
      )}
      {estimate.daily.length > 0 && (
        <EstimateCard
          title={t('analytics.costs.chartTitle', 'Daily Cost Trend')}
          subtitle={t('analytics.costs.chartSubtitle', 'Estimated cost per day')}
        >
          <Box sx={{ width: '100%', overflow: 'hidden', flex: 1, minHeight: 200 }}>
            <ResponsiveContainer width="100%" height={240}>
              <BarChart data={[...estimate.daily]}>
                <XAxis
                  dataKey="date"
                  tick={axisTickStyle}
                  tickFormatter={(value: string) => value.slice(5)}
                  axisLine={{ stroke: axisStroke }}
                  tickLine={{ stroke: axisStroke }}
                />
                <YAxis
                  tick={axisTickStyle}
                  tickFormatter={(value: number) => fmtUsd(value)}
                  axisLine={{ stroke: axisStroke }}
                  tickLine={{ stroke: axisStroke }}
                  width={90}
                />
                <RechartsTooltip content={<ChartTooltip />} />
                <Bar
                  dataKey="total_cost"
                  name={t('analytics.costs.seriesTotal', 'Estimated cost')}
                  fill={theme.vars.palette.status.draft}
                />
              </BarChart>
            </ResponsiveContainer>
          </Box>
        </EstimateCard>
      )}
      <CostTable
        title={t('analytics.costs.byUserTitle', 'Cost by User')}
        subtitle={
          estimate.by_user_truncated
            ? t('analytics.costs.byUserTruncated', 'The busiest callers; the list was capped')
            : t('analytics.costs.byUserSubtitle', 'Every caller in the window')
        }
        entityColumn={t('analytics.costs.columnUser', 'User')}
        rows={userRows}
        shareTotal={shareTotal}
        emptyMessage={t('analytics.costs.byUserEmpty', 'No call in this window resolved to a user.')}
      />
      <CostTable
        title={t('analytics.costs.byModelTitle', 'Cost by Model')}
        subtitle={
          estimate.by_model_truncated
            ? t('analytics.costs.byModelTruncated', 'The busiest models; the list was capped')
            : t('analytics.costs.byModelSubtitle', 'Every model in the window')
        }
        entityColumn={t('analytics.costs.columnModel', 'Model')}
        rows={modelRows}
        shareTotal={shareTotal}
        emptyMessage={t('analytics.costs.byModelEmpty', 'No model was called in this window.')}
      />
    </Box>
  );
}

export const AnalyticsCosts = memo(AnalyticsCostsImpl);
