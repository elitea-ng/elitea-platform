import { router } from 'expo-router';
import { ActivityIndicator, FlatList, Pressable, StyleSheet, Text, View } from 'react-native';

import { selectableProjects, useProjects } from '@/api/queries/projects';
import { StatusScreen } from '@/components/StatusScreen';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { selectProject } from '@/state/projectSelection';
import { useSession } from '@/state/session';

/** Project switcher (modal). Suspended projects are hidden. */
export default function ProjectsScreen() {
  const palette = usePalette();
  const projects = useProjects();
  const selected = useSession((s) => s.projectId);

  if (projects.isLoading) {
    return (
      <View style={[styles.center, { backgroundColor: palette.background }]}>
        <ActivityIndicator color={palette.accent} />
      </View>
    );
  }
  if (projects.error) {
    return (
      <StatusScreen title="Couldn't load projects" body="Check your connection and try again.">
        <Pressable accessibilityRole="button" onPress={() => void projects.refetch()}>
          <Text style={[styles.action, { color: palette.accent }]}>Retry</Text>
        </Pressable>
      </StatusScreen>
    );
  }

  const rows = selectableProjects(projects.data ?? []);
  return (
    <FlatList
      style={{ backgroundColor: palette.background }}
      data={rows}
      keyExtractor={(p) => String(p.id)}
      ListEmptyComponent={
        <Text style={[styles.empty, { color: palette.textSecondary }]}>No active projects.</Text>
      }
      ItemSeparatorComponent={() => <View style={[styles.separator, { backgroundColor: palette.border }]} />}
      renderItem={({ item }) => (
        <Pressable
          accessibilityRole="button"
          accessibilityState={{ selected: item.id === selected }}
          onPress={() => {
            void selectProject(item.id);
            if (router.canGoBack()) router.back();
          }}
          style={styles.row}>
          <Text numberOfLines={1} style={[styles.name, { color: palette.text }]}>
            {item.name}
          </Text>
          {item.id === selected ? <Text style={{ color: palette.accent }}>✓</Text> : null}
        </Pressable>
      )}
    />
  );
}

const styles = StyleSheet.create({
  center: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  row: {
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'space-between',
    paddingHorizontal: Spacing.three,
    paddingVertical: Spacing.three,
    gap: Spacing.two,
  },
  name: { fontSize: 16, flexShrink: 1 },
  separator: { height: StyleSheet.hairlineWidth, marginLeft: Spacing.three },
  empty: { padding: Spacing.four, textAlign: 'center', fontSize: 15 },
  action: { fontSize: 16, fontWeight: '600' },
});
