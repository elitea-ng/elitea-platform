import type { ReactNode } from 'react';
import { useMemo } from 'react';

import type { ApplicationsTabTotals } from './useApplicationsData';
import { Latest } from './Latest';
import { MyLiked } from './MyLiked';
import { PrivateAgentsList } from './PrivateAgentsList';
import { Trending } from './Trending';

export interface ApplicationTab {
  readonly value: string;
  readonly label: string;
  readonly count: number | undefined;
  readonly content: ReactNode;
  /** `true` when the tab must not render (mirrors the baseline's `display: 'none'`, `Applications.jsx`'s public "Admin" tab). */
  readonly hidden?: boolean;
}

const PUBLIC_TAB_VALUES = ['latest', 'my-liked', 'trending', 'admin'] as const;

export type PublicApplicationTab = (typeof PUBLIC_TAB_VALUES)[number];

/** Old app: `common/constants.js:483`, `ApplicationsTabs`. */
export function publicApplicationTabValues(): readonly PublicApplicationTab[] {
  return PUBLIC_TAB_VALUES;
}

function usePublicApplicationTabs(totals: ApplicationsTabTotals, hasAdminPermission: boolean): ApplicationTab[] {
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
        content: (
          <PrivateAgentsList
            cardContentType="admin"
          />
        ),
      },
    ],
    [totals.latestTotal, totals.myLikedTotal, totals.trendingTotal, totals.applicationsTotal, hasAdminPermission],
  );
}

/**
 * The PUBLIC project's feed tabs (`Latest`, `My liked`, `Trending` and the
 * admin `PrivateAgentsList`), ported from
 * `apps/elitea-ui/src/pages/Applications/useApplicationTabs.jsx`.
 *
 * A private project has no tabs any more (ADR-0051 M1): its agents are one list
 * (`AgentsWorkshop`), where status is a badge and a filter in the right sidebar.
 * The six status tabs this file used to build — three of which no agent could
 * ever fill — are gone with it.
 *
 * `trendRange` (baseline: `DateRangeSelect`/`useTrendRange`) is not carried:
 * neither that component nor its backing `trend_start_period` filter has a
 * working equivalent here (see `Trending.tsx`).
 */
export function useApplicationTabs(totals: ApplicationsTabTotals, hasAdminPermission: boolean): ApplicationTab[] {
  return usePublicApplicationTabs(totals, hasAdminPermission);
}
