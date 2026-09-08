import { useEffect, useMemo, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams } from '@tanstack/react-router';

import { EntityImportButton, useEntityImport } from '@/features/agent-lifecycle';
import { t } from '@/shared/i18n';
import { EntityListRail, RAIL_CONTENT_WIDTH, useEntityRailVisible } from '@/shared/ui/EntityRail';
import { ListSearchField, ListViewToggle, PageHeader } from '@/widgets/page-header';
import { useSidebarCollapsedStore } from '@/widgets/sidebar';

import { isPublicAgentsProject } from './lib/isPublicAgentsProject';
import { useHasAdminPermission } from './lib/useHasAdminPermission';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { useApplicationsData } from './useApplicationsData';
import { useApplicationTabs } from './useApplicationTabs';

/**
 * The page's own column, narrowed while the rail is on screen.
 *
 * `EntityRail` is `position: fixed` at `right: 12px` with a `z-index` of
 * 1000 and the full viewport height, so anything the page draws under it is
 * unreachable — the pointer lands on the rail. The header's right-hand
 * controls (the table/card switch, the Import button) sat exactly there:
 * clicking the switch did nothing at all at any viewport wide enough to show
 * the rail, which is every viewport at or above 800px.
 *
 * `CARD_LIST_WIDTH` (`apps/elitea-ui/src/common/constants.js:511`) is the
 * baseline's answer, and `pages/skills/Skills.tsx` already applies it. The
 * width goes on the page column rather than on the header alone, so the tab
 * panel below it lines up with the header instead of running under the rail.
 */
const pageSx = (railVisible: boolean): SxProps<Theme> => ({
  height: '100%',
  display: 'flex',
  flexDirection: 'column',
  width: railVisible ? RAIL_CONTENT_WIDTH : '100%',
});


const tabPanelSx: SxProps<Theme> = {
  flex: 1,
  minHeight: 0,
  overflowY: 'auto',
};

/** The four public tabs (`latest`/`my-liked`/`trending`) plus `admin` are the ones the baseline pins to "Trending Authors" (`RightInfoPanel` picks per-project elsewhere; `PrivateAgentsList.jsx:141-151` hard-codes it for Admin). */
const TRENDING_AUTHOR_TABS: readonly string[] = ['latest', 'my-liked', 'trending', 'admin'];

