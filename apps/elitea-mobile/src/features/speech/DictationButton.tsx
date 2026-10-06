import { Pressable, StyleSheet, Text } from 'react-native';

import { FONT_SCALE } from '@/constants/typeScale';
import { usePalette } from '@/constants/use-palette';
import { useDictation, type DictationProblem } from '@/features/speech/dictation';

/**
 * The composer's microphone: tap to dictate (on the device), tap again to
 * stop. The words go into the composer as they are heard; nothing is sent
 * until the user sends the message.
 */
export function DictationButton({
  onStart,
  onText,
  onProblem,
  disabled = false,
}: {
  /** Dictation begins: the composer remembers the text typed so far. */
  onStart: () => void;
  onText: (heard: string, final: boolean) => void;
  onProblem: (problem: DictationProblem) => void;
  disabled?: boolean;
}) {
  const palette = usePalette();
  const dictation = useDictation({ onText, onProblem });
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={dictation.listening ? 'Stop dictation' : 'Dictate'}
      accessibilityHint={dictation.listening ? undefined : 'Speech is turned into text on this device'}
      accessibilityState={{ disabled, selected: dictation.listening }}
      disabled={disabled}
      hitSlop={6}
      onPress={() => {
        if (dictation.listening) {
          dictation.stop();
          return;
        }
        onStart();
        void dictation.start();
      }}
      style={[
        styles.button,
        {
          borderColor: dictation.listening ? palette.danger : palette.border,
          backgroundColor: dictation.listening ? palette.danger : palette.backgroundElement,
          opacity: disabled ? 0.4 : 1,
        },
      ]}>
      <Text maxFontSizeMultiplier={FONT_SCALE.chrome} style={[styles.label, { color: dictation.listening ? palette.onAccent : palette.text }]}>
        {dictation.listening ? '■' : '🎙'}
      </Text>
    </Pressable>
  );
}

const styles = StyleSheet.create({
  button: { width: 40, height: 40, borderRadius: 20, borderWidth: StyleSheet.hairlineWidth, alignItems: 'center', justifyContent: 'center' },
  label: { fontSize: 17 },
});
