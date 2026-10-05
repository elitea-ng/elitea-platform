import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import { fetchConversation, hasStreamingGroup, isConversationRef } from '@/api/queries/conversation';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const ORIGIN = 'https://ai.example.com';

describe('conversation read', () => {
  beforeEach(() => {
    __resetSessionForTests();
    setSession({ hydrated: true, serverOrigin: ORIGIN, projectId: 7 });
    setCredential({ kind: 'pat', token: 't' });
  });

  it('accepts a numeric id or a uuid as the route reference', () => {
    expect(isConversationRef('42')).toBe(true);
    expect(isConversationRef('3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b')).toBe(true);
    for (const bad of ['', '0', '-1', '1.5', '../x', '42/../../admin', 'abc', undefined]) {
      expect(isConversationRef(bad)).toBe(false);
    }
  });

  it('requests the latest 50 message groups', async () => {
    const urls: string[] = [];
    const fetch: BaseFetch = async (input) => {
      urls.push(input.url);
      return new Response(JSON.stringify({ id: '42', project_id: '7', name: 'n', created_at: '', updated_at: '' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    };
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    await expect(fetchConversation(7, '42', client)).resolves.toMatchObject({ id: '42' });
    expect(urls[0]).toBe(`${ORIGIN}/api/v2/elitea_core/conversation/prompt_lib/7/42?messages_limit=50&sort_order=desc`);
    await expect(fetchConversation(7, '../x', client)).rejects.toThrow('invalid conversation id');
  });

  it('detects a group still being written', () => {
    expect(hasStreamingGroup(undefined)).toBe(false);
    expect(
      hasStreamingGroup({ message_groups: [{ is_streaming: true }] } as never),
    ).toBe(true);
  });
});
