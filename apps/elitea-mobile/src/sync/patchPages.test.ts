import { QueryClient, type InfiniteData } from '@tanstack/react-query';

import { queryKeys } from '@/api/queries/keys';
import type { ConversationPage } from '@/api/queries/conversations';
import { patchConversationPages } from '@/sync/appConversationOps';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const row = (id: number) => ({ id, name: `c${id}`, created_at: '', updated_at: '', duration: -1, message_groups_count: 0 });
const pages = (...ids: number[]): InfiniteData<ConversationPage, number> => ({ pages: [{ offset: 0, total: ids.length, rows: ids.map(row) }], pageParams: [0] });

describe('patchConversationPages', () => {
  it('a delete leaves both the Chats list and the Local work list', () => {
    const client = new QueryClient();
    client.setQueryData(queryKeys.conversations(7), pages(1, 2));
    client.setQueryData(queryKeys.localWork(7), pages(2, 3));
    patchConversationPages(client, 7, 2, { kind: 'delete' });
    expect(client.getQueryData<InfiniteData<ConversationPage>>(queryKeys.conversations(7))?.pages[0].rows.map((r) => r.id)).toEqual([1]);
    expect(client.getQueryData<InfiniteData<ConversationPage>>(queryKeys.localWork(7))?.pages[0].rows.map((r) => r.id)).toEqual([3]);
  });

  it('a read (client contract 1.12) sets unread on the row in both lists, nothing else', () => {
    const client = new QueryClient();
    client.setQueryData(queryKeys.conversations(7), pages(1, 2));
    client.setQueryData(queryKeys.localWork(7), pages(2, 3));
    patchConversationPages(client, 7, 2, { kind: 'unread', unread: false });
    const unread = (key: readonly unknown[]) => client.getQueryData<InfiniteData<ConversationPage>>(key)?.pages[0].rows.map((r) => [r.id, r.unread]);
    expect(unread(queryKeys.conversations(7))).toEqual([
      [1, undefined],
      [2, false],
    ]);
    expect(unread(queryKeys.localWork(7))).toEqual([
      [2, false],
      [3, undefined],
    ]);
  });
});
