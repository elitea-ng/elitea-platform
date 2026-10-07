import { ActivityIndicator, Image, Platform, Pressable, ScrollView, Share, StyleSheet, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { FONT_SCALE } from '@/constants/typeScale';
import { BUBBLE_CHIP_ALPHA, bubbleChipVeil } from '@/design/contrast';
import { useDesign } from '@/design/DesignProvider';
import { extensionOf } from '@/features/chat/attachments/prepare';
import type { ChatAttachment } from '@/features/chat/messageModel';
import { LockAwareModal } from '@/security/lock/LockAwareModal';
import { useShareOut } from '@/security/policy';
import { formatColor } from '@/theme/color';
import { Icon, IconButton, monogramStyle, Text } from '@/ui';

/** How many thumbnails show before "+n". */
export const VISIBLE_THUMBNAILS = 4;

/** A message thumbnail (§4.6). */
const THUMB = 76;

/**
 * A message's files: images the model was given show their inline thumbnail
 * (no download), up to four, then "+n"; everything else is a document chip. A tap
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
  const { colors, radius } = useDesign();
  if (attachments.length === 0) return null;
  const thumbs = attachments.filter((a) => a.inlineDataUrl !== null);
  const chips = attachments.filter((a) => a.inlineDataUrl === null);
  const shown = thumbs.slice(0, VISIBLE_THUMBNAILS);
  const more = thumbs.length - shown.length;
  const color = mine ? colors.bubbleUserText : colors.textPrimary;
  // On my own bubble a chip is a veil that only adds contrast with the bubble text (proved as
  // bubbleUserText / bubbleUserChip, docs/design-system.md §2.1), and its caption stays at full
  // opacity: a dimmed caption on a lightened chip fell under 4.5:1 in light brands.
  const chipFill = mine ? formatColor(bubbleChipVeil(colors)) : colors.surfaceSunken;
  const tileFill = mine ? formatColor(bubbleChipVeil(colors, BUBBLE_CHIP_ALPHA * 1.5)) : colors.surface;
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
              <Image source={{ uri: a.inlineDataUrl as string }} style={[styles.thumb, { borderRadius: radius.md }]} accessibilityIgnoresInvertColors />
              {index === shown.length - 1 && more > 0 ? (
                <View style={[styles.thumb, styles.more, { borderRadius: radius.md, backgroundColor: colors.scrim }]}>
                  <Text maxFontSizeMultiplier={1} color="#FFFFFF" style={[monogramStyle(THUMB, 0.3), styles.bold]}>
                    +{more}
                  </Text>
                </View>
              ) : null}
              {opening === a.key ? <ActivityIndicator style={StyleSheet.absoluteFill} color="#FFFFFF" /> : null}
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
          style={[
            styles.chip,
            { borderRadius: radius.md, borderColor: mine ? 'rgba(255,255,255,0.35)' : colors.separator, backgroundColor: chipFill },
          ]}>
          <View style={[styles.fileTile, { borderRadius: radius.sm, backgroundColor: tileFill }]}>
            <Icon name="document" size={18} color={color} scales={false} />
          </View>
          <View style={styles.flex}>
            <Text variant="footnote" color={color} numberOfLines={1} maxFontSizeMultiplier={FONT_SCALE.control}>
              {a.name}
            </Text>
            <Text variant="caption" color={mine ? color : colors.textSecondary} maxFontSizeMultiplier={FONT_SCALE.control}>
              {(extensionOf(a.name).slice(1) || 'file').slice(0, 4).toUpperCase()}
            </Text>
          </View>
          {opening === a.key ? <ActivityIndicator size="small" color={color} /> : null}
        </Pressable>
      ))}
    </View>
  );
}

/**
 * A full-screen view of a downloaded image (its temporary file is deleted on close): pinch to
 * zoom (the native scroll view's zoom), a glass close button inside the safe area, and Share
 * when the workspace allows sharing out.
 */
export function ImageViewer({ uri, name, onClose }: { uri: string | null; name: string; onClose: () => void }) {
  return (
    <LockAwareModal visible={uri !== null} animationType="fade" onRequestClose={onClose} presentationStyle="fullScreen">
      {uri ? <ViewerBody uri={uri} name={name} onClose={onClose} /> : null}
    </LockAwareModal>
  );
}

function ViewerBody({ uri, name, onClose }: { uri: string; name: string; onClose: () => void }) {
  const insets = useSafeAreaInsets();
  const shareOut = useShareOut();
  return (
    <View style={styles.viewer}>
      <ScrollView
        style={styles.flex}
        contentContainerStyle={styles.flex}
        maximumZoomScale={4}
        minimumZoomScale={1}
        centerContent
        showsHorizontalScrollIndicator={false}
        showsVerticalScrollIndicator={false}>
        <Image source={{ uri }} style={styles.full} resizeMode="contain" accessibilityLabel={`Image ${name}`} />
      </ScrollView>
      <View style={[styles.bar, { top: insets.top + 8 }]} pointerEvents="box-none">
        <IconButton icon="close" variant="glass" accessibilityLabel="Close" onPress={onClose} />
        {/* React Native's Share takes a file URL on iOS only. */}
        {shareOut && Platform.OS === 'ios' ? <IconButton icon="share" variant="glass" accessibilityLabel={`Share ${name}`} onPress={() => void Share.share({ url: uri }).catch(() => {})} /> : null}
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  box: { gap: 6, marginTop: 2 },
  grid: { flexDirection: 'row', flexWrap: 'wrap', gap: 6 },
  thumb: { width: THUMB, height: THUMB },
  more: { position: 'absolute', top: 0, left: 0, alignItems: 'center', justifyContent: 'center' },
  bold: { fontWeight: '700' },
  chip: { flexDirection: 'row', alignItems: 'center', gap: 8, borderWidth: StyleSheet.hairlineWidth, padding: 6, paddingEnd: 12, maxWidth: 280, minHeight: 48, borderCurve: 'continuous' },
  fileTile: { width: 36, height: 36, alignItems: 'center', justifyContent: 'center' },
  flex: { flex: 1 },
  viewer: { flex: 1, backgroundColor: '#000000' },
  full: { flex: 1, width: '100%' },
  bar: { position: 'absolute', start: 16, end: 16, flexDirection: 'row', justifyContent: 'space-between' },
});
