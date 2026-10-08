import { View } from 'react-native';

import type { SwipeRowA11y } from '@/components/SwipeToDelete';
import { useDesign } from '@/design/DesignProvider';
import type { SnippetPart } from '@/db/search';
import { Icon, ListRow, Text } from '@/ui';

export const UNTITLED = 'Untitled conversation';

/**
 * One row of the Chats list: the conversation's title and its age, a pin glyph when it is
 * pinned (design-system §4.4; "pinned" is also in the spoken label, never the glyph alone), and
 * for a search hit in a message the matched passage under the title, the matched words in bold
 * (weight, not colour alone). The Chats tab and its `__DEV__` preview (`/design-tabs/chats`)
 * both draw this (through `ConversationList`), so the review screenshots show the list the user
 * gets (D2). `a11y` carries the row's actions (Pin, Rename, Delete) so VoiceOver/TalkBack can
 * reach them (§4.11), and its busy state while one runs; a long press opens the same actions.
 */
export function ConversationRow({
  title,
  age,
  pinned = false,
  snippet = null,
  onPress,
  onLongPress,
  a11y,
  current = false,
  chevron,
}: {
  title: string | null | undefined;
  age: string;
  pinned?: boolean;
  snippet?: readonly SnippetPart[] | null;
  onPress: () => void;
  onLongPress?: () => void;
  a11y?: SwipeRowA11y;
  /** Open in the detail column beside the list (regular width, issue #15): highlighted, read as selected. */
  current?: boolean;
  /** False when a tap opens the chat beside the list instead of navigating (no disclosure chevron). */
  chevron?: boolean;
}) {
  const { colors } = useDesign();
  const name = title || UNTITLED;
  const passage = snippet && snippet.length > 0 ? snippet.map((part) => part.text).join('') : null;
  return (
    <View style={{ backgroundColor: current ? colors.accentSoft : colors.surface }}>
      <ListRow
        title={name}
        value={age}
        leading={pinned ? <Icon name="pin" size={17} tone="secondaryBrand" /> : undefined}
        footer={
          snippet && passage ? (
            <Text variant="footnote" tone="secondary" numberOfLines={2} testID="chat-snippet">
              {snippet.map((part, i) =>
                part.match ? (
                  <Text key={i} variant="footnote" tone="primary" emphasized>
                    {part.text}
                  </Text>
                ) : (
                  part.text
                ),
              )}
            </Text>
          ) : undefined
        }
        accessibilityLabel={[name, pinned ? 'pinned' : null, age, passage].filter(Boolean).join(', ')}
        onPress={onPress}
        onLongPress={onLongPress}
        accessibilityActions={a11y?.accessibilityActions}
        onAccessibilityAction={a11y?.onAccessibilityAction}
        busy={a11y?.accessibilityState?.busy === true}
        current={current}
        chevron={chevron}
      />
    </View>
  );
}
