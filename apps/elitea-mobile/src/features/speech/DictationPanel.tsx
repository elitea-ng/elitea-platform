import type { ComponentRef, Ref } from 'react';
import { StyleSheet, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { VoiceOrb } from '@/features/speech/VoiceOrb';
import { GlassSurface, GradientBackground, IconButton, Text } from '@/ui';

/**
 * The listening state of dictation, as the reference's voice screen (docs/design-system.md §4.8,
 * screens.md §5) floated over the transcript, right above the composer: a pastel glass card with
 * a red-dot "Listening" pill, the orb, the words heard so far, and the three controls of the
 * reference: keyboard (stop and keep typing), the large microphone (stop) and close (cancel:
 * the text typed before dictation comes back). The composer's input shows the same words, so
 * nothing is lost if the card is dismissed.
 *
 * The orb is decorative; the state is said in text ("Listening") and announced politely.
 */
export function DictationPanel({
  heard,
  level,
  onStop,
  onKeyboard,
  onCancel,
  stopRef,
}: {
  /** The words heard since dictation started. */
  heard: string;
  /** Microphone level 0..1. */
  level: number;
  onStop: () => void;
  onKeyboard: () => void;
  onCancel: () => void;
  /** The Stop button, for the composer to move screen-reader focus to when listening starts. */
  stopRef?: Ref<ComponentRef<typeof View>>;
}) {
  const design = useDesign();
  const { colors, space } = design;
  const compact = design.accessibilityLayout;
  return (
    <GlassSurface radius={design.radius.actionCard} floating style={styles.card} testID="dictation-panel">
      <View style={[StyleSheet.absoluteFill, styles.wash]} pointerEvents="none">
        <GradientBackground />
      </View>
      <View style={[styles.body, { padding: space.cardPadding, gap: space[3] }]}>
        <View style={[styles.pill, { backgroundColor: colors.surface, borderColor: colors.separator }]} accessibilityLiveRegion="polite">
          <View style={[styles.dot, { backgroundColor: colors.danger }]} />
          <Text variant="subheadline" emphasized accessibilityRole="header">
            Listening
          </Text>
        </View>
        <VoiceOrb size={compact ? 96 : design.size.orb.compact} active level={level} />
        <Text variant="body" tone={heard === '' ? 'secondary' : 'primary'} numberOfLines={4} style={styles.heard}>
          {heard === '' ? 'Speak now. Your words stay on this device.' : heard}
        </Text>
        <View style={styles.controls}>
          <IconButton icon="keyboard" accessibilityLabel="Switch to keyboard" onPress={onKeyboard} haptic="voiceStop" />
          <IconButton
            ref={stopRef}
            icon="micFill"
            variant="filled"
            size={64}
            accessibilityLabel="Stop dictation"
            accessibilityHint="Keeps the words in the message"
            selected
            onPress={onStop}
            haptic="voiceStop"
          />
          <IconButton icon="close" accessibilityLabel="Cancel dictation" accessibilityHint="Removes the dictated words" onPress={onCancel} />
        </View>
      </View>
    </GlassSurface>
  );
}

const styles = StyleSheet.create({
  card: { overflow: 'hidden' },
  wash: { opacity: 0.85 },
  body: { alignItems: 'center' },
  pill: { flexDirection: 'row', alignItems: 'center', gap: 6, borderRadius: 999, borderWidth: StyleSheet.hairlineWidth, paddingHorizontal: 12, minHeight: 32 },
  dot: { width: 8, height: 8, borderRadius: 4 },
  heard: { textAlign: 'center', alignSelf: 'stretch' },
  controls: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-evenly', alignSelf: 'stretch' },
});
