import { router, Stack } from 'expo-router';
import { useEffect, useState } from 'react';
import { RefreshControl } from 'react-native';

import { flattenPublicAgentPages, usePublicAgents } from '@/api/queries/discover';
import { StatusScreen } from '@/components/StatusScreen';
import { useDesign } from '@/design/DesignProvider';
import { DiscoverView, type DiscoverLayout } from '@/features/discover/DiscoverView';
import { useDiscoverAvailable } from '@/features/discover/useDiscoverAvailable';
import { useProjectSelection } from '@/features/projects/useProjectSelection';
import { WorkspacePill } from '@/features/workspaces/WorkspacePill';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { headerActionOptions, searchBarOptions } from '@/ui';

/** The search reaches the server once typing pauses. */
export const SEARCH_DEBOUNCE_MS = 300;

/**
 * Discover: the workspace's public agents (client contract 1.3). Search runs on the server
 * (name and description) from the native search field in the navigation bar; a card opens the
 * agent's sheet, where a chat starts in the active project. The bar's grid/list button switches
 * the layout. The tab is always there (a changing tab set remounts the bar), so the screen
 * gates: it waits while the workspace's discovery document is not yet known and refuses below 1.3.
 */
export default function DiscoverScreen() {
  const design = useDesign();
  const known = useWorkspaceInfo() !== null;
  const available = useDiscoverAvailable();
  const { projectId } = useProjectSelection();
  const [search, setSearch] = useState('');
  const [query, setQuery] = useState('');
  const [layout, setLayout] = useState<DiscoverLayout>('grid');
  useEffect(() => {
    const timer = setTimeout(() => setQuery(search), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [search]);
  const agents = usePublicAgents(projectId, query, available);
  const rows = flattenPublicAgentPages(agents.data?.pages);

  if (!known) return <StatusScreen busy title="Checking this workspace" />;
  if (!available) {
    return <StatusScreen mascot="bust" title="Discover isn't available" body="This workspace does not offer public agents to mobile yet." />;
  }
  if (projectId === null) return <StatusScreen title="No project selected" />;

  return (
    <>
      <Stack.Screen
        options={{
          ...searchBarOptions(design, { placeholder: 'Search public agents', onChange: setSearch }),
          ...headerActionOptions(
            [
              layout === 'grid'
                ? { id: 'layout', icon: 'list', label: 'Show as list', onPress: () => setLayout('list') }
                : { id: 'layout', icon: 'grid', label: 'Show as grid', onPress: () => setLayout('grid') },
            ],
            design,
          ),
        }}
      />
      <DiscoverView
        agents={rows}
        projectId={projectId}
        layout={layout}
        status={agents.isLoading ? 'loading' : agents.error && rows.length === 0 ? 'error' : 'ready'}
        query={query.trim()}
        onOpen={(agent) => router.push({ pathname: '/discover/[id]', params: { id: String(agent.id) } })}
        onRetry={() => void agents.refetch()}
        // The list is the screen's root scroll view (F1): iOS collapses the large title only when the
        // first child is a scroll view adjusting its inset, and the collapsed inline title is what
        // names the workspace host. While the title is expanded, WorkspacePill shows the host.
        header={<WorkspacePill />}
        refreshControl={
          <RefreshControl
            refreshing={agents.isRefetching && !agents.isFetchingNextPage}
            onRefresh={() => void agents.refetch()}
            tintColor={design.colors.accent}
          />
        }
        loadingMore={agents.isFetchingNextPage}
        onEndReached={() => {
          if (agents.hasNextPage && !agents.isFetchingNextPage) void agents.fetchNextPage();
        }}
      />
    </>
  );
}
