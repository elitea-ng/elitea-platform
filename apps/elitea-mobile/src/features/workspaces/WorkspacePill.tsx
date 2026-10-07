import { router } from 'expo-router';
import { Pressable, StyleSheet, View } from 'react-native';

import type { BrandInput } from '@/design/brand';
import { useDesign } from '@/design/DesignProvider';
import { displayHost } from '@/features/connect/connectFlow';
import { useActiveWorkspace } from '@/features/workspaces/activeWorkspace';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import type { BrandImage } from '@/theme/brandPack';
import { hitSlopFor, Icon, ripple, Text, uiPlatform, useLayoutMargin } from '@/ui';

/**
 * The active workspace under a large title (design-system §3.5): the logo mark (or a monogram
 * tile in the workspace's accent) and the host in the SYSTEM font, so the user always sees which
 * workspace a list belongs to while the large title is expanded (the collapsed inline title
 * carries the host itself, `WorkspaceTitle`). Tapping it opens the workspace switcher.
 */
export function WorkspacePill({ project, onPress }: { project?: string | null; onPress?: () => void } = {}) {
  const workspace = useActiveWorkspace();
  if (workspace === null) return null;
  return (
    <WorkspacePillView
      origin={workspace.origin}
      image={workspace.logoMark}
      brand={workspace.brand}
      name={workspace.name}
      project={project ?? null}
      onPress={onPress}
    />
  );
}

/** The spoken label: the workspace (name and host), the project when shown, and what a tap does. */
export function workspacePillLabel(host: string, name: string | null, project: string | null): string {
  const who = name ? `${name}, ${host}` : host;
  return project ? `Workspace ${who}, project ${project}. Switch workspace or project` : `Workspace ${who}. Switch workspace`;
}

/**
 * The pill itself, for a given workspace; the `__DEV__` previews draw it with a sample host.
 * With a `project` it reads "Name · Project" (Home, where it replaces the header's project chip)
 * and the host stays on its own line under it: the host is never dropped for the name.
 */
export function WorkspacePillView({
  origin,
  image,
  brand = null,
  name = null,
  project = null,
  onPress,
}: {
  origin: string;
  image: BrandImage | null;
  brand?: BrandInput | null;
  name?: string | null;
  project?: string | null;
  onPress?: () => void;
}) {
  const design = useDesign();
  // The same leading edge as the large title and the search field above it (D7).
  const margin = useLayoutMargin();
  const host = displayHost(origin);
  const title = [name, project].filter(Boolean).join(' · ');
  return (
    <View style={[styles.wrap, { paddingHorizontal: margin }]}>
      <Pressable
        testID="workspace-pill"
        accessibilityRole="button"
        accessibilityLabel={workspacePillLabel(host, name, project)}
        onPress={onPress ?? (() => router.push('/workspaces'))}
        hitSlop={hitSlopFor(36)}
        android_ripple={ripple(design)}
        style={({ pressed }) => [
          styles.pill,
          { backgroundColor: design.colors.surface, borderColor: design.colors.separator },
          pressed && uiPlatform === 'ios' ? styles.pressed : null,
        ]}>
        <WorkspaceMark origin={origin} image={image} brand={brand} size={title ? 28 : 20} />
        <View style={styles.text}>
          {title ? (
            <Text variant="footnote" emphasized numberOfLines={design.accessibilityLayout ? undefined : 1}>
              {title}
            </Text>
          ) : null}
          <Text variant={title ? 'caption' : 'footnote'} tone="secondary" numberOfLines={design.accessibilityLayout ? undefined : 1}>
            {host}
          </Text>
        </View>
        <Icon name="chevronDown" size={12} tone="secondary" weight="semibold" />
      </Pressable>
    </View>
  );
}

const styles = StyleSheet.create({
  wrap: { flexDirection: 'row', paddingBottom: 8 },
  pill: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 8,
    minHeight: 36,
    paddingHorizontal: 10,
    paddingVertical: 4,
    borderRadius: 9999,
    borderCurve: 'continuous',
    borderWidth: StyleSheet.hairlineWidth,
    overflow: 'hidden',
    maxWidth: '100%',
  },
  text: { flexShrink: 1 },
  pressed: { opacity: 0.7 },
});
