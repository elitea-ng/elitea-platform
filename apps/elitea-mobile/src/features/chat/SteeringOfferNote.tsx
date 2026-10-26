import { useState } from 'react';
import { StyleSheet, View } from 'react-native';

import { Button, GlassSurface, Icon, Text } from '@/ui';
import { useDesign } from '@/design/DesignProvider';

/**
 * The note above the composer when a stopped or failed turn did not read
 * some steering messages of this device (client contract 1.27). The server
 * does not send them, and the app does not send them by itself: the user
 * sends them as the next message, or dismisses them (they then stay on the
 * server).
 */
export function SteeringOfferNote({
  text,
  onSend,
  onDismiss,
}: {
  text: string;
  /** Resolves to an error text, or null. */
  onSend: () => Promise<string | null>;
  onDismiss: () => void;
}) {
  const design = useDesign();
  const [sending, setSending] = useState(false);
  const send = async () => {
    setSending(true);
    try {
      await onSend();
    } finally {
      setSending(false);
    }
  };
  return (
    <GlassSurface radius={design.radius.md} floating style={styles.note} testID="thread-steering-offer">
      <View style={styles.row}>
        <Icon name="info" size={18} tone="secondary" />
        <Text variant="footnote" tone="secondary" style={styles.flex}>
          The answer ended before it read these messages. Send them as the next message?
        </Text>
      </View>
      <Text variant="body" numberOfLines={6} testID="thread-steering-offer-text">
        {text}
      </Text>
      <View style={styles.actions}>
        <Button title="Dismiss" variant="plain" size="small" disabled={sending} onPress={onDismiss} />
        <Button title="Send" variant="tonal" size="small" loading={sending} onPress={() => void send()} />
      </View>
    </GlassSurface>
  );
}

const styles = StyleSheet.create({
  note: { alignSelf: 'stretch', gap: 8, paddingHorizontal: 12, paddingVertical: 10 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  flex: { flex: 1 },
  actions: { flexDirection: 'row', justifyContent: 'flex-end', gap: 8 },
});
