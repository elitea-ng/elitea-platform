import { StyleSheet, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { Icon, Text } from '@/ui';

/**
 * One line above a screen that shows the copy saved on this device (§4.13 banners): a
 * `warningSoft` capsule with the offline symbol AND text, never colour alone.
 */
export function OfflineNotice({ visible }: { visible: boolean }) {
  const { colors } = useDesign();
  if (!visible) return null;
  return (
    <View accessibilityRole="summary" style={styles.wrap}>
      <View style={[styles.bar, { backgroundColor: colors.warningSoft }]}>
        <Icon name="offline" size={16} tone="warning" />
        <Text variant="footnote" tone="warning" style={styles.text}>
          Offline. Showing what was saved on this device.
        </Text>
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  wrap: { paddingHorizontal: 16, paddingVertical: 8 },
  bar: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingHorizontal: 12, paddingVertical: 8, borderRadius: 9999, borderCurve: 'continuous' },
  text: { flexShrink: 1 },
});
