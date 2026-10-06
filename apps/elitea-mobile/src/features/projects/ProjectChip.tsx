import { Link } from 'expo-router';
import { StyleSheet, Text } from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { useProjectSelection } from '@/features/projects/useProjectSelection';
import { usePersonalProjectId } from '@/state/selfIdentity';

/** Header chip showing the current project; opens the project switcher. */
export function ProjectChip() {
  const palette = usePalette();
  const { project, projectId } = useProjectSelection();
  const personal = usePersonalProjectId();
  const label =
    projectId !== null && projectId === personal
      ? 'My project'
      : (project?.name ?? (projectId !== null ? `Project ${projectId}` : 'Project'));
  return (
    <Link
      href="/projects"
      accessibilityRole="button"
      accessibilityLabel={`Project: ${label}. Switch project`}
      style={[styles.chip, { borderColor: palette.border }]}>
      <Text numberOfLines={1} style={[styles.text, { color: palette.text }]}>
        {label} ▾
      </Text>
    </Link>
  );
}

const styles = StyleSheet.create({
  chip: {
    borderWidth: 1,
    borderRadius: 14,
    paddingHorizontal: Spacing.two + 2,
    paddingVertical: Spacing.one,
    maxWidth: 180,
  },
  text: { fontSize: 14, fontWeight: '600' },
});
