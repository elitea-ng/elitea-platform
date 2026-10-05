import { useState } from 'react';
import { Pressable, StyleSheet, Text, TextInput, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';

export interface ComposerProps {
  /** Why sending is impossible; renders instead of the input. */
  disabledReason?: string | null;
  busy: boolean;
  /** Returns (or resolves to) an error text when the message was not accepted. */
  onSend: (text: string) => string | null | Promise<string | null>;
}

export function Composer({ disabledReason, busy, onSend }: ComposerProps) {
  const palette = usePalette();
  const [text, setText] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  if (disabledReason) {
    return (
      <View style={[styles.bar, { borderTopColor: palette.border, backgroundColor: palette.background }]}>
        <Text style={[styles.notice, { color: palette.textSecondary }]}>{disabledReason}</Text>
      </View>
    );
  }

  const canSend = !busy && !submitting && text.trim() !== '';
  const settle = (sent: string, problem: string | null) => {
    setError(problem);
    // Only the accepted text is cleared; an edit made meanwhile is kept.
    if (problem === null) setText((current) => (current === sent ? '' : current));
  };
  const submit = () => {
    if (!canSend) return;
    const sent = text;
    const result = onSend(sent);
    if (!(result instanceof Promise)) {
      settle(sent, result);
      return;
    }
    setSubmitting(true);
    result
      .then((problem) => settle(sent, problem))
      .catch(() => settle(sent, 'This message could not be sent.'))
      .finally(() => setSubmitting(false));
  };

  return (
    <View style={[styles.bar, { borderTopColor: palette.border, backgroundColor: palette.background }]}>
      {error ? (
        <Text accessibilityRole="alert" style={[styles.error, { color: palette.danger }]}>
          {error}
        </Text>
      ) : null}
      <View style={styles.row}>
        <TextInput
          value={text}
          onChangeText={(value) => {
            setText(value);
            setError(null);
          }}
          placeholder="Message"
          placeholderTextColor={palette.textSecondary}
          multiline
          accessibilityLabel="Message"
          style={[
            styles.input,
            { color: palette.text, backgroundColor: palette.backgroundElement, borderColor: palette.border },
          ]}
        />
        <Pressable
          accessibilityRole="button"
          accessibilityLabel="Send"
          accessibilityState={{ disabled: !canSend, busy }}
          disabled={!canSend}
          onPress={submit}
          style={[styles.send, { backgroundColor: palette.accent, opacity: canSend ? 1 : 0.4 }]}>
          <Text style={[styles.sendText, { color: palette.onAccent }]}>Send</Text>
        </Pressable>
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  bar: { borderTopWidth: StyleSheet.hairlineWidth, padding: Spacing.two, gap: Spacing.one },
  row: { flexDirection: 'row', alignItems: 'flex-end', gap: Spacing.two },
  input: {
    flex: 1,
    minHeight: 40,
    maxHeight: 140,
    borderWidth: StyleSheet.hairlineWidth,
    borderRadius: 18,
    paddingHorizontal: Spacing.three - 4,
    paddingTop: Spacing.two,
    paddingBottom: Spacing.two,
    fontSize: 16,
  },
  send: { borderRadius: 18, paddingHorizontal: Spacing.three, height: 40, justifyContent: 'center' },
  sendText: { fontSize: 15, fontWeight: '600' },
  notice: { fontSize: 14, textAlign: 'center', padding: Spacing.two },
  error: { fontSize: 13 },
});
