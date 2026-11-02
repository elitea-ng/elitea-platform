import type { ReactNode } from 'react';
import { useMemo } from 'react';

import type { PipelinesTabTotals } from './usePipelinesData';
import { Latest } from './Latest';
import { MyLiked } from './MyLiked';
import { PrivatePipelinesList } from './PrivatePipelinesList';
import { Trending } from './Trending';

export interface PipelineTab {
  readonly value: string;
  readonly label: string;
  readonly count: number | undefined;
  readonly content: ReactNode;
  /** `true` when the tab must not render (mirrors the baseline's `display: 'none'`, `Pipelines.jsx`'s public "Admin" tab). */
  readonly hidden?: boolean;
}

const PUBLIC_TAB_VALUES = ['latest', 'my-liked', 'trending', 'admin'] as const;
const PRIVATE_TAB_VALUES = ['all'] as const;

export type PublicPipelineTab = (typeof PUBLIC_TAB_VALUES)[number];
export type PrivatePipelineTab = (typeof PRIVATE_TAB_VALUES)[number];

/** Old app: `common/constants.js:483`, `ApplicationsTabs` (shared by both agents and pipelines pages). */
export function publicPipelineTabValues(): readonly PublicPipelineTab[] {
  return PUBLIC_TAB_VALUES;
}

/**
 * Old app: `common/constants.js:490`, `PrivateApplicationTabs` — but
 * `Pipelines.jsx`'s own private branch (`Pipelines.jsx:180-197`) only ever
 * renders a single `PrivatePipelinesList` tab labelled "All", unlike the
 * `agents` domain's six-status split (`pages/agents/useApplicationTabs.tsx`,
 * Wave-2 unit A1g). This narrower tuple matches that real baseline call
 * shape rather than the full shared constant, which the pipelines page never
 * actually iterates over.
 */
export function privatePipelineTabValues(): readonly PrivatePipelineTab[] {
  return PRIVATE_TAB_VALUES;
}

function usePublicPipelineTabs(totals: PipelinesTabTotals, hasAdminPermission: boolean): PipelineTab[] {
  return useMemo(
    () => [
      { value: 'latest', label: 'Latest', count: totals.latestTotal, content: <Latest /> },
      // Hidden until the public API can filter by liked status / trend: both
      // tabs would only say "not available yet". An old link lands on the first tab.
      { value: 'my-liked', label: 'My liked', count: totals.myLikedTotal, hidden: true, content: <MyLiked /> },
      { value: 'trending', label: 'Trending', count: totals.trendingTotal, hidden: true, content: <Trending /> },
      {
        value: 'admin',
        label: 'Admin',
        count: totals.applicationsTotal,
        hidden: !hasAdminPermission,
        content: <PrivatePipelinesList cardContentType="admin" />,
      },
    ],
    [totals.latestTotal, totals.myLikedTotal, totals.trendingTotal, totals.applicationsTotal, hasAdminPermission],
  );
}

function usePrivatePipelineTabs(totals: PipelinesTabTotals): PipelineTab[] {
  return useMemo(
    () => [
      {
        value: 'all',
        label: 'All',
        count: totals.applicationsTotal,
        content: <PrivatePipelinesList cardContentType="all" />,
      },
    ],
    [totals.applicationsTotal],
  );
}

/**
 * Ported from `apps/elitea-ui/src/pages/Pipelines/Pipelines.jsx`'s inline
 * `tabs` memo (`Pipelines.jsx:143-201`) — composes the four public
 * (`Latest`/`MyLiked`/`Trending`/admin `PrivatePipelinesList`) or one private
 * (`PrivatePipelinesList`, "All") tab definitions this same unit (A2m) owns.
 * Split into its own file for the same §3.5 line/complexity-budget reason
 * `pages/agents/useApplicationTabs.tsx` (Wave-2 unit A1g) documents — not a
 * distinct old-app file itself.
 *
 * **Disclosed drop:** the baseline's `sortBy`/`sortOrder` are no longer
 * threaded through as a memo dependency — `PrivatePipelinesList` (this unit)
 * reads them itself off `useSearch` directly. `trendRange`
 * (`DateRangeSelect`/`useTrendRange`) is dropped entirely: neither that
 * component nor its backing `trend_start_period` filter has a working
 * equivalent in this app — see `Trending.tsx`'s doc comment for the real
 * backend-contract reason.
 */
export function usePipelineTabs(
  isPublicProject: boolean,
  totals: PipelinesTabTotals,
  hasAdminPermission: boolean,
): PipelineTab[] {
  const publicTabs = usePublicPipelineTabs(totals, hasAdminPermission);
  const privateTabs = usePrivatePipelineTabs(totals);
  return isPublicProject ? publicTabs : privateTabs;
}
