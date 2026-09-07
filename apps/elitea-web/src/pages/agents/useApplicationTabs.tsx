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
const PRIVATE_TAB_VALUES = ['all', 'drafts', 'published', 'moderation', 'approval', 'rejected'] as const;

export type PublicApplicationTab = (typeof PUBLIC_TAB_VALUES)[number];
export type PrivateApplicationTab = (typeof PRIVATE_TAB_VALUES)[number];

/** Old app: `common/constants.js:483`, `ApplicationsTabs`. */
export function publicApplicationTabValues(): readonly PublicApplicationTab[] {
  return PUBLIC_TAB_VALUES;
}

/** Old app: `common/constants.js:490`, `PrivateApplicationTabs`. */
export function privateApplicationTabValues(): readonly PrivateApplicationTab[] {
  return PRIVATE_TAB_VALUES;
}

function usePublicApplicationTabs(totals: ApplicationsTabTotals, hasAdminPermission: boolean): ApplicationTab[] {
  return useMemo(
    () => [
      { value: 'latest', label: 'Latest', count: totals.latestTotal, content: <Latest /> },
      { value: 'my-liked', label: 'My liked', count: totals.myLikedTotal, content: <MyLiked /> },
      { value: 'trending', label: 'Trending', count: totals.trendingTotal, content: <Trending /> },
      {
        value: 'admin',
        label: 'Admin',
        count: totals.applicationsTotal,
        hidden: !hasAdminPermission,
        content: (
          <PrivateAgentsList
            statuses={undefined}
            cardContentType="admin"
          />
        ),
      },
    ],
    [totals.latestTotal, totals.myLikedTotal, totals.trendingTotal, totals.applicationsTotal, hasAdminPermission],
  );
}

/**
 * The six private tabs, and what each of them can actually hold.
 *
 * `Drafts` and `Published` are REAL as of the lifecycle change: the list route
 * now reports each agent's publish state
 * (`internal/infra/db/repos/applications.go` `List`), so publishing a version
 * moves the agent from one tab to the other. Before that the field was never
 * selected and every one of these tabs was empty whatever the user did.
 *
 * `Moderation`, `Approval` and `Rejected` are still empty, and that is a
 * property of the SERVER, not of this file. `application_versions.status`
 * takes three values in the Go service — `draft`, `published`, `embedded` —
 * and there is no `on_moderation`, no `user_approval` and no `rejected`
 * anywhere in it: publishing is immediate, with a validation step and no
 * review step. (`centry.moderation_state` is a different feature — a request
 * to access a catalogue entry — and `internal/api/v2/moderation/requests.go`
 * says so in its own header.) The tabs are kept because the reference has
 * them and because a moderation plane would fill them without a UI change;
 * they are not wired to anything that could fill them today.
 */
function usePrivateApplicationTabs(totals: ApplicationsTabTotals): ApplicationTab[] {
  return useMemo(
    () => [
      {
        value: 'all',
        label: 'All',
        count: totals.applicationsTotal,
        content: (
          <PrivateAgentsList
            statuses={undefined}
            cardContentType="all"
          />
        ),
      },
      {
        value: 'drafts',
        label: 'Drafts',
        count: totals.draftTotal,
        content: (
          <PrivateAgentsList
            statuses={['draft']}
            cardContentType="draft"
          />
        ),
      },
      {
        value: 'published',
        label: 'Published',
        count: totals.publishedTotal,
        content: (
          <PrivateAgentsList
            statuses={['published']}
            cardContentType="published"
          />
        ),
      },
      {
        value: 'moderation',
        label: 'Moderation',
        count: totals.moderationTotal,
        content: (
          <PrivateAgentsList
            statuses={['on_moderation']}
            cardContentType="moderation"
          />
        ),
      },
      {
        value: 'approval',
        label: 'Approval',
        count: totals.approvalTotal,
        content: (
          <PrivateAgentsList
            statuses={['user_approval']}
            cardContentType="approval"
          />
        ),
      },
      {
        value: 'rejected',
        label: 'Rejected',
        count: totals.rejectedTotal,
        content: (
          <PrivateAgentsList
            statuses={['rejected']}
            cardContentType="rejected"
          />
        ),
      },
    ],
    [
      totals.applicationsTotal,
      totals.draftTotal,
      totals.publishedTotal,
      totals.moderationTotal,
      totals.approvalTotal,
      totals.rejectedTotal,
    ],
  );
}

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/useApplicationTabs.jsx`
 * — composes the four public (`Latest`/`MyLiked`/`Trending`/admin
 * `PrivateAgentsList`) or six private (`PrivateAgentsList` per status) tab
 * definitions this same unit (A1g) owns.
 *
 * **Disclosed drop:** the baseline's `sortBy`/`sortOrder` (read off
 * `useSearchParams` inside each `use*ApplicationTabs` half) are no longer
 * threaded through as a memo dependency here — `PrivateAgentsList`
 * (this unit) reads them itself off `useSearch` directly, so there is
 * nothing left for this hook to forward. `trendRange` (baseline:
 * `DateRangeSelect`/`useTrendRange`) is dropped entirely: neither that
 * component nor its backing `trend_start_period` filter has a working
 * equivalent in this app (see `Trending.tsx`'s doc comment for the real
 * backend-contract reason) — carrying a dead prop through would misrepresent
 * a composition gap as a wired feature.
 *
 * **Disclosed drop — every tab `icon`, undisclosed until now:** the
 * baseline's `ApplicationTab` entries each also carry an `icon` (public
 * tabs: `Fire`/`HeartIcon`/`Champion`/`AdminIcon` from
 * `@/components/Icons/*`; private tabs: a per-status `<StatusDot
 * status={...} />` from `@/components/StatusDot`, one of
 * `Draft`/`Published`/`OnModeration`/`UserApproval`/`Rejected`) — see the
 * baseline `useApplicationTabs.jsx` cited above, lines 13-17 (imports) and
 * every tab literal's own `icon:` key. This port's `ApplicationTab`
 * interface has no `icon` field and none of the tab objects below set one.
 * Verified genuinely absent, not merely unwired: `find src/shared/ui
 * -iname '*Fire*' -o -iname '*Heart*' -o -iname '*Champion*' -o -iname
 * '*Admin*' -o -iname '*StatusDot*'` (also checked `shared/ui/icons/`,
 * S2's 116-icon port) turns up none of the five — no `FireIcon`/
 * `HeartIcon`/`ChampionIcon`/`AdminIcon`/`StatusDot` equivalent exists
 * anywhere in this app's `shared/ui`. There is nothing for this hook to
 * wire; adding an `icon` field with no real icon behind it would just move
 * the gap into this file's own type without closing it. Promote/build the
 * missing icon set (a small S2 follow-up: 4 new `shared/ui/icons/*` entries
 * plus a `StatusDot` component) and thread `icon: ReactNode` back onto
 * `ApplicationTab` when they land.
 */
export function useApplicationTabs(
  isPublicProject: boolean,
  totals: ApplicationsTabTotals,
  hasAdminPermission: boolean,
): ApplicationTab[] {
  const publicTabs = usePublicApplicationTabs(totals, hasAdminPermission);
  const privateTabs = usePrivateApplicationTabs(totals);
  return isPublicProject ? publicTabs : privateTabs;
}
