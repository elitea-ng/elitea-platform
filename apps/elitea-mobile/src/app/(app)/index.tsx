import { StyleSheet, Text, View } from 'react-native';

import { Spacing } from '@/constants/theme';
import { usePalette } from '@/constants/use-palette';

/**
 * Conversations list — placeholder.
 *
 * TODO: read from the encrypted local store (SQLCipher), incremental sync from
 * the server, open a conversation into the streaming chat screen (SSE).
 */
export default function ConversationsScreen() {
  const palette = usePalette();
  return (
    <View style={[styles.container, { backgroundColor: palette.background }]}>
      <Text style={[styles.title, { color: palette.text }]}>No conversations yet</Text>
      <Text style={[styles.body, { color: palette.textSecondary }]}>
        Your conversations with workspace agents will appear here.
      </Text>
    </View>
  );
}

const styles = StyleSheet.create({
  container: {
    flex: 1,
    alignItems: 'center',
    justifyContent: 'center',
    padding: Spacing.four,
    gap: Spacing.two,
  },
  title: { fontSize: 20, fontWeight: '600' },
  body: { fontSize: 15, textAlign: 'center' },
});