interface AgentsRouteParams {
  readonly tab?: string;
}

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/Applications.jsx` —
 * ROUTE-011 `/agents/:tab` (spec §8.1), the top-level tab shell over
 * `Latest`/`MyLiked`/`Trending`/`PrivateAgentsList` (all this unit, A1g).
 *
 * **Composition gaps, disclosed (same pattern `pages/apps/Apps.tsx`
 * established for its own not-yet-landed sibling):**
 *  - `StatusFilterSelect` — old app's public-project status/sort
 *    dropdown (`pages/Applications/Components/StatusFilterSelect.jsx`) —
 *    belongs to sub-unit A1f, a sibling of this unit under the same
 *    `agents` domain sub-partition. `src/features/agents/index.ts` (its
 *    public API) does not exist yet as of this unit landing (verified:
 *    `src/features/agents/` has `api/`/`lib/`/`model/` files from other
 *    landed siblings but no `ui/` directory and no barrel) — importing it
 *    would be both a broken import and, even once it lands, this page
 *    would need updating anyway to pass the real props, so nothing is
 *    faked here in the meantime.
 *  - `ToolbarImportButton` (`@/[fsd]/entities/import-wizard/ui`) is mounted
 *    now, as `EntityImportButton` in the header's `actions` slot.
 *  - `DateRangeSelect`/`useTrendRange` (baseline's Trending-tab-only date
 *    picker) — see `Trending.tsx`'s own doc comment: the backing
 *    `trend_start_period` filter has no server-side support at all, so
 *    there is nothing for a working date picker to control.
 *
 * **Search and the view switch are real now.** `ViewToggle`
 * (`@/components/ViewToggle` in the baseline) and the rail's search box
 * (`components/RightPanel.jsx:67-86`) were both disclosed here as having no
 * port. They are `widgets/page-header`'s `ListViewToggle` and
 * `ListSearchField`, mounted in this header's own `viewToggle`/`search`
 * slots. The search text is the route's `query` param and reaches the
 * selected tab from there; the applications list endpoint reads the same
 * name server-side (`ListApplicationsParams.query`), so the filter is not
 * limited to the first fetched page. The view choice is the `view` param
 * `shared/ui/EntityCardList` already obeyed, additionally remembered per page
 * in `localStorage`.
 *
 * **The rail and its tag filter are both real (issue 841).** The "Tags"
 * panel lists this project's real tags and writes the selection into the
 * shell-wide `tags[]` search param (linkable, restored on reload), and
 * `PrivateAgentsList`/`PrivatePipelinesList` send that selection to the
 * server as the `tags` request param.
 *
 * It was decorative until the server caught up at both ends: the
 * applications repo populated no row's `tags` on a list response, and the
 * `tags` param the handler read into `ListRequest.Tags` reached a query that
 * never mentioned it. `internal/infra/db/repos/applications.go` `List` now
 * aggregates the tag names of every version onto the row and applies the
 * filter with AND matching, so the cards carry their tags and a chip click
 * narrows the list.
 */
export function Applications(): ReactNode {
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as AgentsRouteParams;
  const projectId = useSelectedProjectId();
  const isPublicProject = isPublicAgentsProject(projectId);
  const hasAdminPermission = useHasAdminPermission(isPublicProject ? projectId : undefined);
  const navRailCollapsed = useSidebarCollapsedStore((state) => state.collapsed);
  // The same answer the rail itself gives, so the column and the rail can
  // never disagree about whether the rail is on screen.
  const railVisible = useEntityRailVisible(navRailCollapsed);
  const totals = useApplicationsData(projectId, hasAdminPermission);
  const entityImport = useEntityImport(projectId);
  const tabs = useApplicationTabs(isPublicProject, totals, hasAdminPermission);

  const visibleTabs = useMemo(() => tabs.filter((tab) => tab.hidden !== true), [tabs]);
  const selectedIndex = visibleTabs.findIndex((tab) => tab.value === params.tab);

  useEffect(() => {
    if (selectedIndex !== -1) return;
    const firstTab = visibleTabs[0];
    if (firstTab === undefined) return;
    void navigate({ to: '/agents/$tab', params: { tab: firstTab.value }, replace: true });
  }, [selectedIndex, visibleTabs, navigate]);

  /**
   * The tabs whose body is a real list. `MyLiked`/`Trending` are disclosed
   * empty states (their own module comments record the absent server filters),
   * so the header's search box and view switch have nothing to act on there.
   */
  const searchableTab = params.tab !== 'my-liked' && params.tab !== 'trending';

  const handleChangeTab = (_event: SyntheticEvent, nextIndex: number): void => {
    const nextTab = visibleTabs[nextIndex];
    if (nextTab === undefined) return;
    void navigate({ to: '/agents/$tab', params: { tab: nextTab.value } });
  };

  return (
    <Box sx={pageSx(railVisible)}>
      <PageHeader
        tabs={{
          items: visibleTabs.map((tab) => ({
            value: tab.value,
            label: tab.count === undefined ? tab.label : `${tab.label} (${tab.count})`,
          })),
          selectedIndex: selectedIndex === -1 ? false : selectedIndex,
          onChange: handleChangeTab,
          ariaLabel: t('pages.agents.applications.tabsAriaLabel', 'Agents'),
          testIdPrefix: 'agents-tab',
        }}
        /*
         * Import (validation-matrix gap 12). Production carries this control
         * on the Agents, Pipelines and Skills list headers; this app had it on
         * Skills only, so an agent could be exported and never brought back —
         * `POST /elitea_core/import_wizard/prompt_lib/{project}` was generated
         * and had `"usedBy": []`. Hidden for the PUBLIC project's lists, which
         * are read-only catalogues, matching the create button's own rule.
         */
        slots={{
          /*
           * The search box and the table/card switch, both restored from the
           * reference (`pages/agents/…Applications.jsx`/`Pipelines.jsx` mount
           * `ViewToggle` in the header's middle slot; `components/
           * RightPanel.jsx` mounts the search box in the rail). Both were
           * disclosed as unported by this file's own module comment.
           *
           * Rendered only for the tabs that HAVE a list — `MyLiked` and
           * `Trending` render a "not available yet" notice with no rows, so a
           * search box and a view switch over them would be two controls that
           * change nothing.
           */
          ...(searchableTab
            ? {
                search: <ListSearchField data-testid="agent-search-input" />,
                viewToggle: (
                  <ListViewToggle
                    pageKey="agents"
                    testIdPrefix="agent"
                  />
                ),
              }
            : {}),
          actions: isPublicProject ? undefined : (
            <EntityImportButton
              testIdPrefix="agents"
              isImporting={entityImport.run.isPending}
              onImport={async (document) => {
                await entityImport.run.mutateAsync(document);
              }}
            />
          ),
        }}
      />
      <Box
        sx={tabPanelSx}
        role="tabpanel"
      >
        {selectedIndex !== -1 ? visibleTabs[selectedIndex]?.content : null}
      </Box>
      <EntityListRail
        projectId={projectId}
        navRailCollapsed={navRailCollapsed}
        preferTrendingAuthors={TRENDING_AUTHOR_TABS.includes(params.tab ?? '')}
      />
    </Box>
  );
}
