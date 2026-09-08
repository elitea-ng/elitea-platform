import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ProjectAnalytics } from '@/shared/api/generated/model';

import { ANALYTICS_TAB } from '../../lib/constants';
import { AnalyticsAgents } from '../AnalyticsAgents';
import { AnalyticsCosts } from '../AnalyticsCosts';
import { AnalyticsGuide } from '../AnalyticsGuide';
import { AnalyticsHealth } from '../AnalyticsHealth';
import { AnalyticsOverview } from '../AnalyticsOverview';
import { AnalyticsTokens } from '../AnalyticsTokens';
import { AnalyticsTools } from '../AnalyticsTools';
import { AnalyticsUsers } from '../AnalyticsUsers';
import { AnalyticsLoadError } from './DetailStatus';

/**
 * The eight-tab body of `AnalyticsContainer`'s content area. Extracted purely
 * to bring `AnalyticsContainer` itself under the `eslint(complexity)`
 * budget (12) — the eight-plus branch points below (loading/error/six
 * tabs) were the bulk of that function's complexity of 19. The `switch` is
 * further split into its own function (`renderTabBody`) — the loading/
 * error guards plus a 6-case switch in one function still measured 13.
 */
export interface AnalyticsTabContentProps {
  readonly activeTab: number;
  readonly needsOverview: boolean;
  readonly isFetching: boolean;
  readonly isError: boolean;
  /**
   * The overview query's rejection, threaded through so the error branch can
   * tell an absent data source (501) from a failed query (500). `isError`
   * alone cannot: it is the same `true` for both.
   */
  readonly error?: unknown;
  readonly data: ProjectAnalytics | undefined;
  /** Formatted project spend for the window, from `/analytics_costs`. */
  readonly totalCost?: string | undefined;
  readonly projectId: string | undefined;
  readonly dateFrom: string;
  readonly dateTo: string;
  readonly pendingUserId: string | null;
  readonly onUserClick: (userId: string) => void;
  readonly onBackToSource: () => void;
}

const centeredSx: SxProps<Theme> = {
  display: 'flex',
  justifyContent: 'center',
  alignItems: 'center',
  padding: (theme: Theme) => theme.spacing(8),
  position: 'absolute',
  top: '50%',
  left: '50%',
  transform: 'translate(-50%, -50%)',
};

type TabBodyProps = Omit<AnalyticsTabContentProps, 'needsOverview' | 'isFetching' | 'isError' | 'error'>;

function renderTabBody({
  activeTab,
  data,
  totalCost,
  projectId,
  dateFrom,
  dateTo,
  pendingUserId,
  onUserClick,
  onBackToSource,
}: TabBodyProps): ReactNode {
  switch (activeTab) {
    case ANALYTICS_TAB.overview:
      return data === undefined ? null : (
        <AnalyticsOverview
          data={data}
          onUserClick={onUserClick}
          totalCost={totalCost}
        />
      );
    case ANALYTICS_TAB.costs:
      return (
        <AnalyticsCosts
          projectId={projectId}
          dateFrom={dateFrom}
          dateTo={dateTo}
        />
      );
    case ANALYTICS_TAB.tokens:
      return (
        <AnalyticsTokens
          projectId={projectId}
          dateFrom={dateFrom}
          dateTo={dateTo}
        />
      );
    case ANALYTICS_TAB.agents:
      return (
        <AnalyticsAgents
          projectId={projectId}
          dateFrom={dateFrom}
          dateTo={dateTo}
        />
      );
    case ANALYTICS_TAB.tools:
      return (
        <AnalyticsTools
          projectId={projectId}
          dateFrom={dateFrom}
          dateTo={dateTo}
        />
      );
    case ANALYTICS_TAB.users:
      return (
        <AnalyticsUsers
          projectId={projectId}
          dateFrom={dateFrom}
          dateTo={dateTo}
          initialUserId={pendingUserId}
          onBackToSource={onBackToSource}
        />
      );
    case ANALYTICS_TAB.health:
      // `data.health`, not `data.daily_activity`. This used to pass the daily
      // series and NOT the `health` prop the component branches on, so the tab
      // returned its empty state for the whole life of the component — and the
      // series it did pass carried neither of the two fields the chart read.
      return data === undefined ? null : <AnalyticsHealth health={data.health} />;
    case ANALYTICS_TAB.guide:
      return <AnalyticsGuide />;
    default:
      return null;
  }
}

export function AnalyticsTabContent(props: AnalyticsTabContentProps): ReactNode {
  const { needsOverview, isFetching, isError, error } = props;

  if (needsOverview && isFetching) {
    return (
      <Box sx={centeredSx}>
        <CircularProgress size={32} />
      </Box>
    );
  }

  // Only the OVERVIEW query's failure is visible here (`isError` comes from
  // `AnalyticsContainer`'s usage-summary query). The Agents/Tools/Users tabs
  // each own a separate query, so they render their own `AnalyticsLoadError`
  // — see each tab's `isError` guard.
  if (needsOverview && isError) {
    return <AnalyticsLoadError error={error} />;
  }

  return renderTabBody(props);
}
