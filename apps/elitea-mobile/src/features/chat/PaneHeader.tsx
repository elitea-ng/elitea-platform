import { StyleSheet, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { IconButton, Text, uiPlatform, useLayoutMargin, type HeaderAction } from '@/ui';

/**
 * The bar on top of a detail column (the open chat beside the Chats list at regular width,
 * issue #15): the chat's title as a heading and its actions as plain icon buttons, the same
 * actions its own screen puts in the navigation bar. The navigation bar above both columns is
 * the list's, so the column carries its own; a hairline separates it from the transcript.
 */
export function PaneHeader({ title, actions }: { title: string; actions: readonly HeaderAction[] }) {
  const design = useDesign();
  const margin = useLayoutMargin();
  return (
    <View
      testID="pane-header"
      style={[styles.bar, { paddingStart: margin, paddingEnd: margin - 8, borderBottomColor: design.colors.separator, backgroundColor: design.colors.background }]}>
      <Text variant="headline" heading numberOfLines={1} style={styles.title}>
        {title}
      </Text>
      <View style={styles.actions}>
        {actions.map((action) => (
          <IconButton
            key={action.id}
            icon={action.icon}
            variant="plain"
            size={uiPlatform === 'android' ? 48 : 44}
            accessibilityLabel={action.accessibilityLabel ?? action.label}
            badge={action.badge}
            disabled={action.disabled}
            accessibilityState={action.busy ? { busy: true } : undefined}
            onPress={action.onPress}
          />
        ))}
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  bar: { flexDirection: 'row', alignItems: 'center', minHeight: 52, gap: 8, borderBottomWidth: StyleSheet.hairlineWidth },
  title: { flex: 1 },
  actions: { flexDirection: 'row', alignItems: 'center', gap: 4 },
});
