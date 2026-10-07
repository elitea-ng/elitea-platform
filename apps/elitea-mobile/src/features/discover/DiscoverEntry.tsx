import { router } from 'expo-router';
import { Pressable, StyleSheet, Text, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { useDiscoverAvailable } from '@/features/discover/useDiscoverAvailable';

/** The way into Discover, above the conversation list; absent on a workspace older than client contract 1.3. */
export function DiscoverEntry() {
  const palette = usePalette();
  if (!useDiscoverAvailable()) return null;
  return (
    <Pressable
      testID="discover-entry"
      accessibilityRole="button"
      accessibilityLabel="Discover public agents"
      onPress={() => router.push('/discover')}
      style={[styles.row, { borderBottomColor: palette.border }]}>
      <View style={styles.text}>
        <Text style={[styles.title, { color: palette.accent }]}>Discover</Text>
        <Text style={[styles.body, { color: palette.textSecondary }]}>Browse public agents and start a chat</Text>
      </View>
      <Text style={[styles.chevron, { color: palette.textSecondary }]}>›</Text>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  row: {
    flexDirection: 'row',
    alignItems: 'center',
    paddingHorizontal: Spacing.three,
    paddingVertical: Spacing.two + 2,
    borderBottomWidth: StyleSheet.hairlineWidth,
  },
  text: { flex: 1 },
  title: { fontSize: 16, fontWeight: '600' },
  body: { fontSize: 13, marginTop: 1 },
  chevron: { fontSize: 22 },
});
