import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { AccessibilityInfo, StyleSheet, View } from 'react-native';
import { KeyboardAvoidingView } from 'react-native-keyboard-controller';

import { useDesign } from '@/design/DesignProvider';
import { GlassSurface, IconButton, Text } from '@/ui';

/**
 * The conversation's frame (docs/design-system.md §4.5, screens.md §4): the transcript on the
 * flat background, the floating glass composer over its bottom edge (the transcript scrolls
 * under the glass), notes and the "jump to latest" button floating just above the composer.
 *
 * The keyboard: keyboard-controller's `KeyboardAvoidingView` with `automaticOffset` (it measures
 * its own place under the navigation bar, no hard-coded header offset) shrinks the frame, and
 * the composer rides the keyboard / IME insets on both platforms (edge-to-edge on Android). The
 * composer measures itself; `renderTranscript` gets that height to keep the newest message
 * clear of it.
 */
export function ThreadLayout({
  renderTranscript,
  composer,
  floating,
  jumpToLatest,
}: {
  renderTranscript: (composerHeight: number) => ReactNode;
  composer: ReactNode;
  /** Above the composer: notes, the reading-aloud pill, a toast. */
  floating?: ReactNode;
  /** Shown while the transcript is scrolled away from the newest message. */
  jumpToLatest?: (() => void) | null;
}) {
  const design = useDesign();
  const [height, setHeight] = useState(96);
  return (
    <KeyboardAvoidingView style={[styles.flex, { backgroundColor: design.colors.background }]} behavior="padding" automaticOffset>
      <View style={styles.flex}>
        {renderTranscript(height)}
        <View pointerEvents="box-none" style={styles.dock} onLayout={(event) => setHeight(Math.round(event.nativeEvent.layout.height))}>
          <View pointerEvents="box-none" style={[styles.floating, { paddingHorizontal: design.space.composerInset }]}>
            {floating}
            {jumpToLatest ? (
              <IconButton
                icon="arrowDown"
                variant="glass"
                accessibilityLabel="Jump to the latest message"
                onPress={jumpToLatest}
                style={[styles.jump, design.elevation.e2]}
              />
            ) : null}
          </View>
          {composer}
        </View>
      </View>
    </KeyboardAvoidingView>
  );
}

/** A toast to show: every `show` is a new one (a new `id`), even with the same text. */
export interface ToastNote {
  text: string;
  id: number;
}

/** The thread's toast: `show` (re)starts it, so a second "Copied" is announced and timed again. */
export function useToast(): { toast: ToastNote | null; show: (text: string) => void; hide: () => void } {
  const [toast, setToast] = useState<ToastNote | null>(null);
  const next = useRef(0);
  const show = useCallback((text: string) => setToast({ text, id: ++next.current }), []);
  const hide = useCallback(() => setToast(null), []);
  return { toast, show, hide };
}

/**
 * A short confirmation above the composer ("Copied"), §4.13: a glass capsule on iOS, the same
 * capsule as an M3 snackbar stand-in on Android. Announced to screen readers once (an explicit
 * announcement only: a live region as well made TalkBack read it twice); gone after a moment;
 * never for a decision (those are alerts or inline rows).
 */
export function Toast({ toast, onHide }: { toast: ToastNote | null; onHide: () => void }) {
  const design = useDesign();
  const hide = useRef(onHide);
  useEffect(() => {
    hide.current = onHide;
  });
  const text = toast?.text ?? null;
  const id = toast?.id ?? null;
  useEffect(() => {
    if (text === null) return;
    AccessibilityInfo.announceForAccessibility(text);
    const timer = setTimeout(() => hide.current(), 1800);
    return () => clearTimeout(timer);
    // `id`: the same text shown again is a new toast (announced, timer restarted).
  }, [text, id]);
  if (text === null) return null;
  return (
    <GlassSurface radius={design.radius.pill} floating style={styles.toast} testID="thread-toast">
      <Text variant="subheadline" emphasized>
        {text}
      </Text>
    </GlassSurface>
  );
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  dock: { position: 'absolute', start: 0, end: 0, bottom: 0 },
  floating: { alignItems: 'center', gap: 8 },
  jump: { alignSelf: 'flex-end' },
  toast: { paddingHorizontal: 16, paddingVertical: 10, alignSelf: 'center' },
});
