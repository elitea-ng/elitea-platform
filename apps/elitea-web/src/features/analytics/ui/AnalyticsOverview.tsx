import { memo, useMemo } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import { useTheme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import { Area, AreaChart, ResponsiveContainer, Tooltip as RechartsTooltip, XAxis, YAxis } from 'recharts';

import type { ProjectAnalytics, UserActivity } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { pickChartColor, pickMedalColor } from '../lib/constants';
import { fmtNum } from '../lib/format';
import { AnalyticsKpiRow } from './components/AnalyticsKpiRow';
import { ChartTooltip } from './components/ChartTooltip';
import { ModelUsageTable } from './components/ModelUsageTable';

/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/analytics/ui/AnalyticsOverview.jsx`.
 *
 * ── `top_ai_users`/`daily_activity` ARE REAL DATA NOW ──
 *
 * They were `zod.looseObject({})` arrays read through `numField`/`strField`,
 * because the Go handler hardcoded both to `[]` and the spec could not say what
 * an element would look like. Read defensively, they degraded silently: every
 * field the backend did not send became `0` or `''`, so a leaderboard populated
 * with the wrong key names would have rendered a list of blank names scoring
 * zero rather than failing.
 *
 * Both are typed and populated as of the gateway request log (shared migration
 * 0099). The loose readers are gone from this file: the chart's series are
 * `llm_calls`/`active_users` and the leaderboard's rows are `UserActivity`,
 * which the compiler now checks. The old field names it was guessing at
 * (`events`, `users`, `user_email`, `ai_events`, `tool_runs`, `agent_runs`)
 * never existed on any response.
 */
export interface AnalyticsOverviewProps {
  readonly data: ProjectAnalytics;
  readonly onUserClick?: (userId: string) => void;
  /**
   * Project spend for the same window, already formatted. From
   * `/analytics_costs` — the one owner of the money figure — rather than from
   * `data`, which no longer publishes a cost at all.
   */
  readonly totalCost?: string | undefined;
  /** The billing period(s) `totalCost` covers; see `AnalyticsKpiRow`. */
  readonly totalCostPeriod?: string | undefined;
}

const kpiRowWrapSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: (theme: Theme) => theme.spacing(2) };

const chartsGridSx: SxProps<Theme> = {
  display: 'grid',
  gridTemplateColumns: { xs: '1fr', md: '1fr 1fr' },
  gap: (theme: Theme) => theme.spacing(2),
  alignItems: 'stretch',
};

const cardSx = (theme: Theme) => ({
  padding: theme.spacing(2),
  borderRadius: theme.vars.shape.radiusMd,
  backgroundColor: theme.vars.palette.background.userInputBackground,
  display: 'flex',
  flexDirection: 'column',
  minWidth: 0,
});

const titleSx = (theme: Theme) => ({
  color: theme.vars.palette.text.secondary,
  marginBottom: theme.spacing(0.5),
  display: 'block',
});

const subtitleSx = (theme: Theme) => ({
  color: theme.vars.palette.text.metrics,
  fontSize: theme.typography.labelSmall.fontSize,
  marginBottom: theme.spacing(1),
  display: 'block',
});

const emptyTextSx = (theme: Theme) => ({ color: theme.vars.palette.text.metrics });

const leaderboardRowSx = (theme: Theme, clickable: boolean) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1.5),
  padding: theme.spacing(1),
  borderBottom: `1px solid ${theme.vars.palette.border.table}`,
  '&:last-child': { borderBottom: 'none' },
  ...(clickable
    ? { cursor: 'pointer', '&:hover': { backgroundColor: theme.vars.palette.background.conversation.hover } }
    : {}),
});

const rankSx = (theme: Theme) => ({
  width: '1.25rem',
  textAlign: 'center' as const,
  fontSize: theme.typography.bodyMedium.fontSize,
  fontWeight: 700,
  color: theme.vars.palette.text.metrics,
});

const initialSx = (theme: Theme) => ({
  fontSize: theme.typography.labelSmall.fontSize,
  fontWeight: 700,
  color: theme.vars.palette.text.button.primary,
  lineHeight: 1,
});

const emailSx = (theme: Theme, clickable: boolean) => ({
  color: theme.vars.palette.text.secondary,
  fontWeight: 500,
  display: 'block',
  ...(clickable ? { '&:hover': { textDecoration: 'underline' } } : {}),
});

const statsSx = (theme: Theme) => ({
  color: theme.vars.palette.text.metrics,
  fontSize: theme.typography.labelSmall.fontSize,
  display: 'block',
  marginTop: theme.spacing(0.125),
});

const scoreSx = (theme: Theme) => ({
  color: theme.vars.palette.text.secondary,
  fontWeight: 700,
  fontSize: theme.typography.bodyMedium.fontSize,
  flexShrink: 0,
});

/**
 * What to show for a member. `email` is empty when the identity tables are
 * absent — the server still reports the row, because "user 41 made 900 calls"
 * is useful without a display name and dropping it would silently shrink the
 * leaderboard. The id is the fallback, never a blank.
 */
function displayName(user: UserActivity): string {
  // `||`, not `??`: the server sends an EMPTY STRING for an unresolvable
  // email, not null, so `??` would pick the empty string and render a blank
  // row where the id belongs.
  return (user.name ?? '') || user.email || `#${user.user_id}`;
}

