import { StyleSheet, View } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { ListRow } from '@/ui';

/**
 * One row of the Chats list: the conversation's title and its age. The Chats tab and its
 * `__DEV__` preview (`/design-tabs/chats`) both draw this, so the review screenshots show the
 * list the user gets (D2). When the list gains an avatar, a preview line or an unread state,
 * it is added here, with "unread" in the spoken label and an emphasized title, never by colour
 * alone.
 */
export function ConversationRow({ title, age, onPress }: { title: string | null | undefined; age: string; onPress: () => void }) {
  const { colors } = useDesign();
  return (
    <View style={{ backgroundColor: colors.surface }}>
      <ListRow title={title || 'Untitled conversation'} value={age} onPress={onPress} />
    </View>
  );
}

/** The hairline between Chats rows, inset to the row's text. */
export function ConversationSeparator() {
  const { colors } = useDesign();
  return <View style={[styles.separator, { backgroundColor: colors.separator }]} />;
}

const styles = StyleSheet.create({
  separator: { height: StyleSheet.hairlineWidth, marginStart: 16 },
});
