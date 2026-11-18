import { useEffect, useMemo, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';

import { useNavigate, useParams } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { EntityListFilters, useRailTagSelection } from '@/shared/ui/EntityRail';
import { ListViewToggle, PageToolbar, pageBodySx, pageSx } from '@/widgets/page-header';

import { isPublicPipelinesProject } from './lib/isPublicPipelinesProject';
import { useHasAdminPermission } from './lib/useHasAdminPermission';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { PipelinesWorkshop } from './PipelinesWorkshop';
import { usePipelinesData } from './usePipelinesData';
import { usePipelineTabs } from './usePipelineTabs';

/** The public feeds plus the Admin tab pin the rail to "Trending Authors" (`pages/Applications/PrivateAgentsList.jsx:141-151`, the same component the pipelines domain reuses). */
const TRENDING_AUTHOR_TABS: readonly string[] = ['latest', 'my-liked', 'trending', 'admin'];

interface PipelinesRouteParams {
  readonly tab?: string;
}

/**
 * `/pipelines/:tab` (ROUTE-019), ported from
 * `apps/elitea-ui/src/pages/Pipelines/Pipelines.jsx`.
 *
 * A private project is the Workshop (`PipelinesWorkshop`, ADR-0051 M1): one
 * list with every control in the right sidebar, no toolbar row. The PUBLIC
 * project keeps its feed tabs (`Latest`, `Admin`) under the page toolbar.
 */
export function Pipelines(): ReactNode {
  const projectId = useSelectedProjectId();
  if (!isPublicPipelinesProject(projectId)) return <PipelinesWorkshop />;
  return <PublicPipelines />;
}

/** The public project's feed: `Latest` (and `Admin` for a person who may list every pipeline), a read-only catalogue. */
function PublicPipelines(): ReactNode {
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as PipelinesRouteParams;
  const projectId = useSelectedProjectId();
  const hasAdminPermission = useHasAdminPermission(projectId);
  const { selectedTags } = useRailTagSelection();
  const totals = usePipelinesData(projectId, hasAdminPermission);
  const tabs = usePipelineTabs(totals, hasAdminPermission);
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
