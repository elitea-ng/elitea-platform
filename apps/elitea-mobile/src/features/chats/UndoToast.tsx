import { useEffect } from 'react';
import { AccessibilityInfo, StyleSheet, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { useDesign } from '@/design/DesignProvider';
import { Button, GlassSurface, Text } from '@/ui';

/**
 * "Chat deleted · Undo" (design-system §4.11, the snackbar after a removal): floats above the
 * tab bar, is announced, and its Undo is a real button (VoiceOver/TalkBack can reach it; the
 * screen keeps it longer while a screen reader runs).
 */
export function UndoToast({
  text,
  subject,
  subjectName,
  onUndo,
  testID = 'undo-toast',
}: {
  text: string | null;
  /**
   * What Undo restores now (the pending row). A new subject is announced even when `text` is
   * the same: a second delete while the toast is up commits the first and moves Undo to the
   * second, and a screen-reader user must hear that.
   */
  subject?: unknown;
  /** The subject's name, said in the announcement. */
  subjectName?: string | null;
  onUndo: () => void;
  testID?: string;
}) {
  const design = useDesign();
  const insets = useSafeAreaInsets();
  useEffect(() => {
    if (text === null) return;
    const what = subjectName ? `${text}: “${subjectName}”` : text;
    AccessibilityInfo.announceForAccessibility(`${what}. Undo is available.`);
  }, [text, subject, subjectName]);
  if (text === null) return null;
  return (
    <View pointerEvents="box-none" style={[styles.dock, { bottom: Math.max(insets.bottom, 12) + 64 }]}>
      <GlassSurface radius={design.radius.pill} floating style={styles.toast} testID={testID}>
        <Text variant="subheadline" emphasized style={styles.text} numberOfLines={2}>
          {text}
        </Text>
        <Button title="Undo" variant="plain" size="small" onPress={onUndo} testID={`${testID}-undo`} />
      </GlassSurface>
    </View>
  );
}

const styles = StyleSheet.create({
  dock: { position: 'absolute', start: 16, end: 16, alignItems: 'center' },
  toast: { flexDirection: 'row', alignItems: 'center', gap: 12, paddingStart: 18, paddingEnd: 8, paddingVertical: 4, maxWidth: 480 },
  text: { flexShrink: 1 },
});
