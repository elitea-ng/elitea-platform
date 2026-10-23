import { ActivityIndicator, StyleSheet, View } from 'react-native';

import type { SwipeRowA11y } from '@/components/SwipeToDelete';
import { useDesign } from '@/design/DesignProvider';
import type { SnippetPart } from '@/db/search';
import { ACTIVITY_LABEL, UNREAD_LABEL, type ActivityGlyphKind } from '@/features/chats/activity';
import { ActivityGlyph } from '@/features/chats/ActivityGlyph';
import { executorBadge, type WorkspaceExecutor } from '@/features/localWork/model';
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
 * From client contract 1.12 the row carries its activity glyph at the trailing edge (running,
 * needs you, finished unread or read) and a bold title while an answer is unread; the glyph's
 * name is in the spoken label.
 */
export function ConversationRow({
  title,
  age,
  pinned = false,
  snippet = null,
  executor,
  onPress,
  onLongPress,
  a11y,
  current = false,
  chevron,
  archivedBy,
  busy = false,
  busyText,
  activity = null,
  unread = false,
}: {
  title: string | null | undefined;
  age: string;
  pinned?: boolean;
  snippet?: readonly SnippetPart[] | null;
  /** A Local work thread: where it runs now, drawn as a badge under the title ("In cloud", "Returning", "On laptop"). */
  executor?: WorkspaceExecutor;
  onPress: () => void;
  onLongPress?: () => void;
  a11y?: SwipeRowA11y;
  /** Open in the detail column beside the list (regular width, issue #15): highlighted, read as selected. */
  current?: boolean;
  /** False when a tap opens the chat beside the list instead of navigating (no disclosure chevron). */
  chevron?: boolean;
  /** The Archived chats list: who archived it, drawn under the title ("Archived by Ana"). */
  archivedBy?: string;
  /** An action runs on the row (a fork): a spinner, `busyText` for the age, read as busy. */
  busy?: boolean;
  busyText?: string;
  /** Client contract 1.12: the activity glyph (null: none). */
  activity?: ActivityGlyphKind | null;
  /** Client contract 1.12: an answer not opened yet (the title is bold). */
  unread?: boolean;
}) {
  const { colors } = useDesign();
  const archivedLine = archivedBy ? `Archived by ${archivedBy}` : null;
  const shownAge = busy && busyText ? busyText : age;
  const name = title || UNTITLED;
  const passage = snippet && snippet.length > 0 ? snippet.map((part) => part.text).join('') : null;
  const badge = executor === undefined ? null : executorBadge(executor);
  const activityLabel = activity !== null ? ACTIVITY_LABEL[activity] : unread ? UNREAD_LABEL : null;
  return (
    <View style={{ backgroundColor: current ? colors.accentSoft : colors.surface }}>
      <ListRow
        title={name}
        titleEmphasized={unread}
        value={shownAge}
        leading={pinned ? <Icon name="pin" size={17} tone="secondaryBrand" /> : undefined}
        trailing={busy ? <ActivityIndicator testID="chat-row-busy" color={colors.accent} /> : activity !== null ? <ActivityGlyph kind={activity} /> : undefined}
        footer={
          badge !== null || (snippet && passage) || archivedLine ? (
            <View style={styles.footer}>
              {badge !== null ? (
                <View
                  testID="executor-badge"
                  style={[styles.badge, { backgroundColor: current ? colors.surface : executor === 'cloud' ? colors.accentSoft : colors.surfaceSunken, borderColor: current ? colors.separatorStrong : 'transparent' }]}>
                  <Text variant="footnote" color={executor === 'cloud' && !current ? colors.accentSoftOn : colors.textPrimary} emphasized>
                    {badge}
                  </Text>
                </View>
              ) : null}
              {snippet && passage ? (
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
              ) : null}
              {archivedLine ? (
                <Text variant="footnote" tone="secondary" numberOfLines={1} testID="archived-by">
                  {archivedLine}
                </Text>
              ) : null}
            </View>
          ) : undefined
        }
        accessibilityLabel={[name, pinned ? 'pinned' : null, activityLabel, badge, archivedLine, shownAge, passage].filter(Boolean).join(', ')}
        onPress={onPress}
        onLongPress={onLongPress}
        accessibilityActions={a11y?.accessibilityActions}
        onAccessibilityAction={a11y?.onAccessibilityAction}
        busy={busy || a11y?.accessibilityState?.busy === true}
        current={current}
        chevron={chevron}
      />
    </View>
  );
}

const styles = StyleSheet.create({
  footer: { gap: 4, alignItems: 'flex-start' },
  badge: { borderWidth: StyleSheet.hairlineWidth, borderRadius: 9999, paddingHorizontal: 8, paddingVertical: 2 },
});
