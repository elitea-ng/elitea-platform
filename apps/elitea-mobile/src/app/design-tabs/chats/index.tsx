import { router, Stack } from 'expo-router';
import { useMemo, useState } from 'react';
import { FlatList, StyleSheet } from 'react-native';

import { useDesign } from '@/design/DesignProvider';
import { ConversationRow, ConversationSeparator } from '@/features/chats/ConversationRow';
import { filterConversations } from '@/features/chats/filterConversations';
import { SAMPLE_CHATS, SAMPLE_ORIGIN } from '@/features/designGallery/fixtures';
import { notificationBellAction } from '@/features/notifications/NotificationBell';
import { projectSwitcherOptions } from '@/features/projects/ProjectChip';
import { WorkspacePillView } from '@/features/workspaces/WorkspacePill';
import { EmptyState, headerActionOptions, searchBarOptions } from '@/ui';

/**
 * Development builds only: the Chats tab chrome with sample conversations, drawn with the same
 * row, separator and workspace pill as the real tab (D2), so a screenshot of it is the list the
 * user gets.
 */
export default function DesignChatsPreview() {
  const design = useDesign();
  const { colors } = design;
  const [query, setQuery] = useState('');
  const rows = useMemo(() => filterConversations(SAMPLE_CHATS, query), [query]);
  return (
    <>
      <Stack.Screen
        options={{
          ...projectSwitcherOptions('Marketing'),
          ...headerActionOptions(
            [{ ...notificationBellAction(3), onPress: () => router.push('/design-sheet') }, { id: 'new', icon: 'newChat', label: 'New chat', onPress: () => {} }],
            design,
          ),
          ...searchBarOptions(design, { placeholder: 'Search chats', onChange: setQuery }),
        }}
      />
      <FlatList
        contentInsetAdjustmentBehavior="automatic"
        keyboardDismissMode="on-drag"
        style={{ backgroundColor: colors.background }}
        contentContainerStyle={rows.length === 0 ? styles.grow : styles.list}
        data={rows}
        keyExtractor={(c) => String(c.id)}
        ListHeaderComponent={<WorkspacePillView origin={SAMPLE_ORIGIN} image={null} />}
        ListEmptyComponent={<EmptyState mascot="bust" title={`Nothing found for “${query.trim()}”`} body="Try another word from the chat's title." />}
        ItemSeparatorComponent={ConversationSeparator}
        renderItem={({ item }) => <ConversationRow title={item.name} age={item.age} onPress={() => {}} />}
      />
    </>
  );
}

const styles = StyleSheet.create({
  grow: { flexGrow: 1 },
  list: { paddingBottom: 24 },
});
