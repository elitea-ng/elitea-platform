import { router, Stack, useLocalSearchParams } from 'expo-router';
import { useEffect, useMemo, useRef, useState } from 'react';
import { Alert } from 'react-native';
import type { SearchBarCommands } from 'react-native-screens';

import type { SearchHit } from '@/db/search';
import { useDesign } from '@/design/DesignProvider';
import { buildChatRows, buildSearchRows } from '@/features/chats/chatListModel';
import { ConversationList, type ChatListItem, type ChatRowActions } from '@/features/chats/ConversationList';
import { UNTITLED } from '@/features/chats/ConversationRow';
import { UndoToast } from '@/features/chats/UndoToast';
import { usePendingDelete } from '@/features/chats/usePendingDelete';
import { SAMPLE_CHATS, SAMPLE_ORIGIN, SAMPLE_PROJECT_NAME, sampleChatRow } from '@/features/designGallery/fixtures';
import { previewSearch } from '@/features/designGallery/searchPreviewDb';
import { notificationBellAction } from '@/features/notifications/NotificationBell';
import { projectSwitcherOptions } from '@/features/projects/ProjectChip';
import { WorkspacePillView } from '@/features/workspaces/WorkspacePill';
import { log } from '@/lib/log';
import { EmptyState, headerActionOptions, searchBarOptions } from '@/ui';

/**
 * Development builds only: the Chats tab chrome with sample conversations, drawn with the same
 * list, rows, sections, swipe actions, long-press menu and Undo toast as the real tab (D2), so a
 * screenshot of it is the list the user gets. Search runs the real offline full-text search over
 * a throwaway SQLCipher store on the device (`searchPreviewDb`). `?q=` types a query into the
 * header search, `?undo=1` shows a delete waiting out its Undo.
 */
export default function DesignChatsPreview() {
  const design = useDesign();
  const params = useLocalSearchParams<{ q?: string; undo?: string }>();
  const [query, setQuery] = useState(typeof params.q === 'string' ? params.q : '');
  const [now] = useState(() => Date.now());
  const [pins, setPins] = useState<ReadonlyMap<number, boolean>>(new Map());
  const [found, setFound] = useState<{ query: string; hits: SearchHit[] } | null>(null);
  const search = useRef<SearchBarCommands>(null);
  const removal = usePendingDelete<ChatListItem, null>(null, () => {});
  const loaded = useMemo(() => SAMPLE_CHATS.map((c) => sampleChatRow(c, now)).map((r) => ({ ...r, created_at: r.updated_at, duration: -1, message_groups_count: 1 })), [now]);
  const overlay = useMemo(() => new Map([...pins].map(([id, pinned]) => [id, { pinned }])), [pins]);
  const hidden = useMemo(() => new Set(removal.waiting.map((item) => item.row.id)), [removal.waiting]);
  const searching = query.trim() !== '';

  useEffect(() => {
    if (typeof params.q === 'string' && params.q !== '') {
      const timer = setTimeout(() => search.current?.setText(params.q as string), 400);
      return () => clearTimeout(timer);
    }
  }, [params.q]);
  useEffect(() => {
    if (params.undo === '1') {
      const sample = loaded.find((row) => row.id === 3);
      if (sample) removal.start({ ...sample, age: '' });
    }
    // Once, when the preview opens.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => {
    if (!searching) return;
    let live = true;
    previewSearch(query, now)
      .then((result) => {
        if (live) setFound({ query, hits: result.hits });
      })
      .catch((error: unknown) => log.warn('preview search failed', { error }));
    return () => {
      live = false;
    };
  }, [query, searching, now]);

  const hits = searching && found?.query === query ? found.hits : null;
  const rows = searching
    ? buildSearchRows({ hits, loaded, query, overlay, hidden, now })
    : buildChatRows({ loaded, savedPinned: [], overlay, hidden, now });
  const actions: ChatRowActions = {
    onDelete: async (row) => {
      removal.start(row);
      return true;
    },
    onTogglePin: (row) => setPins((prev) => new Map(prev).set(row.id, row.pinned !== true)),
    onRename: (row) => router.push({ pathname: '/design-rename', params: { name: row.name ?? '' } }),
  };
  return (
    <>
      <Stack.Screen
        options={{
          ...projectSwitcherOptions(SAMPLE_PROJECT_NAME),
          ...headerActionOptions(
            [{ ...notificationBellAction(3), onPress: () => router.push('/design-sheet') }, { id: 'new', icon: 'newChat', label: 'New chat', onPress: () => Alert.alert('New chat') }],
            design,
          ),
          headerSearchBarOptions: { ...searchBarOptions(design, { placeholder: 'Search chats', onChange: setQuery }).headerSearchBarOptions, ref: search },
        }}
      />
      <ConversationList
        rows={rows}
        results={searching}
        now={now}
        onOpen={() => {}}
        actions={actions}
        header={<WorkspacePillView origin={SAMPLE_ORIGIN} image={null} brand={null} />}
        empty={<EmptyState mascot="bust" title={`Nothing found for “${query.trim()}”`} body="Try another word from the chat's title or its messages." />}
      />
      <UndoToast
        text={removal.pending ? 'Chat deleted' : null}
        subject={removal.pending}
        subjectName={removal.pending ? removal.pending.name || UNTITLED : null}
        onUndo={removal.undo}
      />
    </>
  );
}
