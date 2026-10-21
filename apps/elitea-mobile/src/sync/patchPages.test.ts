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
});
