import { memo, useCallback, useMemo, useState } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import { useTheme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import { Bar, BarChart, ResponsiveContainer, Tooltip as RechartsTooltip, XAxis, YAxis } from 'recharts';

import type { AgentAnalytics } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { combineSx } from '@/shared/ui/lib/combineSx';

import { useAnalyticsAgentsListQuery } from '../api/useAnalytics';
import { pickChartColor } from '../lib/constants';
import { fmtDuration, fmtNum, UNAVAILABLE_METRIC } from '../lib/format';
import { AnalyticsAgentDetailed } from './AnalyticsAgentDetailed';
import { ChartTooltip } from './components/ChartTooltip';
import { renderColoredBar } from './components/coloredBarShape';
import { AnalyticsLoadError } from './components/DetailStatus';
import { PaginatedEntityTable } from './components/PaginatedEntityTable';
import type { EntityTableColumn } from './components/PaginatedEntityTable';

/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/analytics/ui/AnalyticsAgents.jsx`.
 *
 * Field mapping vs. the baseline (see `api/useAnalytics.ts`'s header and
 * this unit's final report): the real `AgentAnalytics` list row
 * (`application_id`, `name`, `run_count`, `avg_duration_ms`,
 * `total_tokens`, `error_rate`) has neither the baseline's `entity_id`/
 * `entity_name`/`events`/`users`/`errors` field names NOR a `total` count
 * or `chat_daily` series on the list envelope (`AnalyticsAgentsList` is
 * strictly `{items: AgentAnalytics[]}` — no other key exists on it in any
 * form, unlike `health`/`top_ai_users`, which are real if currently-empty
 * fields). The "Chat Messages" chart is therefore dropped, not ported dead:
 * there is no field on this response `chat_daily` could ever read.
 */
export interface AnalyticsAgentsProps {
  readonly projectId: string | undefined;
  readonly dateFrom: string;
  readonly dateTo: string;
}

interface SelectedAgent {
  readonly applicationId: string;
  readonly agentName: string;
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: (theme: Theme) => theme.spacing(2) };

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

const cellSx = (theme: Theme) => ({
  fontSize: theme.typography.bodyMedium.fontSize,
  color: theme.vars.palette.text.secondary,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
});

function matchesSearch(row: AgentAnalytics, query: string): boolean {
  return row.name.toLowerCase().includes(query.toLowerCase());
}

function AnalyticsAgentsImpl({ projectId, dateFrom, dateTo }: AnalyticsAgentsProps): ReactNode {
  const theme = useTheme();
  const [selectedAgent, setSelectedAgent] = useState<SelectedAgent | null>(null);

  // `dateFrom`/`dateTo` ARE sent correctly here — `useAnalyticsAgentsListQuery`
  // (`api/useAnalytics.ts`) forwards them as `date_from`/`date_to` query
  // params on every request. The date-range picker is nonetheless cosmetic
  // for this tab: `internal/api/v2/analytics/handler.go`'s `Agents()`
  // handler parses `start_date`/`end_date` via `parseParams()` (used to
  // build the response for `GetUsageSummary`, which powers the
  // Overview/Health tabs) but never threads that result into
  // `repo.GetAgentAnalytics(...)`, which takes no date bounds at all — so
  // this list is the same all-time aggregate regardless of what preset is
  // selected. OUT OF SCOPE FOR THIS UNIT (a Go handler in a different
  // repo/monorepo than this frontend worktree): the fix is
  // `Agents()` passing `params.StartDate`/`params.EndDate` through to
  // `repo.GetAgentAnalytics(...)`, mirroring how `GetUsageSummary` already
  // receives them. There is also no client-side compensating fix available
  // here the way there is for `AnalyticsUsers.tsx`'s `last_active_at`-based
  // filter: `AgentAnalytics` rows carry no per-row timestamp at all (see
  // this file's header) — only pre-aggregated `run_count`/`avg_duration_ms`/
  // `total_tokens`/`error_rate` — so there is nothing on the already-fetched
  // rows to filter by.
  const { data, isFetching, isError, error } = useAnalyticsAgentsListQuery(projectId, { dateFrom, dateTo });
  const items = useMemo(() => data?.items ?? [], [data]);

  const chartData = useMemo(
    () =>
      items.slice(0, 20).map((agent, index) => ({
        name: agent.name,
        events: agent.run_count,
        color: pickChartColor(index),
      })),
    [items],
  );

  const handleBack = useCallback(() => setSelectedAgent(null), []);

  if (selectedAgent !== null) {
    return (
      <AnalyticsAgentDetailed
        projectId={projectId}
        applicationId={selectedAgent.applicationId}
        agentName={selectedAgent.agentName}
        dateFrom={dateFrom}
        dateTo={dateTo}
        onBack={handleBack}
      />
    );
  }

  // A failed list query must not fall through to the table below: with
  // `data` undefined `items` is `[]`, which renders the card chrome, the
  // five column headers and "0 agents" — indistinguishable from a project
  // that genuinely has no agents. The routes now answer 500 ("analytics: no
  // data source", issue #303) instead of fabricating zeros, and this is the
  // same fabrication one layer up. Placed AFTER the drill-down branch above
  // so a failing LIST query never replaces a detail screen the user is
  // already inside (that screen owns its own query and its own Back button).
  if (isError) {
    return <AnalyticsLoadError error={error} />;
  }

  const columns: readonly EntityTableColumn[] = [
    {
      header: t('analytics.agents.columnAgent', 'Agent'),
      flex: 3,
      render: (row) => (
        <Typography
          noWrap
          sx={cellSx}
        >
          {String(row['name'])}
        </Typography>
      ),
    },
    {
      header: t('analytics.agents.columnEvents', 'Events'),
      flex: 1,
      render: (row) => <Typography sx={cellSx}>{fmtNum(row['run_count'] as number)}</Typography>,
    },
    {
      header: t('analytics.agents.columnUsers', 'Users'),
      flex: 1,
      render: () => <Typography sx={cellSx}>{UNAVAILABLE_METRIC}</Typography>,
    },
    {
      header: t('analytics.agents.columnAvgLatency', 'Avg Latency'),
      flex: 1,
      render: (row) => <Typography sx={cellSx}>{fmtDuration(row['avg_duration_ms'] as number)}</Typography>,
    },
    {
      header: t('analytics.agents.columnErrors', 'Errors'),
      flex: 1,
      render: (row) => {
        const errorRate = row['error_rate'] as number;
        // `error_rate` is a 0-1 fraction (`AgentAnalytics.error_rate`,
        // `internal/domain/analytics/types.go:22-29`), not a 0-100
        // percentage — must be scaled ×100 for display, matching this
        // feature's other fraction→percent readouts (e.g.
        // `ModelUsageTable.tsx`'s `share.toFixed(1)}%`). Previously
        // rendered the raw fraction with a bare `%` suffix, showing every
        // non-zero error rate 100x too small (a real 20% rate as "0.2%").
        const errorRatePercent = errorRate * 100;
        return (
          <Typography
            sx={combineSx(cellSx, { color: errorRate > 0 ? theme.vars.palette.status.rejected : undefined })}
          >
            {errorRatePercent.toFixed(1)}%
          </Typography>
        );
      },
    },
  ];

  return (
    <Box sx={contentSx}>
      {chartData.length > 0 && (
        <Box sx={cardSx}>
          <Typography
            variant="labelMedium"
            sx={titleSx}
          >
            {t('analytics.agents.chartTitle', 'Most Active Agents')}
          </Typography>
          <Typography
            variant="bodySmall"
            sx={subtitleSx}
          >
            {t('analytics.agents.chartSubtitle', 'Top {{count}} by events', { count: chartData.length })}
          </Typography>
          <Box sx={{ width: '100%', overflow: 'hidden', flex: 1, minHeight: 200 }}>
            <ResponsiveContainer
              width="100%"
              height={200}
            >
              <BarChart
                data={chartData}
                margin={{ left: 5, right: 20, top: 5, bottom: 40 }}
              >
                <XAxis
                  dataKey="name"
                  tick={{ fill: theme.vars.palette.text.primary, fontSize: theme.typography.labelSmall.fontSize }}
                  angle={-45}
                  textAnchor="end"
                  interval={0}
                  height={50}
                  axisLine={{ stroke: theme.vars.palette.text.primary }}
                  tickLine={{ stroke: theme.vars.palette.text.primary }}
                />
                <YAxis
                  tick={{ fill: theme.vars.palette.text.primary, fontSize: theme.typography.labelSmall.fontSize }}
                  axisLine={{ stroke: theme.vars.palette.text.primary }}
                  tickLine={{ stroke: theme.vars.palette.text.primary }}
                />
                <RechartsTooltip content={<ChartTooltip />} />
                <Bar
                  dataKey="events"
                  name={t('analytics.agents.chartSeriesEvents', 'Events')}
                  radius={[4, 4, 0, 0]}
                  shape={renderColoredBar}
                />
              </BarChart>
            </ResponsiveContainer>
          </Box>
        </Box>
      )}
      <Box sx={cardSx}>
        <Typography
          variant="labelMedium"
          sx={titleSx}
        >
          {t('analytics.agents.tableTitle', 'Agent Activity')}
        </Typography>
        <Typography
          variant="bodySmall"
          sx={subtitleSx}
        >
          {t('analytics.agents.tableSubtitle', '{{count}} agents', { count: items.length })}
        </Typography>
        <PaginatedEntityTable
          rows={items}
          isFetching={isFetching}
          columns={columns}
          rowKey={(row, index) => `${String(row['application_id'])}-${index}`}
          searchPlaceholder={t('analytics.agents.searchPlaceholder', 'Search by agent name')}
          searchFilter={(row, query) => matchesSearch(row as unknown as AgentAnalytics, query)}
          onRowClick={(row) =>
            setSelectedAgent({ applicationId: String(row['application_id']), agentName: String(row['name']) })
          }
        />
      </Box>
    </Box>
  );
}

export const AnalyticsAgents = memo(AnalyticsAgentsImpl);
