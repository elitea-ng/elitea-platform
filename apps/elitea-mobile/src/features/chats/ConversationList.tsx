import { useMemo, type ReactElement } from 'react';
import { FlatList, StyleSheet, type RefreshControlProps } from 'react-native';

import { SwipeToDelete } from '@/components/SwipeToDelete';
import { useDesign } from '@/design/DesignProvider';
import { ConversationRow } from '@/features/chats/ConversationRow';
import { groupChatsByDate, type ChatSection } from '@/features/chats/sections';
import { ListSection } from '@/ui';

export interface ChatListItem {
  id: number;
  name: string | null | undefined;
  updated_at: string;
  /** The row's age text ("5m", "2d", "2026-09-28"). */
  age: string;
}

/**
 * The Chats list (docs/screens.md §3): inset-grouped sections by day (Today, Yesterday, Previous
 * 7 days, Earlier), each row swipes left to Delete (asks first; the same action is in the row's
 * accessibility actions). The Chats tab and its `__DEV__` preview both render this (D2).
 */
export function ConversationList({
  rows,
  now,
  onOpen,
  onDelete,
  header,
  empty,
  footer,
  refreshControl,
  onEndReached,
}: {
  rows: readonly ChatListItem[];
  now?: number;
  onOpen: (row: ChatListItem) => void;
  /** Deletes a conversation (after the user confirmed); null when deleting is not possible (offline copy). */
  onDelete: ((row: ChatListItem) => Promise<boolean>) | null;
  header?: ReactElement;
  empty: ReactElement;
  footer?: ReactElement;
  refreshControl?: ReactElement<RefreshControlProps>;
  onEndReached?: () => void;
}) {
  const { colors, space } = useDesign();
  const sections = useMemo(() => groupChatsByDate(rows, now), [rows, now]);
  return (
    <FlatList<ChatSection<ChatListItem>>
      testID="chats-list"
      contentInsetAdjustmentBehavior="automatic"
      keyboardDismissMode="on-drag"
      style={{ backgroundColor: colors.background }}
      contentContainerStyle={sections.length === 0 ? styles.grow : styles.list}
      data={sections}
      keyExtractor={(section) => section.key}
      refreshControl={refreshControl}
      ListHeaderComponent={header}
      ListEmptyComponent={empty}
      ListFooterComponent={footer}
      onEndReachedThreshold={0.5}
      onEndReached={onEndReached}
      renderItem={({ item: section }) => (
        <ListSection title={section.title} style={[styles.section, { marginHorizontal: space.screenGutterCompact }]}>
          {section.data.map((row) => (
            <SwipeToDelete key={row.id} actionLabel="Delete" onDelete={onDelete ? () => onDelete(row) : null} testID={`chat-${row.id}`}>
              {(a11y) => <ConversationRow title={row.name} age={row.age} onPress={() => onOpen(row)} a11y={a11y} />}
            </SwipeToDelete>
          ))}
        </ListSection>
      )}
    />
  );
}

const styles = StyleSheet.create({
  grow: { flexGrow: 1 },
  list: { paddingBottom: 24 },
  section: { marginBottom: 20 },
});
