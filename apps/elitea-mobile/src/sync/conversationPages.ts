import type { InfiniteData, QueryClient } from '@tanstack/react-query';

import type { ConversationAction } from '@/api/queries/conversationActions';
import type { ConversationPage } from '@/api/queries/conversations';
import { queryKeys } from '@/api/queries/keys';

/**
 * A change to a conversation's row in the loaded list pages: a Chats-list action taken, or the
 * caller's `unread` after a read (client contract 1.12, src/api/queries/markRead.ts).
 */
export type ConversationPagePatch = ConversationAction | { kind: 'unread'; unread: boolean };

/** Patches the loaded list pages so a taken action (or a read) shows before the refetch lands. */
export function patchConversationPages(client: QueryClient, projectId: number, conversationId: number, change: ConversationPagePatch): void {
  const drop = (page: ConversationPage): ConversationPage => ({ ...page, rows: page.rows.filter((row) => row.id !== conversationId) });
  const patch = (key: readonly unknown[], edit: (page: ConversationPage) => ConversationPage) =>
    client.setQueryData<InfiniteData<ConversationPage, number>>(key, (data) => (data ? { ...data, pages: data.pages.map(edit) } : data));
  // Both lists: a Local work thread is deleted from its own list (rename and pin only match the Chats one).
  for (const key of [queryKeys.conversations(projectId), queryKeys.localWork(projectId)]) {
    patch(key, (page) => {
      // An archived chat leaves the lists; an unarchived one comes back with the refetch.
      if (change.kind === 'delete' || (change.kind === 'archive' && change.archived)) return drop(page);
      if (change.kind === 'archive') return page;
      return {
        ...page,
        rows: page.rows.map((row) => {
          if (row.id !== conversationId) return row;
          if (change.kind === 'rename') return { ...row, name: change.name };
          if (change.kind === 'pin') return { ...row, is_pinned: change.pinned };
          return { ...row, unread: change.unread };
        }),
      };
    });
  }
  // The Archived chats list: an unarchived or deleted chat leaves it (an archived one comes with its refetch).
  if (change.kind === 'delete' || (change.kind === 'archive' && !change.archived)) patch(queryKeys.archived(projectId), drop);
  else if (change.kind === 'rename') {
    patch(queryKeys.archived(projectId), (page) => ({ ...page, rows: page.rows.map((row) => (row.id === conversationId ? { ...row, name: change.name } : row)) }));
  }
}
