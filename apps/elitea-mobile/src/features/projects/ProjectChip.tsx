import type { NativeStackNavigationOptions } from 'expo-router';
import { router } from 'expo-router';
import { Pressable, StyleSheet, View } from 'react-native';

import { PERSONAL_PROJECT_LABEL } from '@/features/projects/ProjectsSheet';
import { useProjectSelection } from '@/features/projects/useProjectSelection';
import { usePersonalProjectId } from '@/state/selfIdentity';
import { hitTarget, Icon, ripple, Text, uiPlatform, type UiPlatform } from '@/ui';
import { useDesign } from '@/design/DesignProvider';

/**
 * The active project as the user sees it: "My project" for the personal one, else its NAME;
 * never the internal id (the switcher once read "Project #90504"). "Project" until the list
 * has loaded.
 */
export function useProjectLabel(): string {
  const { project, projectId } = useProjectSelection();
  const personal = usePersonalProjectId();
  return projectLabel(projectId, project?.name ?? null, personal);
}

export function projectLabel(projectId: number | null, name: string | null, personalProjectId: number | null): string {
  if (projectId !== null && projectId === personalProjectId) return PERSONAL_PROJECT_LABEL;
  return name?.trim() || 'Project';
}

export function projectSwitcherLabel(label: string): string {
  return `Project: ${label}. Switch project`;
}

const openProjects = () => router.push('/projects');

/**
 * The project switcher in the navigation bar's leading slot (audit X6): the project name and a
 * chevron, so it reads as a control that opens a picker on every platform (D6). On iOS 26 the
 * system wraps this custom bar item in its Liquid Glass capsule (no border of ours inside it).
 * The name truncates by the space it has and the text size, not by a character count.
 *
 * A native text bar button is not used: UIKit draws a bar button's image INSTEAD of its title,
 * so it cannot show both the name and the disclosure chevron.
 */
export function projectSwitcherOptions(label: string): Pick<NativeStackNavigationOptions, 'headerLeft'> {
  return { headerLeft: () => <ProjectChip label={label} /> };
}

/** The chip's frame: the platform touch target (44 pt iOS, 48 dp Android; F5) and a width cap. */
export function projectChipFrame(on: UiPlatform = uiPlatform) {
  return { minHeight: hitTarget(on), justifyContent: 'center', paddingHorizontal: 8, maxWidth: 180 } as const;
}

export function ProjectChip({ label }: { label: string }) {
  const design = useDesign();
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={projectSwitcherLabel(label)}
      onPress={openProjects}
      android_ripple={ripple(design, true)}
      style={({ pressed }) => [projectChipFrame(), pressed && uiPlatform === 'ios' ? styles.pressed : null]}>
      <View style={styles.row}>
        <Text variant="subheadline" emphasized numberOfLines={1} style={styles.text}>
          {label}
        </Text>
        <Icon name="chevronDown" size={12} tone="secondary" weight="semibold" />
      </View>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  row: { flexDirection: 'row', alignItems: 'center', gap: 4 },
  text: { flexShrink: 1 },
  pressed: { opacity: 0.6 },
});
