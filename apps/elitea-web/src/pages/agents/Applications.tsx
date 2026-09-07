import { useEffect, useMemo, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams } from '@tanstack/react-router';

import { EntityImportButton, useEntityImport } from '@/features/agent-lifecycle';
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
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: '1rem',
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
        {/*
         * Import (validation-matrix gap 12). Production carries this control
         * on the Agents, Pipelines and Skills list headers; this app had it on
         * Skills only, so an agent could be exported and never brought back —
         * `POST /elitea_core/import_wizard/prompt_lib/{project}` was generated
         * and had `"usedBy": []`. Hidden for the PUBLIC project's lists, which
         * are read-only catalogues, matching the create button's own rule.
         */}
        {!isPublicProject && (
          <EntityImportButton
            testIdPrefix="agents"
            isImporting={entityImport.run.isPending}
            onImport={async (document) => {
              await entityImport.run.mutateAsync(document);
            }}
          />
        )}
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
