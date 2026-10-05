import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import {
  CONVERSATIONS_PAGE_SIZE,
  fetchConversationPage,
  flattenConversationPages,
  nextConversationOffset,
  type ConversationPage,
} from '@/api/queries/conversations';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const ORIGIN = 'https://ai.example.com';
const row = (id: number) => ({
  id,
  name: `c${id}`,
  created_at: '2026-10-04T12:00:00Z',
  updated_at: '2026-10-04T12:00:00Z',
  duration: -1,
  message_groups_count: 0,
});

describe('conversation paging', () => {
  it('computes the next offset and stops at the total', () => {
    const page = (offset: number, n: number, total: number): ConversationPage => ({
      offset,
      total,
      rows: Array.from({ length: n }, (_, i) => row(offset + i + 1)),
    });
    expect(nextConversationOffset(page(0, 30, 75))).toBe(30);
    expect(nextConversationOffset(page(60, 15, 75))).toBeUndefined();
    expect(nextConversationOffset(page(30, 0, 75))).toBeUndefined();
  });

  it('drops a row repeated by a later page', () => {
    const flat = flattenConversationPages([
      { offset: 0, total: 3, rows: [row(3), row(2)] },
      { offset: 2, total: 3, rows: [row(2), row(1)] },
    ]);
    expect(flat.map((r) => r.id)).toEqual([3, 2, 1]);
    expect(flattenConversationPages(undefined)).toEqual([]);
  });
});

describe('fetchConversationPage', () => {
  beforeEach(() => {
    __resetSessionForTests();
    setSession({ hydrated: true, serverOrigin: ORIGIN, projectId: 7 });
    setCredential({ kind: 'pat', token: 't' });
  });

  it('asks for the caller own conversations with offset paging', async () => {
    const urls: string[] = [];
    const fetch: BaseFetch = async (input) => {
      urls.push(input.url);
      return new Response(JSON.stringify({ total: 1, rows: [row(9)] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    };
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    const page = await fetchConversationPage(7, 30, client);
    expect(page).toEqual({ offset: 30, total: 1, rows: [row(9)] });
    const url = urls[0];
    expect(url.startsWith(`${ORIGIN}/api/v2/elitea_core/conversations/prompt_lib/7?`)).toBe(true);
    expect(url).toContain('mine=true');
    expect(url).toContain(`limit=${CONVERSATIONS_PAGE_SIZE}`);
    expect(url).toContain('offset=30');
  });
});
