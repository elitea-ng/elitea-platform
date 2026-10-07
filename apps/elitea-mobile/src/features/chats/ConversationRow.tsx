import { View } from 'react-native';

import type { SwipeRowA11y } from '@/components/SwipeToDelete';
import { useDesign } from '@/design/DesignProvider';
import { ListRow } from '@/ui';

/**
 * One row of the Chats list: the conversation's title and its age. The Chats tab and its
 * `__DEV__` preview (`/design-tabs/chats`) both draw this (through `ConversationList`), so the
 * review screenshots show the list the user gets (D2). When the list gains an avatar, a preview
 * line or an unread state, it is added here, with "unread" in the spoken label and an
 * emphasized title, never by colour alone. `a11y` carries the swipe's Delete action so
 * VoiceOver/TalkBack can reach it (§4.11), and its busy state while the delete runs.
 */
export function ConversationRow({
  title,
  age,
  onPress,
  a11y,
}: {
  title: string | null | undefined;
  age: string;
  onPress: () => void;
  a11y?: SwipeRowA11y;
}) {
  const { colors } = useDesign();
  return (
    <View style={{ backgroundColor: colors.surface }}>
      <ListRow
        title={title || 'Untitled conversation'}
        value={age}
        onPress={onPress}
        accessibilityActions={a11y?.accessibilityActions}
        onAccessibilityAction={a11y?.onAccessibilityAction}
        busy={a11y?.accessibilityState?.busy === true}
      />
    </View>
  );
}
