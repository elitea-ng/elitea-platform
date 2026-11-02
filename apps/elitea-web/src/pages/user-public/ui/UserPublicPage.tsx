import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';

import { t } from '@/shared/i18n';
import { MyLibraryStatusOptions, CollectionStatus } from '@/shared/lib/sort-status';
import { EntityListFilters, useRailTagSelection, type RailStatKind } from '@/shared/ui/EntityRail';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import { PageToolbar, pageBodySx, pageSx } from '@/widgets/page-header';

import { useCurrentUserPermissions, useIsPublicProject, useSelectedProjectId } from '../api/useRouterAuth';
import { computeDisplayedTabs } from '../lib/displayed-tabs';
import { UserPublicTabs, type UserPublicTabValue } from '../lib/constants';

import { AllStuffPanel } from './AllStuffPanel';
import { ApplicationsPanel } from './ApplicationsPanel';
import { UnavailablePanel } from './UnavailablePanel';

/**
 * ROUTE-041 `/user-public/:tab` — parity target: `apps/elitea-ui/src/pages/UserPublic/UserPublic.jsx`.
 *
 * Route-target composition only (spec §3.2): `tab`/`statuses`/`authorId`/
 * `authorName` are the route's own param/search state, owned by whichever
 * `src/routes/_shell/user-public/$tab.tsx` route component eventually
 * supplies them (not yet wired to this page — outside this unit's
 * ownership fence, `src/routes/**`; see the A12 report). `projectId`/
 * `permissions` are read internally via the router's root `auth` context
 * seam (`../api/useRouterAuth.ts`), the same pattern sibling unit
 * `features/apps` independently established for the same cross-cutting gap
 * (no shared/entities primitive for "selected project id" exists yet).
 *
 * NOT reproduced (see the A12 report for the full, evidenced list): the
 * Toolkits/MCPs tabs' real data (no instance-listing endpoint exists), the
 * Public-viewMode branch's author narrowing (the public endpoint's response
 * carries no author field), free-text search (`state.search.query` — no
 * search-state primitive exists in Wave 1's output for this page to read),
 * and tag filtering — `tags[]` is wired through `lib/merge-and-sort.ts`, and
 * this page NOW exposes the tag picker (the Filters tab's `RailTagsPanel`, writing
 * the same `tags[]` search param), but the selection is deliberately not
 * threaded into the panels: elitea-main's applications repo neither returns
 * per-row `tags` on a list response nor honours the `tags` request param
 * (`internal/infra/db/repos/applications.go` mentions neither;
 * `internal/api/v2/applications/handler.go:108` reads the param into
 * `ListRequest.Tags`, which nothing consumes). Applying an AND filter over
 * rows that always carry NO tags would empty the list on the first chip
 * click — a worse lie than an unfiltered list. `mergeSortAndFilterByTags`'s
 * `selectedTagIds` argument stays the seam for the day the server sends
 * them.
 */
export interface UserPublicPageProps {
  readonly tab: UserPublicTabValue;
  readonly onTabChange: (tab: UserPublicTabValue) => void;
  readonly statuses: readonly string[];
  readonly onStatusesChange: (statuses: readonly string[]) => void;
  readonly authorId: string;
  readonly authorName: string;
}

/**
 * `/user-public/:tab` matches none of `railStatForPath`'s four prefixes, but
 * its TAB names the entity the author card should count — so the statistic
 * line is keyed off the tab instead (`RailAuthorCard`'s `statKind` prop).
 * `all`/`MCPs` have no counterpart in the baseline's `ROUTE_STATISTIC_MAP`
 * and therefore show the author with no statistic row, exactly as the
 * baseline does on this route.
 */
const TAB_STAT_KIND = {
  agents: 'agents',
  pipelines: 'pipelines',
  toolkits: 'toolkits',
} as const;

/** Split out of the component body to keep its cyclomatic complexity inside the §3.5 budget. */
function railStatKindForTab(tab: UserPublicTabValue): RailStatKind | undefined {
  return tab === 'agents' || tab === 'pipelines' || tab === 'toolkits' ? TAB_STAT_KIND[tab] : undefined;
}

