import { DeltaShapeError, fetchConversationChanges, fetchMessageChanges, fetchNotificationChanges } from '@/sync/deltaApi';
import {
  conversationDelta,
  conversationRow,
  messageDelta,
  messageRow,
  notificationDelta,
  notificationRow,
  tombstone,
} from '@/sync/__fixtures__/wp6';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('delta requests', () => {
  it('lists conversations with the fixed mine=true filter, limit 100 and the cursor', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/conversations/prompt_lib/7', { status: 200, body: conversationDelta([conversationRow(1)]) });
    const page = await fetchConversationChanges(server.client, 7, 'abc');
    const url = server.requests[0].url;
    expect(url.pathname).toBe('/api/v2/elitea_core/conversations/prompt_lib/7');
    expect(Object.fromEntries(url.searchParams)).toEqual({ mine: 'true', limit: '100', changes_since: 'abc' });
    expect(page.rows.map((r) => r.id)).toEqual([1]);
    expect(page.hasMore).toBe(false);
  });

  it('reads a transcript by the numeric conversation id', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/messages/prompt_lib/7/42', {
      status: 200,
      body: messageDelta([messageRow(5)], { tombstones: [tombstone(4)] }),
    });
    const page = await fetchMessageChanges(server.client, 7, 42, '0');
    expect(server.requests[0].url.pathname).toBe('/api/v2/elitea_core/messages/prompt_lib/7/42');
    expect(page.rows[0].id).toBe('5');
    expect(page.tombstones).toEqual([tombstone(4)]);
  });

  it('sends nothing but limit and the cursor for notifications (anything else is a 400)', async () => {
    const server = fakeServer();
    server.on('GET', '/notifications/notifications/prompt_lib/7', { status: 200, body: notificationDelta([notificationRow(1)]) });
    await fetchNotificationChanges(server.client, 7, '0');
    expect(Object.fromEntries(server.requests[0].url.searchParams)).toEqual({ limit: '100', changes_since: '0' });
  });
});

describe('delta validation', () => {
  it('accepts Go null slices as empty', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/messages/', { status: 200, body: messageDelta(null, { tombstones: null }) });
    const page = await fetchMessageChanges(server.client, 7, 42, '0');
    expect(page.rows).toEqual([]);
    expect(page.tombstones).toEqual([]);
  });

  it('skips a malformed row or tombstone instead of failing the page', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/conversations/', {
      status: 200,
      body: conversationDelta([conversationRow(1), { id: 'x' }, conversationRow(2)], {
        tombstones: [tombstone(3), { id: -1, reason: 'deleted', deleted_at: '' }],
      }),
    });
    const page = await fetchConversationChanges(server.client, 7, '0');
    expect(page.rows.map((r) => r.id)).toEqual([1, 2]);
    expect(page.tombstones.map((t) => t.id)).toEqual([3]);
  });

  it('refuses a legacy page (a server without incremental sync)', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/conversations/', { status: 200, body: { total: 1, rows: [conversationRow(1)] } });
    await expect(fetchConversationChanges(server.client, 7, '0')).rejects.toBeInstanceOf(DeltaShapeError);
  });

  it('keeps unknown tombstone reasons (they are removals too)', async () => {
    const server = fakeServer();
    server.on('GET', '/notifications/', {
      status: 200,
      body: notificationDelta([], { tombstones: [{ ...tombstone(9), reason: 'access_revoked' }] }),
    });
    const page = await fetchNotificationChanges(server.client, 7, '0');
    expect(page.tombstones.map((t) => t.reason)).toEqual(['access_revoked']);
  });
});
