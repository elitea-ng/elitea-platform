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

/** The search reaches the server once typing pauses. */
export const SEARCH_DEBOUNCE_MS = 300;

/**
 * Discover: the workspace's public agents (client contract 1.3). Search runs
 * on the server (name and description); a row opens the agent's details,
 * where a chat starts in the active project. Hidden on an older workspace.
 */
export default function DiscoverScreen() {
  const palette = usePalette();
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

  if (!available) {
    return <StatusScreen title="Discover isn't available" body="This workspace does not offer public agents to mobile yet." />;
  }
  if (projectId === null) return <StatusScreen title="No project selected" />;

  return (
    <View style={[styles.flex, { backgroundColor: palette.background }]}>
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
        style={[styles.search, { color: palette.text, backgroundColor: palette.backgroundElement }]}
      />
      {agents.isLoading ? (
        <ActivityIndicator style={styles.state} color={palette.accent} />
      ) : agents.error && rows.length === 0 ? (
        <Pressable accessibilityRole="button" onPress={() => void agents.refetch()} style={styles.state}>
          <Text style={{ color: palette.accent }}>Couldn&apos;t load public agents. Retry</Text>
        </Pressable>
      ) : (
        <FlatList
          data={rows}
          keyExtractor={(agent) => String(agent.id)}
          keyboardShouldPersistTaps="handled"
          refreshControl={
            <RefreshControl
              refreshing={agents.isRefetching && !agents.isFetchingNextPage}
              onRefresh={() => void agents.refetch()}
              tintColor={palette.accent}
            />
          }
          onEndReachedThreshold={0.5}
          onEndReached={() => {
            if (agents.hasNextPage && !agents.isFetchingNextPage) void agents.fetchNextPage();
          }}
          ListEmptyComponent={
            <Text style={[styles.empty, { color: palette.textSecondary }]}>
              {query.trim() !== '' ? 'No public agents match your search.' : 'This workspace has no public agents yet.'}
            </Text>
          }
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
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  search: { margin: Spacing.three, borderRadius: 10, paddingHorizontal: Spacing.three, paddingVertical: Spacing.two, fontSize: 16 },
  state: { padding: Spacing.four, alignItems: 'center' },
  empty: { textAlign: 'center', padding: Spacing.four, fontSize: 15 },
  separator: { height: StyleSheet.hairlineWidth, marginLeft: Spacing.three + 40 + Spacing.three },
  row: { flexDirection: 'row', alignItems: 'flex-start', gap: Spacing.three, paddingHorizontal: Spacing.three, paddingVertical: Spacing.three },
  text: { flex: 1, gap: Spacing.one },
  name: { fontSize: 16, fontWeight: '500' },
  description: { fontSize: 13 },
});
