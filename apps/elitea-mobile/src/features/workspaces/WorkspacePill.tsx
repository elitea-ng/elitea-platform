import { router } from 'expo-router';
import { Pressable, StyleSheet, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { displayHost } from '@/features/connect/connectFlow';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import { useSession } from '@/state/session';
import type { BrandImage } from '@/theme/brandPack';
import { useBrandTheme } from '@/theme/brandStore';
import { hitSlopFor, Icon, ripple, Text, uiPlatform, useLayoutMargin } from '@/ui';

/**
 * The active workspace under a large title (design-system §3.5): the logo mark (or a monogram
 * tile) and the host in the SYSTEM font, so the user always sees which workspace a list
 * belongs to while the large title is expanded (the collapsed inline title carries the host
 * itself, `WorkspaceTitle`). Tapping it opens the workspace switcher.
 */
export function WorkspacePill() {
  const origin = useSession((s) => s.serverOrigin);
  const brand = useBrandTheme();
  if (origin === null) return null;
  return <WorkspacePillView origin={origin} image={brand?.origin === origin ? (brand.logoMark ?? null) : null} />;
}

/** The pill itself, for a given workspace; the `__DEV__` tab preview draws it with a sample host. */
export function WorkspacePillView({ origin, image }: { origin: string; image: BrandImage | null }) {
  const design = useDesign();
  // The same leading edge as the large title and the search field above it (D7).
  const margin = useLayoutMargin();
  const host = displayHost(origin);
  return (
    <View style={[styles.wrap, { paddingHorizontal: margin }]}>
      <Pressable
        testID="workspace-pill"
        accessibilityRole="button"
        accessibilityLabel={`Workspace ${host}. Switch workspace`}
        onPress={() => router.push('/workspaces')}
        hitSlop={hitSlopFor(36)}
        android_ripple={ripple(design)}
        style={({ pressed }) => [
          styles.pill,
          { backgroundColor: design.colors.surface, borderColor: design.colors.separator },
          pressed && uiPlatform === 'ios' ? styles.pressed : null,
        ]}>
        <WorkspaceMark origin={origin} image={image} size={20} />
        <Text variant="footnote" tone="secondary" numberOfLines={1} style={styles.host}>
          {host}
        </Text>
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
    gap: 6,
    minHeight: 36,
    paddingHorizontal: 10,
    borderRadius: 9999,
    borderCurve: 'continuous',
    borderWidth: StyleSheet.hairlineWidth,
    overflow: 'hidden',
    maxWidth: '100%',
  },
  host: { flexShrink: 1 },
  pressed: { opacity: 0.7 },
});