export function UserPublicPage({
  tab,
  onTabChange,
  statuses,
  onStatusesChange,
  authorId,
  authorName,
}: UserPublicPageProps): ReactNode {
  const projectId = useSelectedProjectId() ?? '';
  const permissions = useCurrentUserPermissions();
  const isPublicProject = useIsPublicProject(projectId === '' ? undefined : projectId);

  const { selectedTags } = useRailTagSelection();
  const statKind = railStatKindForTab(tab);

  const displayedTabs = computeDisplayedTabs(permissions, isPublicProject);
  const visibleTabs = useMemo(() => UserPublicTabs.filter((candidate) => displayedTabs[candidate]), [displayedTabs]);

  const activeIndex = useMemo(() => {
    const found = visibleTabs.indexOf(tab);
    return found === -1 ? 0 : found;
  }, [visibleTabs, tab]);
  /**
   * Adversarial-review fix (cluster A12-ui, finding 1): this used to fall
   * back to the LITERAL string `'all'` whenever `visibleTabs[activeIndex]`
   * was `undefined` — which, critically, is not just "the requested `tab`
   * isn't visible, so land on the first visible one instead" (the case
   * `activeIndex`'s own `found === -1 ? 0` fallback exists for). It is ALSO
   * what happens when `visibleTabs` is empty altogether — the parity
   * target's own "no permissions" case (`lib/displayed-tabs.ts`'s doc
   * comment: `permissions.length === 0` maps every tab, including `'all'`,
   * to `false`). In that case `visibleTabs[0]` is `undefined` too, and the
   * old `?? 'all'` fallback manufactured an "active" tab value that was
   * NEVER a member of `displayedTabs`/`visibleTabs` — so `activeTab ===
   * 'all'` below still matched and rendered `AllStuffPanel` (real
   * project-application fetch) for exactly the logged-out/no-permission
   * visitor this gate exists to lock out, even though no tab was ever
   * showing for them to click.
   *
   * `undefined` here (not a manufactured tab value) restores parity with
   * the baseline (`UserPublic.jsx`'s own `tabs.filter(i =>
   * displayedTabs[i.label])` + `tabs[currentTabValue]`, `UserPublic.jsx:
   * 243,298-301`): when nothing is visible, `tabs` is `[]`, `currentTabValue`
   * is `0`, and `tabs[0]` is `undefined` — the baseline's `StickyTabs`
   * renders no tab strip and no panel content for that `undefined`, the
   * same nothing-renders outcome every `activeTab === <value>` check below
   * now produces once `activeTab` is `undefined` instead of a fake `'all'`.
   */
  const activeTab: UserPublicTabValue | undefined = visibleTabs[activeIndex];

  const tabLabels: Readonly<Record<UserPublicTabValue, string>> = {
    all: t('userPublic.tabAll', 'All'),
    agents: t('userPublic.tabAgents', 'Agents'),
    pipelines: t('userPublic.tabPipelines', 'Pipelines'),
    toolkits: t('userPublic.tabToolkits', 'Toolkits'),
    MCPs: t('userPublic.tabMCPs', 'MCPs'),
  };
  return (
    <Box sx={pageSx}>
      <PageToolbar
        tabs={{
          items: visibleTabs.map((visibleTab) => ({ value: visibleTab, label: tabLabels[visibleTab] })),
          selectedIndex: activeTab === undefined ? false : activeIndex,
          onChange: (_event, nextIndex) => {
            const nextTab = visibleTabs[nextIndex];
            if (nextTab !== undefined) onTabChange(nextTab);
          },
          ariaLabel: t('userPublic.tabsAriaLabel', 'User public content'),
        }}
        filters={{ activeCount: selectedTags.length }}
        secondaryActions={
          isPublicProject ? undefined : (
            <SingleSelect
              value={statuses[0] ?? CollectionStatus.All}
              onChange={(next) => onStatusesChange([next])}
              options={[...MyLibraryStatusOptions]}
            />
          )
        }
      />
      <Box sx={pageBodySx}>
        <UserPublicTabPanel
          activeTab={activeTab}
          projectId={projectId}
          authorId={authorId}
          authorName={authorName}
          statuses={statuses}
          isPublicProject={isPublicProject}
        />
      </Box>
      <EntityListFilters
        projectId={projectId === '' ? undefined : projectId}
        subject={authorName}
        authorIdFromUrl={authorId}
        {...(statKind === undefined ? {} : { statKind })}
      />
    </Box>
  );
}

interface UserPublicTabPanelProps {
  readonly activeTab: UserPublicTabValue | undefined;
  readonly projectId: string;
  readonly authorId: UserPublicPageProps['authorId'];
  readonly authorName: string;
  readonly statuses: UserPublicPageProps['statuses'];
  readonly isPublicProject: boolean;
}

/** The active tab's content; nothing while no tab is visible (see `activeTab` above). */
function UserPublicTabPanel({ activeTab, projectId, authorId, authorName, statuses, isPublicProject }: UserPublicTabPanelProps): ReactNode {
  const toolkitsUnavailableReason = t(
    'userPublic.toolkitsUnavailable',
    "The generated API's toolkit-listing endpoint returns toolkit TYPE schemas, not this author's configured toolkit instances, so this tab has no data source yet.",
  );
  const mcpsUnavailableReason = t(
    'userPublic.mcpsUnavailable',
    "The generated API's toolkit-listing endpoint returns toolkit TYPE schemas, not this author's configured MCP instances, so this tab has no data source yet.",
  );

  return (
    <>
      {activeTab === 'all' && (
        <AllStuffPanel
          projectId={projectId}
          authorId={authorId}
          authorName={authorName}
          statuses={statuses}
          isPublicProject={isPublicProject}
          enabled
        />
      )}
      {activeTab === 'agents' && (
        <ApplicationsPanel
          projectId={projectId}
          authorId={authorId}
          authorName={authorName}
          statuses={statuses}
          forPipeline={false}
          isPublicProject={isPublicProject}
          enabled
        />
      )}
      {activeTab === 'pipelines' && (
        <ApplicationsPanel
          projectId={projectId}
          authorId={authorId}
          authorName={authorName}
          statuses={statuses}
          forPipeline
          isPublicProject={isPublicProject}
          enabled
        />
      )}
      {activeTab === 'toolkits' && <UnavailablePanel reason={toolkitsUnavailableReason} />}
      {activeTab === 'MCPs' && <UnavailablePanel reason={mcpsUnavailableReason} />}
    </>
  );
}
