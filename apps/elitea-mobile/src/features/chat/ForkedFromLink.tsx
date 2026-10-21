import { Pressable, StyleSheet, View } from 'react-native';

import { Icon, Text } from '@/ui';

/**
 * The "Forked from" back-link's data (client contract 1.11): getConversation names the source
 * of a fork in `forked_from`, and states its `name` only while the reader can still see the
 * source. Without a name there is nothing the reader could open, so there is no link.
 */
export interface ForkedFrom {
  conversationId: number;
  name: string;
}

export function forkedFromOf(detail: unknown): ForkedFrom | null {
  const value = (detail as { forked_from?: unknown } | null | undefined)?.forked_from;
  if (typeof value !== 'object' || value === null) return null;
  const { conversation_id: id, name } = value as { conversation_id?: unknown; name?: unknown };
  const numeric = typeof id === 'number' ? id : typeof id === 'string' && /^[1-9]\d{0,15}$/.test(id) ? Number(id) : NaN;
  if (!Number.isSafeInteger(numeric) || numeric <= 0 || typeof name !== 'string') return null;
  return { conversationId: numeric, name };
}

export const UNTITLED_SOURCE = 'an untitled chat';

/** One line above a fork's transcript: "Forked from <name>"; a tap opens the source. */
export function ForkedFromLink({ forkedFrom, onOpen }: { forkedFrom: ForkedFrom | null; onOpen: (conversationId: number) => void }) {
  if (forkedFrom === null) return null;
  const name = forkedFrom.name.trim() === '' ? UNTITLED_SOURCE : forkedFrom.name;
  return (
    <View style={styles.wrap}>
      <Pressable
        testID="chat-forked-from"
        accessibilityRole="link"
        accessibilityLabel={`Forked from ${name}`}
        accessibilityHint="Opens the original chat"
        hitSlop={8}
        onPress={() => onOpen(forkedFrom.conversationId)}
        style={styles.row}>
        <Icon name="fork" size={14} tone="secondary" />
        <Text variant="footnote" tone="secondary" numberOfLines={1} style={styles.text}>
          Forked from{' '}
          <Text variant="footnote" tone="accent" emphasized>
            {name}
          </Text>
        </Text>
      </Pressable>
    </View>
  );
}

const styles = StyleSheet.create({
  wrap: { paddingHorizontal: 16, paddingTop: 8, paddingBottom: 4 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 6, minHeight: 32 },
  text: { flexShrink: 1 },
});
