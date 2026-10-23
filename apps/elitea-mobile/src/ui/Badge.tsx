import { StyleSheet, View, type ViewProps } from 'react-native';

import { useDesign } from '@/design/DesignProvider';

import { Text } from './Text';

/** Text of a count badge: the number up to 99, then "99+" (§4.1). */
export function badgeText(count: number): string {
  return count > 99 ? '99+' : String(Math.max(0, Math.floor(count)));
}

/**
 * A count badge (`badge`/`badgeText`, proven 4.5:1). Drawn INSIDE its host's frame (top-end
 * corner), never at a negative offset, so a glass capsule or a list cell cannot clip it.
 * Decorative: the host's label carries the count ("Notifications, 3 unread").
 */
export function Badge({ count, style }: { count: number; style?: ViewProps['style'] }) {
  const { colors } = useDesign();
  if (count <= 0) return null;
  return (
    <View
      accessible={false}
      importantForAccessibility="no-hide-descendants"
      accessibilityElementsHidden
      style={[styles.badge, { backgroundColor: colors.badge }, style]}>
      <Text variant="label" color={colors.badgeText} style={styles.text}>
        {badgeText(count)}
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  badge: { minWidth: 18, minHeight: 18, borderRadius: 9, paddingHorizontal: 5, alignItems: 'center', justifyContent: 'center' },
  text: { fontWeight: '700', textAlign: 'center' },
});
