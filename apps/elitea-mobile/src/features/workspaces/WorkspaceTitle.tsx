import { router } from 'expo-router';
import { Pressable, StyleSheet, Text, View } from 'react-native';

import { usePalette } from '@/constants/use-palette';
import { displayHost } from '@/features/connect/connectFlow';
import { WorkspaceMark } from '@/features/workspaces/WorkspaceMark';
import { useSession } from '@/state/session';
import { useBrandTheme } from '@/theme/brandStore';

/**
 * Header title of every signed-in screen: the screen title over the ACTIVE
 * workspace's host, with its mark. The host is always on screen, so the user
 * always knows which workspace they are typing into (two workspaces may share
 * a brand). Tapping it opens the workspace switcher.
 */
export function WorkspaceTitle({ title }: { title: string }) {
  const palette = usePalette();
  const origin = useSession((s) => s.serverOrigin);
  const brand = useBrandTheme();
  if (origin === null) return <Text style={[styles.title, { color: palette.text }]}>{title}</Text>;
  const host = displayHost(origin);
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={`${title}. Workspace ${host}. Switch workspace`}
      testID="workspace-title"
      onPress={() => router.push('/workspaces')}
      style={styles.box}>
      <Text numberOfLines={1} style={[styles.title, { color: palette.text }]}>
        {title}
      </Text>
      <View style={styles.hostRow}>
        <WorkspaceMark origin={origin} image={brand?.origin === origin ? (brand.logoMark ?? null) : null} size={14} />
        <Text numberOfLines={1} style={[styles.host, { color: palette.textSecondary }]}>
          {host} ▾
        </Text>
      </View>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  box: { alignItems: 'center', maxWidth: 220 },
  title: { fontSize: 17, fontWeight: '600' },
  hostRow: { flexDirection: 'row', alignItems: 'center', gap: 4 },
  host: { fontSize: 12 },
});
