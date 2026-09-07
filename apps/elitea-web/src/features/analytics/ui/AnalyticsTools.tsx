import { memo, useCallback, useMemo, useState } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import { useTheme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import { Bar, BarChart, ResponsiveContainer, Tooltip as RechartsTooltip, XAxis, YAxis } from 'recharts';

import type { ToolAnalytics } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { combineSx } from '@/shared/ui/lib/combineSx';

import { useAnalyticsToolsListQuery } from '../api/useAnalytics';
import { pickChartColor } from '../lib/constants';
import { fmtDuration, fmtNum, UNAVAILABLE_METRIC } from '../lib/format';
import { AnalyticsToolDetailed } from './AnalyticsToolDetailed';
import { ChartTooltip } from './components/ChartTooltip';
import { renderColoredBar } from './components/coloredBarShape';
import { AnalyticsDimensionUnavailable, AnalyticsLoadError } from './components/DetailStatus';
import { PaginatedEntityTable } from './components/PaginatedEntityTable';
import type { EntityTableColumn } from './components/PaginatedEntityTable';

/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/analytics/ui/AnalyticsTools.jsx`. See
 * `AnalyticsAgents.tsx`'s header for the general field-mapping rationale
 * (same shape of defect, different domain): `ToolAnalytics`
 * (`toolkit_id`, `tool_name`, `run_count`, `avg_duration_ms`, `error_rate`)
 * has no `users`/`errors`(count)/`total` — only `error_rate` survives as a
 * genuine substitute for the "Errors" column.
 */
export interface AnalyticsToolsProps {
  readonly projectId: string | undefined;
  readonly dateFrom: string;
  readonly dateTo: string;
}

interface SelectedTool {
  readonly toolkitId: string;
  readonly toolName: string;
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

function matchesSearch(row: ToolAnalytics, query: string): boolean {
  return row.tool_name.toLowerCase().includes(query.toLowerCase());
}

function AnalyticsToolsImpl({ projectId, dateFrom, dateTo }: AnalyticsToolsProps): ReactNode {
  const theme = useTheme();
  const [selectedTool, setSelectedTool] = useState<SelectedTool | null>(null);

  // `dateFrom`/`dateTo` ARE sent correctly here — `useAnalyticsToolsListQuery`
  // (`api/useAnalytics.ts`) forwards them as `date_from`/`date_to` query
  // params on every request. The date-range picker is nonetheless cosmetic
  // for this tab, same defect shape as `AnalyticsAgents.tsx` (see that
  // file's header comment above its own list-query call for the full
  // rationale): `internal/api/v2/analytics/handler.go`'s `Tools()` handler
  // parses `start_date`/`end_date` via `parseParams()` but never threads
  // that result into `repo.GetToolAnalytics(...)`, which takes no date
  // bounds at all — only `GetUsageSummary` (Overview/Health) actually
  // receives them. OUT OF SCOPE FOR THIS UNIT (a Go handler in a different
  // repo/monorepo than this frontend worktree): the fix is `Tools()`
  // passing `params.StartDate`/`params.EndDate` through to
  // `repo.GetToolAnalytics(...)`. No client-side compensating fix is
  // available either: `ToolAnalytics` rows carry no per-row timestamp (see
  // this file's header) — only pre-aggregated `run_count`/`avg_duration_ms`/
  // `error_rate` — so there is nothing on the already-fetched rows to filter
  // by (contrast `AnalyticsUsers.tsx`, whose rows do carry `last_active_at`).
  const { data, isFetching, isError, error } = useAnalyticsToolsListQuery(projectId, { dateFrom, dateTo });
  const items = useMemo(() => data?.items ?? [], [data]);

  const chartData = useMemo(
    () =>
      items.slice(0, 20).map((tool, index) => ({
        toolName: tool.tool_name,
        calls: tool.run_count,
        color: pickChartColor(index),
      })),
    [items],
  );

  const handleBack = useCallback(() => setSelectedTool(null), []);

  if (selectedTool !== null) {
    return (
      <AnalyticsToolDetailed
        projectId={projectId}
        toolkitId={selectedTool.toolkitId}
        toolName={selectedTool.toolName}
        dateFrom={dateFrom}
        dateTo={dateTo}
        onBack={handleBack}
      />
    );
  }

  // See `AnalyticsAgents.tsx`'s identical guard: without it a failed query
  // falls through to `items = []` and renders the card chrome, the column
  // headers and "0 tools" — a convincing empty table where the backend has
  // just reported it has no data source at all (issue #303). After the
  // drill-down branch, so a failing LIST query never yanks the user out of
  // a detail screen that owns its own query and Back button.
  if (isError) {
    return <AnalyticsLoadError error={error} />;
  }

  // The dimension can be unavailable on a 200 (issue 618). The tool record is
  // shared migration 0119, so a window that closed before it was applied has no
  // tool data — the server answers `tool_dimension_available: false` and NO
  // `items` key rather than an empty list, and `data?.items ?? []` above would
  // otherwise turn that into a convincing "0 tools" table for a period in which
  // tools ran constantly. That is the same claim the 501 refusal used to
  // prevent, so it gets the same screen.
  if (data !== undefined && !data.tool_dimension_available) {
    return (
      <AnalyticsDimensionUnavailable
        detail={t(
          'analytics.tools.notRecorded',
          'This deployment did not record tool calls for the selected period.',
        )}
      />
    );
  }

  const columns: readonly EntityTableColumn[] = [
    {
      header: t('analytics.tools.columnTool', 'Tool'),
      flex: 3,
      render: (row) => (
        <Typography
          noWrap
          sx={cellSx}
        >
          {String(row['tool_name'])}
        </Typography>
      ),
    },
    {
      header: t('analytics.tools.columnCalls', 'Calls'),
      flex: 1,
      render: (row) => <Typography sx={cellSx}>{fmtNum(row['run_count'] as number)}</Typography>,
    },
    {
      header: t('analytics.tools.columnUsers', 'Users'),
      flex: 1,
      render: () => <Typography sx={cellSx}>{UNAVAILABLE_METRIC}</Typography>,
    },
    {
      header: t('analytics.tools.columnAvgLatency', 'Avg Latency'),
      flex: 1,
      render: (row) => <Typography sx={cellSx}>{fmtDuration(row['avg_duration_ms'] as number)}</Typography>,
    },
    {
      header: t('analytics.tools.columnErrors', 'Errors'),
      flex: 1,
      render: (row) => {
        const errorRate = row['error_rate'] as number;
        // `error_rate` is a 0-1 fraction (`ToolAnalytics.error_rate`,
        // `internal/domain/analytics/types.go:31-37`), not a 0-100
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
            {t('analytics.tools.chartTitle', 'Most Popular Tools')}
          </Typography>
          <Typography
            variant="bodySmall"
            sx={subtitleSx}
          >
            {t('analytics.tools.chartSubtitle', 'Top {{count}} by usage', { count: chartData.length })}
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
                  dataKey="toolName"
                  tick={{ fill: theme.vars.palette.text.primary, fontSize: theme.typography.labelTiny.fontSize }}
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
                  dataKey="calls"
                  name={t('analytics.tools.chartSeriesCalls', 'Calls')}
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
          {t('analytics.tools.tableTitle', 'Tool Details')}
        </Typography>
        <Typography
          variant="bodySmall"
          sx={subtitleSx}
        >
          {t('analytics.tools.tableSubtitle', '{{count}} tools', { count: items.length })}
        </Typography>
        <PaginatedEntityTable
          rows={items}
          isFetching={isFetching}
          columns={columns}
          rowKey={(row, index) => `${String(row['toolkit_id'])}-${index}`}
          searchPlaceholder={t('analytics.tools.searchPlaceholder', 'Search by tool name')}
          searchFilter={(row, query) => matchesSearch(row as unknown as ToolAnalytics, query)}
          onRowClick={(row) =>
            setSelectedTool({ toolkitId: String(row['toolkit_id']), toolName: String(row['tool_name']) })
          }
        />
      </Box>
    </Box>
  );
}

export const AnalyticsTools = memo(AnalyticsToolsImpl);
