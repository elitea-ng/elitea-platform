import { useEffect, useMemo, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';

import { useNavigate, useParams } from '@tanstack/react-router';

import { EntityImportButton, useEntityImport } from '@/features/agent-lifecycle';
import { TEAM_PERMISSIONS, TeamsPanel } from '@/features/agent-teams';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { EntityListFilters, useRailTagSelection } from '@/shared/ui/EntityRail';
import { ListViewToggle, PageToolbar, pageBodySx, pageSx, type PageToolbarProps } from '@/widgets/page-header';
import { usePermissionSet } from '@/widgets/sidebar';

import { isPublicAgentsProject } from './lib/isPublicAgentsProject';
import { useHasAdminPermission } from './lib/useHasAdminPermission';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { useApplicationsData } from './useApplicationsData';
import { TEAMS_TAB, useApplicationTabs } from './useApplicationTabs';

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
 * **The page's one toolbar** (`widgets/page-header`'s `PageToolbar`): the
 * tabs, the search box (`PageSearch`), the Filters button, the card/table
 * switch (`ListViewToggle`), Import and "+ New agent". The page's name is the
 * shell's title row; the tags and authors are the Filters tab of the shell's
 * side panel (`EntityListFilters`). The search text is the route's `query` param and reaches the
 * selected tab from there; the applications list endpoint reads the same
 * name server-side (`ListApplicationsParams.query`), so the filter is not
 * limited to the first fetched page. The view choice is the `view` param
 * `shared/ui/EntityCardList` already obeyed, additionally remembered per page
 * in `localStorage`.
 *
 * **The tag filter is real (issue 841).** The "Tags" panel lists this project's real tags and writes the selection into the
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
  const permissions = usePermissionSet(projectId);
  const canCreate = permissions.has(PERMISSIONS.applications.create) && !isPublicProject;
  const { selectedTags } = useRailTagSelection();
  const totals = useApplicationsData(projectId, hasAdminPermission);
  const entityImport = useEntityImport(projectId);
  const teams = useTeamsTab(projectId, permissions.has(TEAM_PERMISSIONS.list) && !isPublicProject);
  const tabs = useApplicationTabs(isPublicProject, totals, hasAdminPermission, teams);
  const title = t('pages.agents.applications.tabsAriaLabel', 'Agents');

  const visibleTabs = useMemo(() => tabs.filter((tab) => tab.hidden !== true), [tabs]);
  const selectedIndex = visibleTabs.findIndex((tab) => tab.value === params.tab);

  useEffect(() => {
    if (selectedIndex !== -1) return;
    const firstTab = visibleTabs[0];
    if (firstTab === undefined) return;
    void navigate({ to: '/agents/$tab', params: { tab: firstTab.value }, replace: true });
  }, [selectedIndex, visibleTabs, navigate]);

  const handleChangeTab = (_event: SyntheticEvent, nextIndex: number): void => {
    const nextTab = visibleTabs[nextIndex];
    if (nextTab === undefined) return;
    void navigate({ to: '/agents/$tab', params: { tab: nextTab.value } });
  };

  // The Teams tab lists teams, not agents: no agent search, filters, view switch, import or agent create.
  const agentControls = params.tab !== TEAMS_TAB;
  const createAction = canCreate && agentControls
    ? { label: t('pages.agents.applications.create', 'New agent'), testId: 'agents-create', onClick: () => void navigate({ to: '/agents/create' }) }
    : undefined;
  const importButton = isPublicProject || !agentControls ? undefined : (
    <EntityImportButton
      testIdPrefix="agents"
      isImporting={entityImport.run.isPending}
      onImport={async (document) => {
        await entityImport.run.mutateAsync(document);
      }}
    />
  );

  return (
    <Box sx={pageSx}>
      <PageToolbar
        tabs={{
          items: visibleTabs.map((tab) => ({
            value: tab.value,
            label: tab.count === undefined ? tab.label : `${tab.label} (${tab.count})`,
          })),
          selectedIndex: selectedIndex === -1 ? false : selectedIndex,
          onChange: handleChangeTab,
          ariaLabel: title,
          testIdPrefix: 'agents-tab',
        }}
        {...agentToolbar(agentControls, selectedTags.length)}
        /*
         * Import (validation-matrix gap 12) and create. Hidden for the PUBLIC
         * project's lists, which are read-only catalogues.
         */
        secondaryActions={importButton}
        primaryAction={createAction}
      />
      <Box
        sx={pageBodySx}
        role="tabpanel"
      >
        {selectedIndex !== -1 ? visibleTabs[selectedIndex]?.content : null}
      </Box>
      <EntityListFilters
        projectId={projectId}
        subject={title}
        preferTrendingAuthors={TRENDING_AUTHOR_TABS.includes(params.tab ?? '')}
      />
    </Box>
  );
}

/** ADR-0050 decision 8 (issue 1620): the Teams tab's content, for a caller who may list teams. */
function useTeamsTab(projectId: string | undefined, enabled: boolean): ReactNode {
  const navigate = useNavigate();
  return useMemo(
    () => (enabled
      ? <TeamsPanel projectId={projectId} onStarted={(conversation) => void navigate({ to: '/chat/$conversationId', params: { conversationId: conversation.id } })} />
      : undefined),
    [enabled, projectId, navigate],
  );
}

/** The agent lists' search, Filters and view switch; none of them on the Teams tab. */
function agentToolbar(agentControls: boolean, activeTags: number): Pick<PageToolbarProps, 'search' | 'filters' | 'viewToggle'> {
  if (!agentControls) return {};
  return {
    search: { placeholder: t('pages.agents.applications.search', 'Search agents'), 'data-testid': 'agent-search-input' },
    filters: { activeCount: activeTags },
    viewToggle: <ListViewToggle pageKey="agents" testIdPrefix="agent" />,
  };
}
