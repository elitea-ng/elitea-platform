import { useRef, useState, type ComponentRef } from 'react';
import { ActionSheetIOS, ActivityIndicator, Alert, findNodeHandle, Image, Platform, Pressable, ScrollView, StyleSheet, View } from 'react-native';

import { FONT_SCALE } from '@/constants/typeScale';
import { useDesign } from '@/design/DesignProvider';
import { ActionMenu } from '@/features/chat/ActionMenu';
import { extensionOf, formatBytes } from '@/features/chat/attachments/prepare';
import type { TrayItem } from '@/features/chat/attachments/tray';
import { hitSlopFor, Icon, IconButton, Text } from '@/ui';

/** The thumbnail of a file in the tray (§4.5: 56 pt). */
const THUMB = 56;

/**
 * The files a message will carry, inside the composer above the input (docs/design-system.md
 * §4.5): 56 pt rounded thumbnails for images, a document tile with the extension for other
 * files, the name and size (or upload progress) beside it. Remove is an `xmark.circle.fill`
 * on the corner (24 pt glyph, 44/48 target); on a file uploading it cancels the upload. A
 * failed file is retried by tapping it.
 */
export function AttachmentTrayView({
  items,
  onRemove,
  onRetry,
  pending = null,
}: {
  items: readonly TrayItem[];
  onRemove: (id: string) => void;
  onRetry: (id: string) => void;
  /**
   * A file still being made before it can join the tray (a document scan's PDF or text): a
   * placeholder chip with a spinner, so the wait is seen and the message is not sent without it.
   */
  pending?: { name: string; meta: string } | null;
}) {
  const design = useDesign();
  const { colors, radius } = design;
  if (items.length === 0 && pending === null) return null;
  return (
    <ScrollView horizontal keyboardShouldPersistTaps="handled" showsHorizontalScrollIndicator={false} contentContainerStyle={styles.tray} accessibilityLabel="Attached files">
      {pending ? (
        <View style={styles.slot}>
          <View
            accessible
            accessibilityLabel={`${pending.name}, preparing`}
            style={[styles.chip, { backgroundColor: colors.surface, borderColor: colors.separator, borderRadius: radius.md }]}>
            <View style={[styles.thumb, { borderRadius: radius.sm, backgroundColor: colors.surfaceSunken }]}>
              <ActivityIndicator size="small" color={colors.textSecondary} />
            </View>
            <View style={styles.texts}>
              <Text variant="footnote" maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1}>
                {pending.name}
              </Text>
              <Text variant="caption" tone="secondary" maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1}>
                {pending.meta}
              </Text>
            </View>
          </View>
        </View>
      ) : null}
      {items.map((item) => {
        const failed = item.state === 'failed';
        const busy = item.state === 'preparing' || item.state === 'uploading';
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
        const status = failed
          ? (item.error ?? 'Failed')
          : item.state === 'uploading' && item.progress !== null
            ? `${Math.round(item.progress * 100)}%`
            : item.size !== null
              ? formatBytes(item.size)
              : '';
        // A scan's date sits here, where nothing truncates it (its full name would lose it).
        const meta = item.display && !failed ? [item.display.detail, status].filter((t) => t !== '').join(' · ') : status;
        const retry = failed && item.ready !== null;
        return (
          <View key={item.id} style={styles.slot}>
            <Pressable
              accessible
              accessibilityRole={retry ? 'button' : undefined}
              accessibilityLabel={`${item.name}, ${stateWord}${item.error ? `. ${item.error}` : ''}`}
              accessibilityHint={retry ? 'Tap to try the upload again' : undefined}
              disabled={!retry}
              onPress={() => onRetry(item.id)}
              style={[
                styles.chip,
                {
                  backgroundColor: colors.surface,
                  borderColor: failed ? colors.danger : colors.separator,
                  borderRadius: radius.md,
                },
              ]}>
              <View style={[styles.thumb, { borderRadius: radius.sm, backgroundColor: colors.surfaceSunken }]}>
                {item.previewUri ? (
                  <>
                    <Image source={{ uri: item.previewUri }} style={[styles.thumb, { borderRadius: radius.sm }]} accessibilityIgnoresInvertColors />
                    {/* A photo carries no text, so a veil can sit over it. */}
                    {busy || retry ? (
                      <View style={[StyleSheet.absoluteFill, styles.veil, { borderRadius: radius.sm, backgroundColor: colors.scrim }]}>
                        {busy ? <ActivityIndicator size="small" color="#FFFFFF" /> : <Icon name="retry" size={20} color="#FFFFFF" scales={false} />}
                      </View>
                    ) : null}
                  </>
                ) : (
                  <>
                    {/* A document's type label stays unveiled (4.5:1): the spinner or the retry
                        glyph takes the document glyph's place above it. */}
                    {busy ? (
                      <ActivityIndicator testID="tray-busy" size="small" color={colors.textSecondary} style={styles.glyph} />
                    ) : retry ? (
                      <Icon name="retry" size={22} tone="danger" scales={false} />
                    ) : (
                      <Icon name="document" size={22} tone="secondary" scales={false} />
                    )}
                    <Text variant="label" tone="secondary" maxFontSizeMultiplier={1}>
                      {(extensionOf(item.name).slice(1) || 'file').slice(0, 4).toUpperCase()}
                    </Text>
                  </>
                )}
              </View>
              <View style={styles.texts}>
                {/* Middle truncation keeps the extension, the part that tells similar files apart. */}
                <Text variant="footnote" maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1} ellipsizeMode="middle">
                  {item.display?.title ?? item.name}
                </Text>
                <Text variant="caption" tone={failed ? 'danger' : 'secondary'} maxFontSizeMultiplier={FONT_SCALE.control} numberOfLines={1}>
                  {meta}
                </Text>
              </View>
            </Pressable>
            {/* Always there: while a file uploads it cancels that upload (HIG/M3: a long
                operation the user started can be cancelled). */}
            <Pressable
              accessibilityRole="button"
              accessibilityLabel={item.state === 'uploading' ? `Cancel upload of ${item.name}` : `Remove ${item.name}`}
              hitSlop={hitSlopFor(24)}
              onPress={() => onRemove(item.id)}
              style={[styles.remove, { backgroundColor: colors.surface, borderRadius: 12 }]}>
              <Icon name="closeCircle" size={22} tone="secondary" scales={false} />
            </Pressable>
          </View>
        );
      })}
    </ScrollView>
  );
}

