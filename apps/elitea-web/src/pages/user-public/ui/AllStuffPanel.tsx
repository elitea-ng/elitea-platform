import type { ReactNode } from 'react';
import { useMemo } from 'react';

import { useNavigate } from '@tanstack/react-router';

import { t } from '@/shared/i18n';

import { useOwnerApplications } from '../api/useOwnerApplications';
import { useCurrentUserPermissions } from '../api/useRouterAuth';
import { computeDisplayedTabs } from '../lib/displayed-tabs';
import { allStuffEmptyMessage } from '../lib/empty-copy';
import { mapApplicationToListItem } from '../lib/map-application';
import { mergeSortAndFilterByTags } from '../lib/merge-and-sort';
import type { UserPublicListItem } from '../lib/types';

import { EntityListPanel } from './EntityListPanel';
import { UnavailablePanel } from './UnavailablePanel';

/**
 * Detail-page navigation for a merged "All" item (A12-ui adversarial-review
 * fix — see `EntityListPanel`'s doc for why the route decision lives up
 * here, not in that presentational component). Literal `to` strings per
 * branch, each verified directly against its route file's own
 * `createFileRoute(...)` call — `src/routes/_shell/user-public/{agents,
 * pipelines,toolkits,mcps}.$*Id.tsx` (ROUTE-042..045) — not imported from
 * `src/routes/**`: that tree sits above `pages/user-public`'s own
 * composition (pages/routes mount pages, not the reverse). `src/app/router.tsx`'s
 * `declare module '@tanstack/react-router' { interface Register … } }`
 * ambient augmentation still type-checks every branch below against the
 * REAL registered route tree, so a typo or a route rename fails
 * `tsc --noEmit` here too.
 *
 * The `toolkit`/`MCP` branches are unreachable today (no mapper in this
 * unit ever produces those kinds — see `types.ts`'s `EntityKind` doc) but
 * are included for the same reason that union has all four members: this
 * function's parameter type is the one every panel shares, and leaving two
 * kinds unhandled would make navigation silently do nothing for them the
 * moment a future data source lands.
 */
function navigateToEntity(navigate: ReturnType<typeof useNavigate>, item: UserPublicListItem): void {
  switch (item.kind) {
    case 'agent':
      void navigate({ to: '/user-public/agents/$agentId', params: { agentId: item.id } });
      return;
    case 'pipeline':
      void navigate({ to: '/user-public/pipelines/$agentId', params: { agentId: item.id } });
      return;
    case 'toolkit':
      void navigate({ to: '/user-public/toolkits/$toolkitId', params: { toolkitId: item.id } });
      return;
    case 'MCP':
      void navigate({ to: '/user-public/mcps/$mcpId', params: { mcpId: item.id } });
  }
}

export interface AllStuffPanelProps {
  readonly projectId: string;
  readonly authorId: string;
  readonly authorName: string;
  readonly statuses: readonly string[];
  readonly isPublicProject: boolean;
  readonly enabled: boolean;
}

/**
 * ROUTE-041's "All" tab — merges applications and pipelines, newest-first
 * (parity: `AllStuffList.jsx:150-179`, see `lib/merge-and-sort.ts`).
 *
 * Toolkits and MCPs are NOT included in the merge here (the baseline's
 * `AllStuffList` also merges `useLoadToolkits` results into the same list —
 * `AllStuffList.jsx:84-133,150-166`). No instance-listing endpoint exists
 * for either domain in this app's generated client (see
 * `UnavailablePanel`'s doc) — the merged total this panel shows is
 * therefore a real but incomplete subset of the baseline's, a disclosed gap
 * (A12 report), not a silent one.
 *
 * Applications-in-the-merge permission gate (adversarial-review fix,
 * cluster A12-lib, finding 1): the baseline's `AllStuffList` only fetches
 * applications when the viewer can see the standalone "Agents" tab —
 * `useLoadApplications(..., !displayedTabs.agents, false, true)`
 * (`AllStuffList.jsx:44-51`, `displayedTabs` passed down from
 * `UserPublic.jsx` as a prop) — the SAME `models.applications.applications.list`
 * admin-permission gate `usePermissions.jsx:8-10`'s `publicAdminPermissions`
 * applies to that tab (ported here as `lib/displayed-tabs.ts`'s
 * `computeDisplayedTabs`/`AGENTS_TAB_ADMIN_PERMISSION`). Pipelines have NO
 * such entry in `publicAdminPermissions`, so they are always fetched
 * regardless. This component used to fetch+merge applications
 * unconditionally, so a viewer without that permission — for whom the
 * "Agents" tab is correctly hidden — could still see every application via
 * the always-visible "All" tab, silently reopening the gate the baseline
 * enforces. Recomputed locally from the router-context permissions (via
 * `computeDisplayedTabs`, the exact same pure function `UserPublicPage`
 * uses for the tab bar itself) rather than threaded through as a new prop:
 * `UserPublicPage.tsx` is outside this cluster's file scope, and reusing
 * the shared pure function here means the tab-bar gate and this merge gate
 * cannot drift out of sync even though they're computed at two call sites.
 */
export function AllStuffPanel({
  projectId,
  authorId,
  authorName,
  statuses,
  isPublicProject,
  enabled,
}: AllStuffPanelProps): ReactNode {
  const navigate = useNavigate();
  const permissions = useCurrentUserPermissions();
  const hasAgentsPermission = computeDisplayedTabs(permissions, isPublicProject).agents;

  const applications = useOwnerApplications({
    projectId,
    authorId,
    statuses,
    forPipeline: false,
    enabled: enabled && !isPublicProject && hasAgentsPermission,
  });
  const pipelines = useOwnerApplications({
    projectId,
    authorId,
    statuses,
    forPipeline: true,
    enabled: enabled && !isPublicProject,
  });

  const mapped = useMemo(() => {
    const merged = mergeSortAndFilterByTags([applications.items, pipelines.items], []);
    return merged.map((application) => mapApplicationToListItem(application));
  }, [applications.items, pipelines.items]);

  if (isPublicProject) {
    return (
      <UnavailablePanel
        reason={t(
          'userPublic.publicViewModeUnavailable',
          "The public catalog's response does not include author information, so items cannot be narrowed to one author's profile here.",
        )}
      />
    );
  }

  return (
    <EntityListPanel
      items={mapped}
      isLoading={applications.isLoading || pipelines.isLoading}
      isError={applications.isError || pipelines.isError}
      emptyTitle={allStuffEmptyMessage(false, authorName)}
      errorMessage={t('userPublic.loadError', 'Something went wrong loading this list.')}
      loadingMessage={t('userPublic.loading', 'Loading…')}
      onSelect={(item) => {
        navigateToEntity(navigate, item);
      }}
    />
  );
}