function AnalyticsOverviewImpl({ data, onUserClick, totalCost, totalCostPeriod }: AnalyticsOverviewProps): ReactNode {
  const theme = useTheme();
  const axisStroke = theme.vars.palette.text.primary;
  const axisTickStyle = { fill: axisStroke, fontSize: theme.typography.labelSmall.fontSize };

  const dailyActivity = data.daily_activity;
  const topAiUsers = data.top_ai_users;

  // The share denominator, and the reason it is not always this sum.
  //
  // `models` is CAPPED server-side. Summing a cut array normalises every share
  // against the busiest N rather than against the project, so the column adds to
  // 100% over a subset while the LLM CALLS tile beside it reports the real
  // total — two numbers on one screen that disagree with nothing explaining why.
  //
  // When the server says it cut the list, the denominator becomes the KPI's own
  // request count instead. That figure counts requests this table cannot show —
  // ones that never resolved a model — so the shares no longer sum to 100%, and
  // that is the honest shape: the remainder is traffic the table is not
  // describing.
  const totalModelCalls = useMemo(() => {
    if (data.models_truncated) return data.kpis.llm_calls ?? 0;
    return data.models.reduce((sum, model) => sum + model.run_count, 0);
  }, [data.models, data.models_truncated, data.kpis.llm_calls]);

  return (
    <Box sx={kpiRowWrapSx}>
      <AnalyticsKpiRow
        kpis={data.kpis}
        totalCost={totalCost}
        totalCostPeriod={totalCostPeriod}
      />
      <Box sx={chartsGridSx}>
        <Box sx={cardSx}>
          <Typography
            variant="labelMedium"
            sx={titleSx}
          >
            {t('analytics.overview.dailyActivity.title', 'Daily Activity')}
          </Typography>
          <Box sx={{ width: '100%', overflow: 'hidden', flex: 1, minHeight: 200 }}>
            <ResponsiveContainer
              width="100%"
              height="100%"
            >
              <AreaChart data={dailyActivity}>
                <XAxis
                  dataKey="date"
                  tick={axisTickStyle}
                  tickFormatter={(value: string) => value.slice(5)}
                  axisLine={{ stroke: axisStroke }}
                  tickLine={{ stroke: axisStroke }}
                />
                <YAxis
                  yAxisId="events"
                  tick={axisTickStyle}
                  axisLine={{ stroke: axisStroke }}
                  tickLine={{ stroke: axisStroke }}
                />
                <YAxis
                  yAxisId="users"
                  orientation="right"
                  tick={axisTickStyle}
                  axisLine={{ stroke: axisStroke }}
                  tickLine={{ stroke: axisStroke }}
                />
                <RechartsTooltip content={<ChartTooltip />} />
                <Area
                  yAxisId="events"
                  type="monotone"
                  dataKey="llm_calls"
                  name={t('analytics.overview.dailyActivity.seriesEvents', 'LLM calls')}
                  stroke={theme.vars.palette.status.draft}
                  fill={theme.vars.palette.status.draft}
                  fillOpacity={0.15}
                  strokeWidth={2}
                />
                <Area
                  yAxisId="users"
                  type="monotone"
                  dataKey="active_users"
                  name={t('analytics.overview.dailyActivity.seriesUsers', 'Active users')}
                  stroke={theme.vars.palette.status.published}
                  fill={theme.vars.palette.status.published}
                  fillOpacity={0.1}
                  strokeWidth={2}
                />
              </AreaChart>
            </ResponsiveContainer>
          </Box>
        </Box>
        <Box sx={cardSx}>
          <Typography
            variant="labelMedium"
            sx={titleSx}
          >
            {t('analytics.overview.leaderboard.title', 'Top AI Adopters')}
          </Typography>
          <Typography
            variant="bodySmall"
            sx={subtitleSx}
          >
            {t('analytics.overview.leaderboard.subtitle', 'Leaderboard by LLM calls')}
          </Typography>
          {topAiUsers.length > 0 ? (
            <Box sx={{ display: 'flex', flexDirection: 'column', width: '100%', overflow: 'auto' }}>
              {topAiUsers.map((user, index) => {
                const clickable = onUserClick !== undefined;
                return (
                  <Box
                    key={`${user.user_id}-${index}`}
                    sx={leaderboardRowSx(theme, clickable)}
                    onClick={clickable ? () => onUserClick(user.user_id) : undefined}
                  >
                    <Typography sx={rankSx}>{index + 1}</Typography>
                    <Box
                      sx={{
                        width: 32,
                        height: 32,
                        borderRadius: theme.vars.shape.radiusPill,
                        display: 'flex',
                        alignItems: 'center',
                        justifyContent: 'center',
                        flexShrink: 0,
                        backgroundColor: index < 3 ? pickMedalColor(index) : pickChartColor(index - 3),
                      }}
                    >
                      <Typography sx={initialSx}>{(displayName(user) || '?')[0]?.toUpperCase()}</Typography>
                    </Box>
                    <Box sx={{ flex: 1, minWidth: 0 }}>
                      <Typography
                        variant="bodySmall"
                        noWrap
                        sx={emailSx(theme, clickable)}
                      >
                        {displayName(user)}
                      </Typography>
                      <Typography
                        variant="bodySmall"
                        sx={statsSx}
                      >
                        {t('analytics.overview.leaderboard.stats', '{{tokens}} tokens', {
                          tokens: fmtNum(user.total_tokens),
                        })}
                      </Typography>
                    </Box>
                    <Typography
                      variant="bodyMedium"
                      sx={scoreSx}
                    >
                      {fmtNum(user.run_count)}
                    </Typography>
                  </Box>
                );
              })}
            </Box>
          ) : (
            <Typography
              variant="bodySmall"
              sx={emptyTextSx}
            >
              {t('analytics.overview.leaderboard.empty', 'No AI activity data.')}
            </Typography>
          )}
        </Box>
      </Box>
      <ModelUsageTable
        models={data.models}
        totalCalls={totalModelCalls}
      />
    </Box>
  );
}

export const AnalyticsOverview = memo(AnalyticsOverviewImpl);
