import { router } from 'expo-router';
import { useEffect, useState } from 'react';
import { ActivityIndicator, FlatList, Pressable, RefreshControl, StyleSheet, Text, TextInput, View } from 'react-native';

import { flattenPublicAgentPages, usePublicAgents } from '@/api/queries/discover';
import { StatusScreen } from '@/components/StatusScreen';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { AgentIcon } from '@/features/discover/AgentIcon';
import { TagList } from '@/features/discover/TagList';
import { useDiscoverAvailable } from '@/features/discover/useDiscoverAvailable';
import { useProjectSelection } from '@/features/projects/useProjectSelection';
import { WorkspacePill } from '@/features/workspaces/WorkspacePill';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { ramp, useLayoutMargin } from '@/ui';

/** The search reaches the server once typing pauses. */
export const SEARCH_DEBOUNCE_MS = 300;

/**
 * Discover: the workspace's public agents (client contract 1.3). Search runs
 * on the server (name and description); a row opens the agent's details,
 * where a chat starts in the active project. The tab is always there (a
 * changing tab set remounts the bar), so the screen gates: it waits while the
 * workspace's discovery document is not yet known and refuses below 1.3.
 */
export default function DiscoverScreen() {
  const palette = usePalette();
  const margin = useLayoutMargin();
  const known = useWorkspaceInfo() !== null;
  const available = useDiscoverAvailable();
  const { projectId } = useProjectSelection();
  const [search, setSearch] = useState('');
  const [query, setQuery] = useState('');
  useEffect(() => {
    const timer = setTimeout(() => setQuery(search), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [search]);
  const agents = usePublicAgents(projectId, query, available);
  const rows = flattenPublicAgentPages(agents.data?.pages);

  if (!known) return <StatusScreen busy title="Checking this workspace" />;
  if (!available) {
    return <StatusScreen title="Discover isn't available" body="This workspace does not offer public agents to mobile yet." />;
  }
  if (projectId === null) return <StatusScreen title="No project selected" />;

  const status = agents.isLoading ? (
    <ActivityIndicator style={styles.state} color={palette.accent} />
  ) : agents.error && rows.length === 0 ? (
    <Pressable accessibilityRole="button" onPress={() => void agents.refetch()} style={styles.state}>
      <Text style={{ color: palette.accent }}>Couldn&apos;t load public agents. Retry</Text>
    </Pressable>
  ) : (
    <Text style={[styles.empty, { color: palette.textSecondary }]}>
      {query.trim() !== '' ? 'No public agents match your search.' : 'This workspace has no public agents yet.'}
    </Text>
  );

  // The list is the screen's root scroll view (F1): iOS collapses the large title only when the
  // first child is a scroll view adjusting its inset, and the collapsed inline title is what
  // names the workspace host. While the title is expanded, WorkspacePill shows the host.
  return (
    <FlatList
      contentInsetAdjustmentBehavior="automatic"
      keyboardDismissMode="on-drag"
      style={[styles.flex, { backgroundColor: palette.background }]}
      data={agents.isLoading || (agents.error && rows.length === 0) ? [] : rows}
      keyExtractor={(agent) => String(agent.id)}
      keyboardShouldPersistTaps="handled"
      refreshControl={
        <RefreshControl
          refreshing={agents.isRefetching && !agents.isFetchingNextPage}
          onRefresh={() => void agents.refetch()}
          tintColor={palette.accent}
        />
      }
      ListHeaderComponent={
        <>
          <WorkspacePill />
          <TextInput
            value={search}
            onChangeText={setSearch}
            placeholder="Search public agents"
            placeholderTextColor={palette.textSecondary}
            autoCorrect={false}
            clearButtonMode="while-editing"
            returnKeyType="search"
            onSubmitEditing={() => setQuery(search)}
            accessibilityLabel="Search public agents"
            style={[styles.search, { marginHorizontal: margin, color: palette.text, backgroundColor: palette.field }]}
          />
        </>
      }
      onEndReachedThreshold={0.5}
      onEndReached={() => {
        if (agents.hasNextPage && !agents.isFetchingNextPage) void agents.fetchNextPage();
      }}
      ListEmptyComponent={status}
      ListFooterComponent={agents.isFetchingNextPage ? <ActivityIndicator style={styles.state} color={palette.accent} /> : undefined}
      ItemSeparatorComponent={() => <View style={[styles.separator, { backgroundColor: palette.border }]} />}
      renderItem={({ item }) => (
        <Pressable
          testID={`public-agent-${item.id}`}
          accessibilityRole="button"
          accessibilityLabel={item.name}
          accessibilityHint="Opens the agent's details"
          onPress={() => router.push({ pathname: '/discover/[id]', params: { id: String(item.id) } })}
          style={styles.row}>
          <AgentIcon name={item.name} iconPath={item.iconPath} projectId={projectId} />
          <View style={styles.text}>
            <Text numberOfLines={1} style={[styles.name, { color: palette.text }]}>
              {item.name}
            </Text>
            {item.description ? (
              <Text numberOfLines={2} style={[styles.description, { color: palette.textSecondary }]}>
                {item.description}
              </Text>
            ) : null}
            <TagList category={item.category} tags={item.tags} limit={3} />
          </View>
        </Pressable>
      )}
    />
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  search: { ...ramp.body, marginBottom: Spacing.three, borderRadius: 10, paddingHorizontal: Spacing.three, paddingVertical: Spacing.two },
  state: { padding: Spacing.four, alignItems: 'center' },
  empty: { ...ramp.subheadline, textAlign: 'center', padding: Spacing.four },
  separator: { height: StyleSheet.hairlineWidth, marginLeft: Spacing.three + 40 + Spacing.three },
  row: { flexDirection: 'row', alignItems: 'flex-start', gap: Spacing.three, paddingHorizontal: Spacing.three, paddingVertical: Spacing.three },
  text: { flex: 1, gap: Spacing.one },
  name: { ...ramp.body, fontWeight: '500' },
  description: { ...ramp.footnote },
});