export type PickSource = 'photos' | 'camera' | 'scan' | 'files';

type SourceRow = { source: PickSource; label: string; icon: 'photoLibrary' | 'camera' | 'scanDocument' | 'document' };

/**
 * The attach menu's rows. "Scan document" only where a scan can run (`scan`: VisionKit's
 * document camera, or the development sample pages on the simulator, said in its label).
 */
export function attachSources(scan: 'camera' | 'samples' | null = null): SourceRow[] {
  return [
    { source: 'photos', label: 'Photo library', icon: 'photoLibrary' },
    { source: 'camera', label: 'Camera', icon: 'camera' },
    ...(scan === null ? [] : [{ source: 'scan' as const, label: scan === 'camera' ? 'Scan document' : 'Scan document (sample pages)', icon: 'scanDocument' as const }]),
    { source: 'files', label: 'Files', icon: 'document' },
  ];
}

/**
 * The composer's attach button (§4.5): a paperclip in the 44/48 target. iOS shows the system
 * action sheet (anchored to the button, so iPad and iOS 26 present it as a popover from it);
 * Android an M3 bottom sheet with icon rows. When files cannot be attached here, it says why
 * (`onBlocked`: a toast, not an alert) and the reason is its accessibility hint.
 */
export function AttachButton({
  disabledReason,
  onPick,
  onBlocked,
  scan = null,
}: {
  disabledReason: string | null;
  onPick: (source: PickSource) => void;
  /** Says why files can't be attached (default: an alert). */
  onBlocked?: (reason: string) => void;
  /** Whether "Scan document" is offered (useDocumentScan's `source`). */
  scan?: 'camera' | 'samples' | null;
}) {
  const sources = attachSources(scan);
  const anchor = useRef<ComponentRef<typeof View>>(null);
  const [menuOpen, setMenuOpen] = useState(false);
  const open = () => {
    if (disabledReason) {
      if (onBlocked) onBlocked(disabledReason);
      else Alert.alert('Attach files', disabledReason);
      return;
    }
    if (Platform.OS === 'ios') {
      const node = anchor.current ? findNodeHandle(anchor.current) : null;
      ActionSheetIOS.showActionSheetWithOptions(
        { options: [...sources.map((s) => s.label), 'Cancel'], cancelButtonIndex: sources.length, ...(node ? { anchor: node } : null) },
        (index) => {
          if (index >= 0 && index < sources.length) onPick(sources[index].source);
        },
      );
      return;
    }
    setMenuOpen(true);
  };
  return (
    <View ref={anchor} collapsable={false}>
      <IconButton
        icon="attach"
        variant="plain"
        size={36}
        accessibilityLabel="Attach files"
        accessibilityHint={disabledReason ?? (scan ? 'Photo library, camera, document scan or files' : 'Photo library, camera or files')}
        accessibilityState={{ disabled: disabledReason !== null }}
        style={disabledReason ? styles.blocked : undefined}
        onPress={open}
      />
      <ActionMenu
        visible={menuOpen}
        title="Attach"
        onClose={() => setMenuOpen(false)}
        items={sources.map((s) => ({
          key: s.source,
          label: s.label,
          icon: s.icon,
          onPress: () => {
            setMenuOpen(false);
            onPick(s.source);
          },
        }))}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  tray: { gap: 10, paddingTop: 8, paddingEnd: 8 },
  slot: { paddingTop: 6, paddingEnd: 6 },
  chip: { flexDirection: 'row', alignItems: 'center', gap: 8, borderWidth: StyleSheet.hairlineWidth, padding: 4, paddingEnd: 12, maxWidth: 240, borderCurve: 'continuous' },
  thumb: { width: THUMB, height: THUMB, alignItems: 'center', justifyContent: 'center', overflow: 'hidden' },
  veil: { alignItems: 'center', justifyContent: 'center', opacity: 0.85 },
  glyph: { height: 22 },
  texts: { flexShrink: 1, maxWidth: 140 },
  remove: { position: 'absolute', top: 0, end: 0, width: 24, height: 24, alignItems: 'center', justifyContent: 'center' },
  blocked: { opacity: 0.38 },
});
