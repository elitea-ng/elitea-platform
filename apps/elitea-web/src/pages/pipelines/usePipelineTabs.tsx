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

export type PublicPipelineTab = (typeof PUBLIC_TAB_VALUES)[number];

/** Old app: `common/constants.js:483`, `ApplicationsTabs` (shared by both agents and pipelines pages). */
export function publicPipelineTabValues(): readonly PublicPipelineTab[] {
  return PUBLIC_TAB_VALUES;
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

/**
 * The PUBLIC project's feed tabs (`Latest`, `My liked`, `Trending` and the
 * admin `PrivatePipelinesList`), ported from the inline `tabs` memo of
 * `apps/elitea-ui/src/pages/Pipelines/Pipelines.jsx`.
 *
 * A private project has no tabs any more (ADR-0051 M1): its pipelines are one
 * list (`PipelinesWorkshop`) with every control in the right sidebar.
 */
export function usePipelineTabs(totals: PipelinesTabTotals, hasAdminPermission: boolean): PipelineTab[] {
  return usePublicPipelineTabs(totals, hasAdminPermission);
}
