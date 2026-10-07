import { ActivityIndicator, ScrollView, StyleSheet, View } from 'react-native';

import type { Project } from '@/api/queries/projects';
import { useDesign } from '@/design/DesignProvider';
import { displayHost } from '@/features/connect/connectFlow';
import type { ActiveWorkspace } from '@/features/workspaces/activeWorkspace';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import { Button, EmptyState, ListRow, ListSection } from '@/ui';

/** The personal project's label: its server name is an internal handle (`project_user_42`). */
export const PERSONAL_PROJECT_LABEL = 'My project';

/**
 * The project switcher's content (the `/projects` sheet and its `__DEV__` preview draw this):
 * the active workspace (host always shown, its mark in its own accent), then the projects by
 * name with a check on the selected one.
 */
export function ProjectsSheet({
  workspace,
  loading,
  failed,
  onRetry,
  personal,
  team,
  selectedId,
  onSelect,
  onSwitchWorkspace,
}: {
  workspace: ActiveWorkspace | null;
  loading: boolean;
  failed: boolean;
  onRetry: () => void;
  personal: Project | null;
  team: readonly Project[];
  selectedId: number | null;
  onSelect: (id: number) => void;
  onSwitchWorkspace: () => void;
}) {
  const { colors, space } = useDesign();
  const host = workspace ? displayHost(workspace.origin) : null;
  return (
    <ScrollView
      style={{ backgroundColor: colors.elevated }}
      contentInsetAdjustmentBehavior="automatic"
      contentContainerStyle={[styles.content, { paddingHorizontal: space.screenGutterCompact }]}>
      {workspace && host ? (
        <ListSection title="Workspace">
          <ListRow
            title={workspace.name ?? host}
            subtitle={workspace.name ? host : null}
            leading={<WorkspaceMark origin={workspace.origin} image={workspace.logoMark} brand={workspace.brand} size={32} />}
            value="Switch"
            accessibilityLabel={`Workspace ${workspace.name ? `${workspace.name}, ` : ''}${host}`}
            accessibilityHint="Opens your workspaces"
            onPress={onSwitchWorkspace}
          />
        </ListSection>
      ) : null}
      {loading ? (
        <View style={styles.center}>
          <ActivityIndicator color={colors.accent} />
        </View>
      ) : failed ? (
        <EmptyState mascot="none" title="Couldn't load projects" body="Check your connection and try again." />
      ) : personal === null && team.length === 0 ? (
        <EmptyState mascot="bust" title="No active projects" body="Your account has no active project in this workspace." />
      ) : (
        <>
          {personal ? (
            <ListSection title="Personal">
              <ProjectRow project={personal} label={PERSONAL_PROJECT_LABEL} selected={personal.id === selectedId} onSelect={onSelect} />
            </ListSection>
          ) : null}
          {team.length > 0 ? (
            <ListSection title={personal ? 'Team projects' : 'Projects'}>
              {team.map((project) => (
                <ProjectRow key={project.id} project={project} label={project.name} selected={project.id === selectedId} onSelect={onSelect} />
              ))}
            </ListSection>
          ) : null}
        </>
      )}
      {failed ? <Button title="Try again" variant="tonal" onPress={onRetry} /> : null}
    </ScrollView>
  );
}

function ProjectRow({ project, label, selected, onSelect }: { project: Project; label: string; selected: boolean; onSelect: (id: number) => void }) {
  return (
    <ListRow
      testID={`project-${project.id}`}
      icon="project"
      title={label}
      selected={selected}
      onPress={() => onSelect(project.id)}
    />
  );
}

const styles = StyleSheet.create({
  content: { paddingVertical: 16, gap: 24 },
  center: { padding: 24, alignItems: 'center' },
});
