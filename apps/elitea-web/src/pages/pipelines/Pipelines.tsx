import { useEffect, useMemo, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';

import { useNavigate, useParams } from '@tanstack/react-router';

import { EntityImportButton, useEntityImport } from '@/features/agent-lifecycle';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { EntityListFilters, useRailTagSelection } from '@/shared/ui/EntityRail';
import { EntityFolderPanel } from '@/widgets/entity-folders';
import { ListViewToggle, PageToolbar, pageBodySx, pageSx } from '@/widgets/page-header';
import { usePermissionSet } from '@/widgets/sidebar';

import { isPublicPipelinesProject } from './lib/isPublicPipelinesProject';
import { useHasAdminPermission } from './lib/useHasAdminPermission';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { usePipelinesData } from './usePipelinesData';
import { usePipelineTabs } from './usePipelineTabs';

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
 * **The page's one toolbar** — `widgets/page-header`'s `PageToolbar`, as on
 * `pages/agents/Applications.tsx` (see its module comment), with
 * `pageKey="pipelines"` and "+ New pipeline".
 *  - `DateRangeSelect`/`useTrendRange` — see `Trending.tsx`'s own doc
 *    comment: the backing `trend_start_period` filter has no server-side
 *    support at all, so there is nothing for a working date picker to
 *    control.
 *
 * **The Filters tab is real; the tag FILTER on this page is not — disclosed.** The
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
  const permissions = usePermissionSet(projectId);
  const canCreate = permissions.has(PERMISSIONS.applications.create) && !isPublicProject;
  const { selectedTags } = useRailTagSelection();
  const totals = usePipelinesData(projectId, hasAdminPermission);
  const entityImport = useEntityImport(projectId);
  const tabs = usePipelineTabs(isPublicProject, totals, hasAdminPermission);
  const title = t('pages.pipelines.pipelines.tabsAriaLabel', 'Pipelines');

  const visibleTabs = useMemo(() => tabs.filter((tab) => tab.hidden !== true), [tabs]);
  const selectedIndex = visibleTabs.findIndex((tab) => tab.value === params.tab);

  useEffect(() => {
    if (selectedIndex !== -1) return;
    const firstTab = visibleTabs[0];
    if (firstTab === undefined) return;
    void navigate({ to: '/pipelines/$tab', params: { tab: firstTab.value }, replace: true });
  }, [selectedIndex, visibleTabs, navigate]);

  const handleChangeTab = (_event: SyntheticEvent, nextIndex: number): void => {
    const nextTab = visibleTabs[nextIndex];
    if (nextTab === undefined) return;
    void navigate({ to: '/pipelines/$tab', params: { tab: nextTab.value } });
  };

  const createAction = canCreate
    ? { label: t('pages.pipelines.pipelines.create', 'New pipeline'), testId: 'pipelines-create', onClick: () => void navigate({ to: '/pipelines/create' }) }
    : undefined;

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
          testIdPrefix: 'pipelines-tab',
        }}
        search={{ placeholder: t('pages.pipelines.pipelines.search', 'Search pipelines'), 'data-testid': 'pipeline-search-input' }}
        filters={{ activeCount: selectedTags.length }}
        viewToggle={
          <ListViewToggle
            pageKey="pipelines"
            testIdPrefix="pipeline"
          />
        }
        /* Import (validation-matrix gap 12) and create: hidden on the PUBLIC project's read-only catalogue. */
        secondaryActions={
          isPublicProject ? undefined : (
            <EntityImportButton
              testIdPrefix="pipelines"
              isImporting={entityImport.run.isPending}
              onImport={async (document) => {
                await entityImport.run.mutateAsync(document);
              }}
            />
          )
        }
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
        foldersSlot={
          <EntityFolderPanel
            enabled={!isPublicProject}
            projectId={projectId}
            kind="pipeline"
            permissions={permissions}
          />
        }
      />
    </Box>
  );
}
