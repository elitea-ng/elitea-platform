import { router } from 'expo-router';
import { ActivityIndicator, Pressable, SectionList, StyleSheet, Text, View } from 'react-native';

import { groupProjects, useProjects, type Project } from '@/api/queries/projects';
import { StatusScreen } from '@/components/StatusScreen';
import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { selectProject } from '@/state/projectSelection';
import { usePersonalProjectId } from '@/state/selfIdentity';
import { useSession } from '@/state/session';

/**
 * Project switcher (modal) for the ACTIVE workspace: the personal project
 * first, then the team projects (the client-tagged project list; suspended
 * ones are hidden). The choice is remembered per workspace.
 */
export default function ProjectsScreen() {
  const palette = usePalette();
  const projects = useProjects();
  const selected = useSession((s) => s.projectId);
  const personalId = usePersonalProjectId();

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

  const { personal, team } = groupProjects(projects.data ?? [], personalId);
  const sections = [
    ...(personal ? [{ key: 'personal', title: 'Personal', data: [personal] }] : []),
    ...(team.length > 0 ? [{ key: 'team', title: personal ? 'Team projects' : 'Projects', data: team }] : []),
  ];
  return (
    <SectionList
      style={{ backgroundColor: palette.background }}
      sections={sections}
      keyExtractor={(p: Project) => String(p.id)}
      stickySectionHeadersEnabled={false}
      ListEmptyComponent={
        <Text style={[styles.empty, { color: palette.textSecondary }]}>No active projects.</Text>
      }
      renderSectionHeader={({ section }) => (
        <Text accessibilityRole="header" style={[styles.section, { color: palette.textSecondary }]}>
          {section.title}
        </Text>
      )}
      ItemSeparatorComponent={() => <View style={[styles.separator, { backgroundColor: palette.border }]} />}
      renderItem={({ item }) => (
        <Pressable
          accessibilityRole="button"
          accessibilityState={{ selected: item.id === selected }}
          testID={`project-${item.id}`}
          onPress={() => {
            void selectProject(item.id);
            if (router.canGoBack()) router.back();
          }}
          style={styles.row}>
          <Text numberOfLines={1} style={[styles.name, { color: palette.text }]}>
            {item === personal ? 'My project' : item.name}
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
  section: {
    fontSize: 13,
    fontWeight: '600',
    paddingHorizontal: Spacing.three,
    paddingTop: Spacing.four,
    paddingBottom: Spacing.one,
  },
  action: { fontSize: 16, fontWeight: '600' },
});
