import { StyleSheet, Text, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';

/** One line above a screen that shows the copy saved on this device. */
export function OfflineNotice({ visible }: { visible: boolean }) {
  const palette = usePalette();
  if (!visible) return null;
  return (
    <View accessibilityRole="summary" style={[styles.bar, { borderColor: palette.border }]}>
      <Text style={[styles.text, { color: palette.textSecondary }]}>
        Offline. Showing what was saved on this device.
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  bar: { paddingHorizontal: Spacing.three, paddingVertical: Spacing.two, borderBottomWidth: StyleSheet.hairlineWidth },
  text: { fontSize: 13, textAlign: 'center' },
});
