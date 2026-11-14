import { useEffect, useMemo, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';

import { useNavigate, useParams } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { EntityListFilters, useRailTagSelection } from '@/shared/ui/EntityRail';
import { ListViewToggle, PageToolbar, pageBodySx, pageSx } from '@/widgets/page-header';

import { AgentsWorkshop } from './AgentsWorkshop';
import { isPublicAgentsProject } from './lib/isPublicAgentsProject';
import { useHasAdminPermission } from './lib/useHasAdminPermission';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { useApplicationsData } from './useApplicationsData';
import { useApplicationTabs } from './useApplicationTabs';

/** The four public tabs (`latest`/`my-liked`/`trending`) plus `admin` are the ones the baseline pins to "Trending Authors". */
const TRENDING_AUTHOR_TABS: readonly string[] = ['latest', 'my-liked', 'trending', 'admin'];

interface AgentsRouteParams {
  readonly tab?: string;
}

/**
 * `/agents/:tab` (ROUTE-011), ported from
 * `apps/elitea-ui/src/pages/Applications/Applications.jsx`.
 *
 * A private project is the Workshop (`AgentsWorkshop`, ADR-0051 M1): one list
 * of the project's agents with every control in the right sidebar and no
 * status tabs. The PUBLIC project keeps the feed tabs (`Latest`, `Admin`) over
 * its read-only catalogue, under the page toolbar; the Marketplace (ADR-0051
 * M0) replaces them.
 */
export function Applications(): ReactNode {
  const projectId = useSelectedProjectId();
  if (!isPublicAgentsProject(projectId)) return <AgentsWorkshop />;
  return <PublicApplications />;
}

/** The public project's feed: `Latest` (and `Admin` for a person who may list every agent), a read-only catalogue. */
function PublicApplications(): ReactNode {
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as AgentsRouteParams;
  const projectId = useSelectedProjectId();
  const hasAdminPermission = useHasAdminPermission(projectId);
  const { selectedTags } = useRailTagSelection();
  const totals = useApplicationsData(projectId, hasAdminPermission);
  const tabs = useApplicationTabs(totals, hasAdminPermission);
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
        search={{ placeholder: t('pages.agents.applications.search', 'Search agents'), 'data-testid': 'agent-search-input' }}
        filters={{ activeCount: selectedTags.length }}
        viewToggle={<ListViewToggle pageKey="agents" testIdPrefix="agent" />}
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
