import { ActivityIndicator, ActionSheetIOS, Alert, Image, Platform, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { FONT_SCALE } from '@/constants/typeScale';
import { usePalette } from '@/constants/use-palette';
import { extensionOf, formatBytes } from '@/features/chat/attachments/prepare';
import type { TrayItem } from '@/features/chat/attachments/tray';

/** The chips of the files a message will carry: thumbnail or type, name, size, state, remove. */
export function AttachmentTrayView({
  items,
  onRemove,
  onRetry,
}: {
  items: readonly TrayItem[];
  onRemove: (id: string) => void;
  onRetry: (id: string) => void;
}) {
  const palette = usePalette();
  if (items.length === 0) return null;
  return (
    <ScrollView horizontal keyboardShouldPersistTaps="handled" contentContainerStyle={styles.tray} accessibilityLabel="Attached files">
      {items.map((item) => {
        const failed = item.state === 'failed';
        const stateWord =
          item.state === 'preparing'
            ? 'preparing'
            : item.state === 'uploading'
              ? item.progress === null
                ? 'uploading'
                : `uploading, ${Math.round(item.progress * 100)}%`
              : item.state === 'uploaded'
                ? 'uploaded'
                : failed
                  ? 'failed'
                  : 'ready';
        return (
          <View
            key={item.id}
            accessible
            accessibilityLabel={`${item.name}, ${stateWord}${item.error ? `. ${item.error}` : ''}`}
            style={[styles.chip, { borderColor: failed ? palette.danger : palette.border, backgroundColor: palette.backgroundElement }]}>
            {item.previewUri ? (
              <Image source={{ uri: item.previewUri }} style={styles.thumb} accessibilityIgnoresInvertColors />
            ) : (
              <View style={[styles.thumb, styles.icon, { backgroundColor: palette.border }]}>
                <Text maxFontSizeMultiplier={FONT_SCALE.control} style={[styles.iconText, { color: palette.text }]}>{(extensionOf(item.name).slice(1) || 'file').slice(0, 4).toUpperCase()}</Text>
              </View>
            )}
            <View style={styles.texts}>
              <Text maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1} style={[styles.name, { color: palette.text }]}>
                {item.name}
              </Text>
              <Text maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1} style={[styles.meta, { color: failed ? palette.danger : palette.textSecondary }]}>
                {failed ? (item.error ?? 'Failed') : item.state === 'uploading' && item.progress !== null ? `${Math.round(item.progress * 100)}%` : item.size !== null ? formatBytes(item.size) : ''}
              </Text>
            </View>
            {item.state === 'preparing' || item.state === 'uploading' ? <ActivityIndicator size="small" color={palette.accent} /> : null}
            {failed && item.ready ? (
              <Pressable accessibilityRole="button" accessibilityLabel={`Retry ${item.name}`} hitSlop={8} onPress={() => onRetry(item.id)}>
                <Text maxFontSizeMultiplier={FONT_SCALE.control} style={[styles.action, { color: palette.accent }]}>↻</Text>
              </Pressable>
            ) : null}
            {item.state !== 'uploading' ? (
              <Pressable accessibilityRole="button" accessibilityLabel={`Remove ${item.name}`} hitSlop={8} onPress={() => onRemove(item.id)}>
                <Text maxFontSizeMultiplier={FONT_SCALE.control} style={[styles.action, { color: palette.textSecondary }]}>✕</Text>
              </Pressable>
            ) : null}
          </View>
        );
      })}
    </ScrollView>
  );
}

export type PickSource = 'photos' | 'camera' | 'files';

/** The paperclip: an action sheet of Photo library, Camera and Files; or why files cannot be attached. */
export function AttachButton({
  disabledReason,
  onPick,
}: {
  disabledReason: string | null;
  onPick: (source: PickSource) => void;
}) {
  const palette = usePalette();
  const open = () => {
    if (disabledReason) {
      Alert.alert('Attach files', disabledReason);
      return;
    }
    const labels = ['Photo library', 'Camera', 'Files', 'Cancel'];
    const sources: PickSource[] = ['photos', 'camera', 'files'];
    if (Platform.OS === 'ios') {
      ActionSheetIOS.showActionSheetWithOptions({ options: labels, cancelButtonIndex: 3 }, (index) => {
        if (index >= 0 && index < 3) onPick(sources[index]);
      });
      return;
    }
    Alert.alert('Attach', undefined, [
      { text: 'Photo library', onPress: () => onPick('photos') },
      { text: 'Camera', onPress: () => onPick('camera') },
      { text: 'Files', onPress: () => onPick('files') },
      { text: 'Cancel', style: 'cancel' },
    ]);
  };
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel="Attach files"
      accessibilityHint={disabledReason ?? undefined}
      accessibilityState={{ disabled: disabledReason !== null }}
      onPress={open}
      style={[styles.clip, { borderColor: palette.border, opacity: disabledReason ? 0.4 : 1 }]}>
      <Text maxFontSizeMultiplier={FONT_SCALE.control} style={[styles.clipText, { color: palette.accent }]}>+</Text>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  tray: { gap: Spacing.one, paddingVertical: 2 },
  chip: { flexDirection: 'row', alignItems: 'center', gap: Spacing.one, borderWidth: StyleSheet.hairlineWidth, borderRadius: 10, padding: 4, maxWidth: 240 },
  thumb: { width: 36, height: 36, borderRadius: 6 },
  icon: { alignItems: 'center', justifyContent: 'center' },
  iconText: { fontSize: 10, fontWeight: '700' },
  texts: { flexShrink: 1, maxWidth: 140 },
  name: { fontSize: 13 },
  meta: { fontSize: 11 },
  action: { fontSize: 16, paddingHorizontal: 2 },
  clip: { width: 40, height: 40, borderRadius: 20, borderWidth: StyleSheet.hairlineWidth, alignItems: 'center', justifyContent: 'center' },
  clipText: { fontSize: 24, lineHeight: 26, fontWeight: '400' },
});
