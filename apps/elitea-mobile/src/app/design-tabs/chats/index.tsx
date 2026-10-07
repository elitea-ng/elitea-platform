import { router, Stack } from 'expo-router';
import { useMemo, useState } from 'react';

import { useDesign } from '@/design/DesignProvider';
import { ConversationList } from '@/features/chats/ConversationList';
import { filterConversations } from '@/features/chats/filterConversations';
import { SAMPLE_CHATS, SAMPLE_ORIGIN, SAMPLE_PROJECT_NAME, sampleChatRow } from '@/features/designGallery/fixtures';
import { formatAge } from '@/lib/time';
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
  const [query, setQuery] = useState('');
  const [now] = useState(() => Date.now());
  const rows = useMemo(
    () => filterConversations(SAMPLE_CHATS.map((c) => sampleChatRow(c, now)), query).map((c) => ({ ...c, age: formatAge(c.updated_at, now) })),
    [query, now],
  );
  return (
    <>
      <Stack.Screen
        options={{
          ...projectSwitcherOptions(SAMPLE_PROJECT_NAME),
          ...headerActionOptions(
            [{ ...notificationBellAction(3), onPress: () => router.push('/design-sheet') }, { id: 'new', icon: 'newChat', label: 'New chat', onPress: () => {} }],
            design,
          ),
          ...searchBarOptions(design, { placeholder: 'Search chats', onChange: setQuery }),
        }}
      />
      <ConversationList
        rows={rows}
        now={now}
        onOpen={() => {}}
        onDelete={async () => false}
        header={<WorkspacePillView origin={SAMPLE_ORIGIN} image={null} brand={null} />}
        empty={<EmptyState mascot="bust" title={`Nothing found for “${query.trim()}”`} body="Try another word from the chat's title." />}
      />
    </>
  );
}
