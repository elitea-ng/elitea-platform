import type { ReactNode } from 'react';
import { Pressable, ScrollView, StyleSheet, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { useDesign } from '@/design/DesignProvider';
import { useModalAnimation } from '@/features/a11y/motion';
import { LockAwareModal } from '@/security/lock/LockAwareModal';
import { Icon, pressedOverlay, ripple, Text, uiPlatform, type IconName } from '@/ui';

export interface ActionMenuItem {
  key: string;
  label: string;
  icon: IconName;
  onPress: () => void;
  destructive?: boolean;
  disabled?: boolean;
  /** A choice among several (a radio row with a trailing check when `checked`). */
  checked?: boolean;
}

/**
 * A menu of actions as a bottom sheet (docs/design-system.md §4.10): the M3 modal bottom sheet
 * on Android (drag handle, scrim, `elevated` surface, 56 dp rows with a leading icon), and the
 * same sheet for the iOS long-press menu of a message, with 44 pt rows. Lock-aware: it closes
 * under the lock cover. `header` and `children` hold anything that is not a plain row
 * (a rating row, a comment field, a note).
 */
export function ActionMenu({
  visible,
  title,
  items,
  onClose,
  header,
  children,
}: {
  visible: boolean;
  title: string;
  items: readonly ActionMenuItem[];
  onClose: () => void;
  header?: ReactNode;
  children?: ReactNode;
}) {
  const animation = useModalAnimation('slide');
  return (
    <LockAwareModal visible={visible} transparent animationType={animation} onRequestClose={onClose}>
      {visible ? (
        <MenuBody title={title} items={items} onClose={onClose} header={header}>
          {children}
        </MenuBody>
      ) : null}
    </LockAwareModal>
  );
}

function MenuBody({ title, items, onClose, header, children }: { title: string; items: readonly ActionMenuItem[]; onClose: () => void; header?: ReactNode; children?: ReactNode }) {
  const design = useDesign();
  const { colors, radius, space } = design;
  const insets = useSafeAreaInsets();
  const rowHeight = uiPlatform === 'ios' ? 50 : 56;
  return (
    <View style={[styles.backdrop, { backgroundColor: colors.scrim }]}>
      <Pressable accessibilityLabel={`Close ${title.toLowerCase()}`} accessibilityRole="button" style={styles.flex} onPress={onClose} />
      {/* VoiceOver keeps to the sheet (the scrim behind it is out of reach), so the sheet
          closes from inside: the two-finger scrub and, on iOS, a Cancel row. Android closes
          with the back gesture and TalkBack still reaches the scrim. */}
      <View
        accessibilityViewIsModal
        onAccessibilityEscape={onClose}
        style={[
          styles.sheet,
          design.elevation.e3,
          {
            backgroundColor: colors.elevated,
            borderTopLeftRadius: radius.sheetTop,
            borderTopRightRadius: radius.sheetTop,
            paddingBottom: Math.max(insets.bottom, space[4]),
          },
        ]}>
        <View style={[styles.handle, { backgroundColor: colors.separatorStrong }]} />
        <Text variant="footnote" tone="secondary" heading style={[styles.title, { paddingHorizontal: space.screenGutter }]}>
          {title}
        </Text>
        <ScrollView bounces={false} keyboardShouldPersistTaps="handled" contentContainerStyle={{ paddingHorizontal: space[2], gap: space[1] }}>
          {header ? <View style={{ paddingHorizontal: space[3], gap: space[2] }}>{header}</View> : null}
          {items.map((item) => (
            <Pressable
              key={item.key}
              accessibilityRole={item.checked === undefined ? 'button' : 'radio'}
              accessibilityLabel={item.label}
              accessibilityState={item.checked === undefined ? { disabled: item.disabled === true } : { disabled: item.disabled === true, checked: item.checked }}
              disabled={item.disabled}
              onPress={item.onPress}
              android_ripple={ripple(design)}
              style={({ pressed }) => [
                styles.row,
                { minHeight: rowHeight, borderRadius: radius.md, paddingHorizontal: space[3] },
                pressed && uiPlatform === 'ios' ? { backgroundColor: pressedOverlay(design) } : null,
                item.disabled ? styles.disabled : null,
              ]}>
              <Icon name={item.icon} size={22} tone={item.destructive ? 'danger' : 'primary'} />
              <Text variant="body" tone={item.destructive ? 'danger' : 'primary'} emphasized={item.checked === true} style={styles.flex}>
                {item.label}
              </Text>
              {item.checked ? <Icon name="check" size={20} tone="accent" weight="semibold" /> : null}
            </Pressable>
          ))}
          {children ? <View style={{ paddingHorizontal: space[3], gap: space[2] }}>{children}</View> : null}
          {uiPlatform === 'ios' ? (
            <Pressable
              accessibilityRole="button"
              accessibilityLabel="Cancel"
              accessibilityHint={`Closes ${title.toLowerCase()}`}
              onPress={onClose}
              style={({ pressed }) => [
                styles.row,
                styles.cancel,
                { minHeight: rowHeight, borderRadius: radius.md, paddingHorizontal: space[3], borderTopColor: colors.separator },
                pressed ? { backgroundColor: pressedOverlay(design) } : null,
              ]}>
              <Text variant="body" emphasized tone="accent" style={[styles.flex, styles.center]}>
                Cancel
              </Text>
            </Pressable>
          ) : null}
        </ScrollView>
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  backdrop: { flex: 1, justifyContent: 'flex-end' },
  sheet: { paddingTop: 8, maxHeight: '80%', borderCurve: 'continuous' },
  handle: { alignSelf: 'center', width: 36, height: 5, borderRadius: 3, marginBottom: 8, opacity: 0.5 },
  title: { fontWeight: '600', paddingBottom: 6 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 16, borderCurve: 'continuous' },
  disabled: { opacity: 0.38 },
  cancel: { marginTop: 4, borderTopWidth: StyleSheet.hairlineWidth, borderTopLeftRadius: 0, borderTopRightRadius: 0 },
  center: { textAlign: 'center' },
});
