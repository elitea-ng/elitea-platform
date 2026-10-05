import { ActivityIndicator, Image, Modal, Pressable, StyleSheet, Text, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';
import { extensionOf } from '@/features/chat/attachments/prepare';
import type { ChatAttachment } from '@/features/chat/messageModel';

/** How many thumbnails show before "+n". */
export const VISIBLE_THUMBNAILS = 4;

/**
 * A message's files: images the model was given show their inline thumbnail
 * (no download), up to four, then "+n"; everything else is a chip. A tap
 * opens the file (downloaded on demand) when `onOpen` is given.
 */
export function AttachmentStrip({
  attachments,
  mine,
  opening,
  onOpen,
}: {
  attachments: readonly ChatAttachment[];
  mine: boolean;
  /** The key of the file being opened, for its spinner. */
  opening: string | null;
  onOpen: ((attachment: ChatAttachment) => void) | null;
}) {
  const palette = usePalette();
  if (attachments.length === 0) return null;
  const thumbs = attachments.filter((a) => a.inlineDataUrl !== null);
  const chips = attachments.filter((a) => a.inlineDataUrl === null);
  const shown = thumbs.slice(0, VISIBLE_THUMBNAILS);
  const more = thumbs.length - shown.length;
  const color = mine ? palette.onAccent : palette.text;
  return (
    <View style={styles.box}>
      {shown.length > 0 ? (
        <View style={styles.grid}>
          {shown.map((a, index) => (
            <Pressable
              key={a.key}
              accessibilityRole={onOpen ? 'button' : 'image'}
              accessibilityLabel={`Image ${a.name}`}
              disabled={!onOpen}
              onPress={() => onOpen?.(a)}>
              <Image source={{ uri: a.inlineDataUrl as string }} style={styles.thumb} accessibilityIgnoresInvertColors />
              {index === shown.length - 1 && more > 0 ? (
                <View style={[styles.thumb, styles.more]}>
                  <Text style={styles.moreText}>+{more}</Text>
                </View>
              ) : null}
              {opening === a.key ? <ActivityIndicator style={StyleSheet.absoluteFill} color="#fff" /> : null}
            </Pressable>
          ))}
        </View>
      ) : null}
      {chips.map((a) => (
        <Pressable
          key={a.key}
          accessibilityRole={onOpen ? 'button' : 'text'}
          accessibilityLabel={`File ${a.name}`}
          accessibilityHint={onOpen ? 'Opens the file' : undefined}
          disabled={!onOpen}
          onPress={() => onOpen?.(a)}
          style={[styles.chip, { borderColor: mine ? palette.onAccent : palette.border }]}>
          <Text style={[styles.ext, { color }]}>{(extensionOf(a.name).slice(1) || 'file').slice(0, 4).toUpperCase()}</Text>
          <Text numberOfLines={1} style={[styles.name, { color }]}>
            {a.name}
          </Text>
          {opening === a.key ? <ActivityIndicator size="small" color={color} /> : null}
        </Pressable>
      ))}
    </View>
  );
}

/** A full-screen view of a downloaded image; its temporary file is deleted on close. */
export function ImageViewer({ uri, name, onClose }: { uri: string | null; name: string; onClose: () => void }) {
  return (
    <Modal visible={uri !== null} animationType="fade" onRequestClose={onClose} presentationStyle="fullScreen">
      <View style={styles.viewer}>
        {uri ? <Image source={{ uri }} style={styles.full} resizeMode="contain" accessibilityLabel={`Image ${name}`} /> : null}
        <Pressable accessibilityRole="button" accessibilityLabel="Close" onPress={onClose} style={styles.close} hitSlop={12}>
          <Text style={styles.closeText}>Done</Text>
        </Pressable>
      </View>
    </Modal>
  );
}

const styles = StyleSheet.create({
  box: { gap: Spacing.one, marginTop: 2 },
  grid: { flexDirection: 'row', flexWrap: 'wrap', gap: 4 },
  thumb: { width: 72, height: 72, borderRadius: 8 },
  more: { position: 'absolute', top: 0, left: 0, backgroundColor: 'rgba(0,0,0,0.5)', alignItems: 'center', justifyContent: 'center' },
  moreText: { color: '#fff', fontSize: 18, fontWeight: '700' },
  chip: { flexDirection: 'row', alignItems: 'center', gap: Spacing.one, borderWidth: StyleSheet.hairlineWidth, borderRadius: 8, paddingHorizontal: Spacing.two, paddingVertical: 4, maxWidth: 260 },
  ext: { fontSize: 10, fontWeight: '700' },
  name: { fontSize: 13, flexShrink: 1 },
  viewer: { flex: 1, backgroundColor: '#000', justifyContent: 'center' },
  full: { flex: 1 },
  close: { position: 'absolute', top: 56, right: 20 },
  closeText: { color: '#fff', fontSize: 17, fontWeight: '600' },
});
