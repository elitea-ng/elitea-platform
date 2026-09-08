import { useEffect, useMemo, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams } from '@tanstack/react-router';

import { EntityImportButton, useEntityImport } from '@/features/agent-lifecycle';
import { t } from '@/shared/i18n';
import { EntityListRail } from '@/shared/ui/EntityRail';
import { ListSearchField, ListViewToggle, PageHeader } from '@/widgets/page-header';
import { useSidebarCollapsedStore } from '@/widgets/sidebar';

import { isPublicPipelinesProject } from './lib/isPublicPipelinesProject';
import { useHasAdminPermission } from './lib/useHasAdminPermission';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { usePipelinesData } from './usePipelinesData';
import { usePipelineTabs } from './usePipelineTabs';

const pageSx: SxProps<Theme> = {
  height: '100%',
  display: 'flex',
  flexDirection: 'column',
};


const tabPanelSx: SxProps<Theme> = {
  flex: 1,
  minHeight: 0,
  overflowY: 'auto',
};

/** `CARD_LIST_WIDTH` (`apps/elitea-ui/src/common/constants.js:511`) — see `pages/agents/Applications.tsx` for the shared rationale. */

/** The public feeds plus the Admin tab pin the rail to "Trending Authors" (`pages/Applications/PrivateAgentsList.jsx:141-151`, the same component the pipelines domain reuses). */
const TRENDING_AUTHOR_TABS: readonly string[] = ['latest', 'my-liked', 'trending', 'admin'];

interface PipelinesRouteParams {
  readonly tab?: string;
}

/**
 * Ported from `apps/elitea-ui/src/pages/Pipelines/Pipelines.jsx` —
 * ROUTE-019 `/pipelines/:tab` (spec §8.1), the top-level tab shell over
 * `Latest`/`MyLiked`/`Trending`/`PrivatePipelinesList` (all this unit,
 * A2m). Structurally the pipelines-domain mirror of `pages/agents/
 * Applications.tsx` (Wave-2 unit A1g) — a Pipeline literally IS an
 * Application row (`agent_type: 'pipeline'`, see `entities/pipeline/model/
 * types.ts`'s own doc comment) — narrowed to the ONE private "All" tab the
 * baseline's own `Pipelines.jsx` actually renders (see `usePipelineTabs.tsx`
 * doc comment) rather than the agents domain's six-status split.
 *
 * **Composition gaps, disclosed (same pattern `pages/agents/Applications.tsx`
 * established for its own not-yet-landed siblings):**
 *  - `SingleSelect`-based status filter dropdown (`Pipelines.jsx`'s own
 *    `middleTabComponent`, `statusFilterWrapper`/`statusOptions`) has no
 *    confirmed port in this unit's ownership fence; `BaseTabs` already
 *    provides equivalent tab selection via click, so this page does not
 *    duplicate it.
 *  - `ToolbarImportButton` (`@/[fsd]/entities/import-wizard/ui`) is mounted
 *    now, as `EntityImportButton` in the header's `actions` slot.
 *
 * **Search and the view switch are real now** — `widgets/page-header`'s
 * `ListSearchField` and `ListViewToggle`, in this header's own slots. See
 * `pages/agents/Applications.tsx`'s module comment for the full contract;
 * this page mounts the identical pair against `pageKey="pipelines"`.
 *  - `DateRangeSelect`/`useTrendRange` — see `Trending.tsx`'s own doc
 *    comment: the backing `trend_start_period` filter has no server-side
 *    support at all, so there is nothing for a working date picker to
 *    control.
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
export function Pipelines(): ReactNode {
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as PipelinesRouteParams;
  const projectId = useSelectedProjectId();
  const isPublicProject = isPublicPipelinesProject(projectId);
  const hasAdminPermission = useHasAdminPermission(isPublicProject ? projectId : undefined);
  const navRailCollapsed = useSidebarCollapsedStore((state) => state.collapsed);
  const totals = usePipelinesData(projectId, hasAdminPermission);
  const entityImport = useEntityImport(projectId);
  const tabs = usePipelineTabs(isPublicProject, totals, hasAdminPermission);

  const visibleTabs = useMemo(() => tabs.filter((tab) => tab.hidden !== true), [tabs]);
  const selectedIndex = visibleTabs.findIndex((tab) => tab.value === params.tab);

  useEffect(() => {
    if (selectedIndex !== -1) return;
    const firstTab = visibleTabs[0];
    if (firstTab === undefined) return;
    void navigate({ to: '/pipelines/$tab', params: { tab: firstTab.value }, replace: true });
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
    void navigate({ to: '/pipelines/$tab', params: { tab: nextTab.value } });
  };


  return (
    <Box sx={pageSx}>
      <PageHeader
        tabs={{
          items: visibleTabs.map((tab) => ({
            value: tab.value,
            label: tab.count === undefined ? tab.label : `${tab.label} (${tab.count})`,
          })),
          selectedIndex: selectedIndex === -1 ? false : selectedIndex,
          onChange: handleChangeTab,
          ariaLabel: t('pages.pipelines.pipelines.tabsAriaLabel', 'Pipelines'),
          testIdPrefix: 'pipelines-tab',
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
           * reference (`pages/pipelines/…Applications.jsx`/`Pipelines.jsx` mount
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
                search: <ListSearchField data-testid="pipeline-search-input" />,
                viewToggle: (
                  <ListViewToggle
                    pageKey="pipelines"
                    testIdPrefix="pipeline"
                  />
                ),
              }
            : {}),
          actions: isPublicProject ? undefined : (
            <EntityImportButton
              testIdPrefix="pipelines"
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
