import { useEffect, useMemo, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';
import { EntityListRail } from '@/shared/ui/EntityRail';
import { useSidebarCollapsedStore } from '@/widgets/sidebar';

import { isPublicAgentsProject } from './lib/isPublicAgentsProject';
import { useHasAdminPermission } from './lib/useHasAdminPermission';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { useApplicationsData } from './useApplicationsData';
import { useApplicationTabs } from './useApplicationTabs';

const pageSx: SxProps<Theme> = {
  height: '100%',
  display: 'flex',
  flexDirection: 'column',
};

const tabBarSx: SxProps<Theme> = {
  flexShrink: 0,
  borderBottom: 1,
  borderColor: 'divider',
  padding: '0 1.5rem',
};

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
 *  - `ToolbarImportButton` (`@/[fsd]/entities/import-wizard/ui`) and
 *    `ViewToggle` (`@/components/ViewToggle`) have no confirmed port
 *    anywhere in `shared/ui`/`widgets` (grepped both trees for both names —
 *    zero hits) and are out of this unit's ownership fence to add.
 *  - `DateRangeSelect`/`useTrendRange` (baseline's Trending-tab-only date
 *    picker) — see `Trending.tsx`'s own doc comment: the backing
 *    `trend_start_period` filter has no server-side support at all, so
 *    there is nothing for a working date picker to control.
 *
 * **The rail is real; the tag FILTER on this page is not — disclosed.** The
 * "Tags" panel lists this project's real tags and writes the selection into
 * the shell-wide `tags[]` search param (linkable, restored on reload), but
 * no application list narrows by it, because elitea-main cannot support it
 * at either end: the applications repo never populates a row's `tags` on a
 * list response (`internal/infra/db/repos/applications.go` — the field is
 * absent, and the version maps hardcode `"tags": []any{}`), and the `tags`
 * request param the handler does read into `ListRequest.Tags`
 * (`internal/api/v2/applications/handler.go:108`) is consumed by nothing.
 * Filtering client-side over rows that carry no tags would empty the list on
 * the first chip click — a worse lie than an unfiltered one. `pages/skills`
 * DOES filter for real (skills rows carry their tags), which is what this
 * looks like once the server catches up.
 */
export function Applications(): ReactNode {
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as AgentsRouteParams;
  const projectId = useSelectedProjectId();
  const isPublicProject = isPublicAgentsProject(projectId);
  const hasAdminPermission = useHasAdminPermission(isPublicProject ? projectId : undefined);
  const navRailCollapsed = useSidebarCollapsedStore((state) => state.collapsed);
  const totals = useApplicationsData(projectId, hasAdminPermission);
  const tabs = useApplicationTabs(isPublicProject, totals, hasAdminPermission);

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

  return (
    <Box sx={pageSx}>
      <Box sx={tabBarSx}>
        <BaseTabs
          value={selectedIndex === -1 ? false : selectedIndex}
          onChange={handleChangeTab}
          aria-label={t('pages.agents.applications.tabsAriaLabel', 'Agents')}
        >
          {visibleTabs.map((tab) => (
            <BaseTab
              key={tab.value}
              label={tab.count === undefined ? tab.label : `${tab.label} (${tab.count})`}
              data-testid={`agents-tab-${tab.value}`}
            />
          ))}
        </BaseTabs>
      </Box>
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
