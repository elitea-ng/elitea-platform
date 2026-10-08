import { useMemo, useState, type ReactElement } from 'react';
import { FlatList, StyleSheet, type RefreshControlProps } from 'react-native';

import { SwipeActions, type RowAction } from '@/components/SwipeActions';
import type { SnippetPart } from '@/db/search';
import { useDesign } from '@/design/DesignProvider';
import { ActionMenu, type ActionMenuItem } from '@/features/chat/ActionMenu';
import { ConversationRow, UNTITLED } from '@/features/chats/ConversationRow';
import { CHAT_SECTION_TITLE, groupChatsByDate, type ChatSection } from '@/features/chats/sections';
import { ListSection, uiPlatform } from '@/ui';

export interface ChatListItem {
  id: number;
  name: string | null | undefined;
  updated_at: string;
  /** The row's age text ("5m", "2d", "2026-09-28"). */
  age: string;
  /** Pinned in its project (client contract 1.4): the row goes to the Pinned section. */
  pinned?: boolean;
  /** A search hit in a message: the matched passage. */
  snippet?: readonly SnippetPart[] | null;
}

/** What the rows can do; each is absent when it is not possible (an older server, the offline copy). */
export interface ChatRowActions {
  /** Deletes a conversation (asks first); resolves true when the row went away. */
  onDelete: ((row: ChatListItem) => Promise<boolean>) | null;
  /** Pins or unpins (client contract 1.4). */
  onTogglePin?: ((row: ChatListItem) => void) | null;
  /** Opens the rename sheet (client contract 1.4). */
  onRename?: ((row: ChatListItem) => void) | null;
}

function rowActions(row: ChatListItem, actions: ChatRowActions | null): { pin: RowAction | null; rename: RowAction | null; remove: RowAction | null } {
  const togglePin = actions?.onTogglePin ?? null;
  const rename = actions?.onRename ?? null;
  const remove = actions?.onDelete ?? null;
  return {
    pin: togglePin ? { name: 'pin', label: row.pinned ? 'Unpin' : 'Pin', icon: row.pinned ? 'unpin' : 'pin', run: () => togglePin(row) } : null,
    rename: rename ? { name: 'rename', label: 'Rename', icon: 'rename', run: () => rename(row) } : null,
    remove: remove ? { name: 'delete', label: 'Delete', icon: 'delete', run: () => remove(row) } : null,
  };
}

/**
 * The Chats list (docs/screens.md §3): inset-grouped sections (Pinned, then by day: Today,
 * Yesterday, Previous 7 days, Earlier), or one Results section while searching. Row actions
 * follow design-system §4.11: on iOS the leading swipe toggles Pin and the trailing swipe
 * deletes (it asks first); Android has the trailing swipe only. Every action (Pin, Rename,
 * Delete) is also in the row's long-press menu and in its accessibility actions. The Chats
 * tab and its `__DEV__` preview both render this (D2).
 */
export function ConversationList({
  rows,
  now,
  onOpen,
  actions,
  onDelete,
  results = false,
  header,
  empty,
  footer,
  refreshControl,
  onEndReached,
}: {
  rows: readonly ChatListItem[];
  now?: number;
  onOpen: (row: ChatListItem) => void;
  actions?: ChatRowActions | null;
  /** Shorthand for `actions={{ onDelete }}`. */
  onDelete?: ((row: ChatListItem) => Promise<boolean>) | null;
  /** Search results: one flat section, in the given order. */
  results?: boolean;
  header?: ReactElement;
  empty: ReactElement;
  footer?: ReactElement;
  refreshControl?: ReactElement<RefreshControlProps>;
  onEndReached?: () => void;
}) {
  const { colors, space } = useDesign();
  const rowsActions: ChatRowActions | null = actions ?? (onDelete !== undefined ? { onDelete } : null);
  const sections = useMemo<ChatSection<ChatListItem>[]>(
    () => (results ? (rows.length > 0 ? [{ key: 'results', title: CHAT_SECTION_TITLE.results, data: [...rows] }] : []) : groupChatsByDate(rows, now)),
    [rows, now, results],
  );
  const [menu, setMenu] = useState<ChatListItem | null>(null);
  const menuItems: ActionMenuItem[] = [];
  if (menu) {
    const { pin, rename, remove } = rowActions(menu, rowsActions);
    const close = (run: () => unknown) => () => {
      setMenu(null);
      void run();
    };
    if (pin) menuItems.push({ key: 'pin', label: pin.label, icon: pin.icon, onPress: close(pin.run) });
    if (rename) menuItems.push({ key: 'rename', label: 'Rename', icon: 'rename', onPress: close(rename.run) });
    if (remove) menuItems.push({ key: 'delete', label: 'Delete', icon: 'delete', destructive: true, onPress: close(remove.run) });
  }
  const hasMenu = rowsActions !== null && (rowsActions.onDelete || rowsActions.onTogglePin || rowsActions.onRename);
  return (
    <>
      <FlatList<ChatSection<ChatListItem>>
        testID="chats-list"
        contentInsetAdjustmentBehavior="automatic"
        keyboardDismissMode="on-drag"
        keyboardShouldPersistTaps="handled"
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
            {section.data.map((row) => {
              const { pin, rename, remove } = rowActions(row, rowsActions);
              return (
                <SwipeActions
                  key={row.id}
                  // HIG: the leading swipe is a toggle (Pin); Material 3 has none (§4.11).
                  leading={uiPlatform === 'ios' ? pin : null}
                  trailing={remove}
                  extra={[...(uiPlatform === 'ios' ? [] : pin ? [pin] : []), ...(rename ? [rename] : [])]}
                  testID={`chat-${row.id}`}>
                  {(a11y) => (
                    <ConversationRow
                      title={row.name}
                      age={row.age}
                      pinned={row.pinned === true}
                      snippet={row.snippet}
                      onPress={() => onOpen(row)}
                      onLongPress={hasMenu ? () => setMenu(row) : undefined}
                      a11y={a11y}
                    />
                  )}
                </SwipeActions>
              );
            })}
          </ListSection>
        )}
      />
      <ActionMenu visible={menu !== null} title={menu?.name || UNTITLED} items={menuItems} onClose={() => setMenu(null)} />
    </>
  );
}

const styles = StyleSheet.create({
  grow: { flexGrow: 1 },
  list: { paddingBottom: 24 },
  section: { marginBottom: 20 },
});
