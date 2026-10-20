/**
 * A turn in a conversation moves it to the top of Today, in every list that
 * reads the grouped listing (the web rail and the desktop sidebar's chats).
 *
 * The server buckets the date groups by last activity, its newest message
 * included (`folders/handler.go` `groupByDate`). So:
 *
 * - `sent` — a message went out: the cached listing (`['folder', 'list', …]`)
 *   moves the row to the top of Today at once. Each list re-reads the cache
 *   (`useQueryFoldersList`), so both lists move it together.
 * - `settled` — the turn ended: the listing is read again, so the rows end
 *   where the server puts them.
 *
 * Only an ungrouped row moves. A pinned row and a row in a folder stay where
 * they are: the folder holds the conversation, not the date.
 */
import { useCallback } from 'react';

import { useQueryClient, type QueryClient } from '@tanstack/react-query';

import type { DateGroup, FolderConversationRef, GroupedFoldersResponse } from '@/entities/folder';

const FOLDER_LIST_KEY = ['folder', 'list'] as const;
/** The server's label of the newest bucket (`folders/handler.go` `groupOrder`). */
const TODAY = 'Today';

export type ConversationActivity = 'sent' | 'settled';

function isToday(group: DateGroup): boolean {
  return group.name.toLowerCase() === TODAY.toLowerCase();
}

/** The listing with the conversation at the top of Today; the same listing when it is not an ungrouped row. */
export function moveToToday(listing: GroupedFoldersResponse, conversationId: string, now: string): GroupedFoldersResponse {
  const source = listing.dateGroups.find((group) => group.conversations.some((row) => row.id === conversationId));
  if (source === undefined) return listing;
  const row = source.conversations.find((item) => item.id === conversationId) as FolderConversationRef;
  if (isToday(source) && source.conversations[0]?.id === conversationId) return listing;
  const moved: FolderConversationRef = { ...row, updatedAt: now };
  const without = listing.dateGroups.map((group) => {
    if (group !== source) return group;
    return {
      ...group,
      conversations: group.conversations.filter((item) => item.id !== conversationId),
      ...(group.total !== undefined ? { total: group.total - 1 } : {}),
      ...(group.offset !== undefined ? { offset: group.offset - 1 } : {}),
    };
  });
  const today = without.find(isToday);
  const withToday: DateGroup[] =
    today === undefined
      ? [{ name: TODAY, conversations: [moved], total: 1, offset: 1 }, ...without]
      : without.map((group) =>
          group === today
            ? {
                ...group,
                conversations: [moved, ...group.conversations],
                ...(group.total !== undefined ? { total: group.total + 1 } : {}),
                ...(group.offset !== undefined ? { offset: group.offset + 1 } : {}),
              }
            : group,
        );
  return { ...listing, dateGroups: withToday.filter((group) => group.conversations.length > 0 || (group.total ?? 0) > 0) };
}

export function applyConversationActivity(queryClient: QueryClient, phase: ConversationActivity, conversationId: string): void {
  if (phase === 'settled') {
    void queryClient.invalidateQueries({ queryKey: FOLDER_LIST_KEY });
    return;
  }
  const now = new Date().toISOString();
  queryClient.setQueriesData<GroupedFoldersResponse>({ queryKey: FOLDER_LIST_KEY }, (listing) =>
    listing === undefined || !Array.isArray(listing.dateGroups) ? listing : moveToToday(listing, conversationId, now),
  );
}

/** The chat page's side: tell the listing that a turn started (`sent`) or ended (`settled`) in this conversation. */
export function useConversationActivity(): (phase: ConversationActivity, conversationId: string) => void {
  const queryClient = useQueryClient();
  return useCallback((phase, conversationId) => applyConversationActivity(queryClient, phase, conversationId), [queryClient]);
}
