import { memo, useMemo } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import { useTheme } from '@mui/material/styles';
import { Bar, BarChart, ResponsiveContainer, Tooltip as RechartsTooltip, XAxis, YAxis } from 'recharts';

import { t } from '@/shared/i18n';

import { useProjectCostsQuery } from '../api/useAnalytics';
import { modelRow, tokenShareTotal, userRow } from '../lib/estimate';
import { fmtNum } from '../lib/format';
import { ChartTooltip } from './components/ChartTooltip';
import { AnalyticsLoadError } from './components/DetailStatus';
import { EstimateCard, EstimateNotice, EstimateTiles, EstimateUnavailable } from './components/EstimateShell';
import type { EstimateTile } from './components/EstimateShell';
import { TokenTable } from './components/EstimateTables';

/**
 * The Tokens tab.
 *
 * ── WHERE THE FIGURES COME FROM ──
 *
 * `/analytics_costs`'s `estimate` block, over `gateway.llm_request_logs`. The
 * counts are RECORDED — one row per call, with the provider's own reported
 * prompt and completion totals — so this tab needs no price catalogue and
 * answers on any deployment that has the log.
 *
 * ── WHAT IT DOES NOT SHOW, AND WHY THAT IS NOT A ZERO ──
 *
 * The reference deployment has CACHE READ and CACHE WRITE tiles beside these
 * three. The request log records prompt and completion tokens and no cache
 * counts, and no other table carries them, so those two tiles are ABSENT here
 * rather than present and zero — a zero would claim the project read nothing
 * from cache, which is a different and unmeasured statement. The notice below
 * the tiles says so on screen rather than leaving the reader to notice two
 * missing cards.
 *
 * ── THE QUERY IS THE CONTAINER'S ──
 *
 * `useProjectCostsQuery` with the same key the container uses, so switching to
 * this tab reads the cache the Overview tile already filled rather than issuing
 * a second request for the same window.
 */
export interface AnalyticsTokensProps {
  readonly projectId: string | undefined;
  readonly dateFrom: string;
  readonly dateTo: string;
}

function AnalyticsTokensImpl({ projectId, dateFrom, dateTo }: AnalyticsTokensProps): ReactNode {
  const theme = useTheme();
  const { data, isFetching, isError, error } = useProjectCostsQuery(projectId, { dateFrom, dateTo }, true);
  const estimate = data?.estimate;

  const tiles: readonly EstimateTile[] = useMemo(() => {
    if (estimate === undefined) return [];
    return [
      {
        key: 'total',
        label: t('analytics.tokens.totalLabel', 'TOTAL TOKENS'),
        value: fmtNum(estimate.totals.total_tokens),
        caption: t('analytics.tokens.totalCaption', 'input + output tokens'),
      },
      {
        key: 'input',
        label: t('analytics.tokens.inputLabel', 'INPUT TOKENS'),
        value: fmtNum(estimate.totals.prompt_tokens),
        caption: t('analytics.tokens.inputCaption', 'prompt tokens'),
      },
      {
        key: 'output',
        label: t('analytics.tokens.outputLabel', 'OUTPUT TOKENS'),
        value: fmtNum(estimate.totals.completion_tokens),
        caption: t('analytics.tokens.outputCaption', 'completion tokens'),
      },
      {
        key: 'calls',
        label: t('analytics.tokens.callsLabel', 'CALLS'),
        value: fmtNum(estimate.totals.calls),
        caption: t('analytics.tokens.callsCaption', 'requests the gateway served'),
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
          'analytics.tokens.noSource',
          'This deployment carries no gateway request log, which is where token counts are recorded.',
        )}
      />
    );
  }

  const axisStroke = theme.vars.palette.text.primary;
  const axisTickStyle = { fill: axisStroke, fontSize: theme.typography.labelSmall.fontSize };
  const shareTotal = tokenShareTotal(estimate);

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: theme.spacing(2) }}>
      <EstimateTiles tiles={tiles} />
      {!estimate.cache_dimension_available && (
        <EstimateNotice>
          {t(
            'analytics.tokens.noCache',
            'Cache read and cache write tokens are not recorded on this deployment, so they are not shown.',
          )}
        </EstimateNotice>
      )}
      {estimate.daily.length > 0 && (
        <EstimateCard
          title={t('analytics.tokens.chartTitle', 'Daily Token Usage')}
          subtitle={t('analytics.tokens.chartSubtitle', 'Token usage per day')}
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
                  axisLine={{ stroke: axisStroke }}
                  tickLine={{ stroke: axisStroke }}
                />
                <RechartsTooltip content={<ChartTooltip />} />
                {/*
                  Stacked, because the two series are parts of one total: the
                  reader compares days by column height and reads the split
                  inside it. Two side-by-side bars would invite comparing input
                  against output, which is not a comparison that means anything.
                */}
                <Bar
                  stackId="tokens"
                  dataKey="prompt_tokens"
                  name={t('analytics.tokens.seriesInput', 'Input')}
                  fill={theme.vars.palette.status.draft}
                />
                <Bar
                  stackId="tokens"
                  dataKey="completion_tokens"
                  name={t('analytics.tokens.seriesOutput', 'Output')}
                  fill={theme.vars.palette.status.published}
                />
              </BarChart>
            </ResponsiveContainer>
          </Box>
        </EstimateCard>
      )}
      <TokenTable
        title={t('analytics.tokens.byUserTitle', 'Token Usage by User')}
        subtitle={
          estimate.by_user_truncated
            ? t('analytics.tokens.byUserTruncated', 'The busiest callers; the list was capped')
            : t('analytics.tokens.byUserSubtitle', 'Every caller in the window')
        }
        entityColumn={t('analytics.tokens.columnUser', 'User')}
        rows={userRows}
        shareTotal={shareTotal}
        emptyMessage={t('analytics.tokens.byUserEmpty', 'No call in this window resolved to a user.')}
      />
      <TokenTable
        title={t('analytics.tokens.byModelTitle', 'Token Usage by Model')}
        subtitle={
          estimate.by_model_truncated
            ? t('analytics.tokens.byModelTruncated', 'The busiest models; the list was capped')
            : t('analytics.tokens.byModelSubtitle', 'Every model in the window')
        }
        entityColumn={t('analytics.tokens.columnModel', 'Model')}
        rows={modelRows}
        shareTotal={shareTotal}
        emptyMessage={t('analytics.tokens.byModelEmpty', 'No model was called in this window.')}
      />
    </Box>
  );
}

export const AnalyticsTokens = memo(AnalyticsTokensImpl);
