import { fetchNotifications, fetchSelfUserIdFromNotifications, markSeen } from '@/api/queries/notifications';
import { endSession } from '@/security/endSession';
import { getSelfUserId, isSentQuestion, learnSelfUserId, noteSentQuestion, resetSelfIdentity } from '@/state/selfIdentity';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

// A listNotifications row as the server writes it (client contract 1.1).
const row = {
  id: 31,
  uuid: '9a3e0c4b-6d1f-4b2a-8e5c-7f9a0b1c2d3e',
  project_id: 7,
  user_id: 42,
  event_type: 'chat_user_mentioned',
  is_seen: false,
  meta: { conversation_id: '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c', message_id: '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f', sender: { id: 9, name: 'Alice Smith' } },
  created_at: '2026-10-05T10:00:00Z',
  updated_at: null,
};

beforeEach(() => {
  __resetSessionForTests();
  resetSelfIdentity();
  setSession({ serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 'tok' });
});

describe('self identity', () => {
  it('learns "me" from the recipient of a notification row', async () => {
    const server = fakeServer();
    server.on('GET', '/notifications/notifications/prompt_lib/7', { status: 200, body: { total: 1, rows: [row] } });
    await expect(fetchSelfUserIdFromNotifications(7, server.client)).resolves.toBe(42);
    expect(getSelfUserId()).toBe(42);
    expect(server.requests[0].url.searchParams.get('limit')).toBe('1');
  });

  it('learns it from the notification list too, and stays unknown without rows', async () => {
    const server = fakeServer();
    server.on('GET', '/notifications/notifications/prompt_lib/7', { status: 200, body: { total: 0, rows: [] } });
    await fetchNotifications(7, server.client);
    expect(getSelfUserId()).toBeNull();
    server.on('GET', '/notifications/notifications/prompt_lib/7', { status: 200, body: { total: 1, rows: [row] } });
    await fetchNotifications(7, server.client);
    expect(getSelfUserId()).toBe(42);
  });

  it('ignores ids that are not positive integers', () => {
    for (const id of ['42', 0, -3, 1.5, null]) learnSelfUserId(id);
    expect(getSelfUserId()).toBeNull();
  });

  it('remembers the question ids this device sent, case-insensitively', () => {
    noteSentQuestion('0B7E5D3A-1C2F-4E6A-8B9D-0A1B2C3D4E5F');
    expect(isSentQuestion('0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f')).toBe(true);
    expect(isSentQuestion(null)).toBe(false);
  });

  it('is wiped with the session', async () => {
    learnSelfUserId(42);
    noteSentQuestion('0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f');
    await endSession({ reason: 'user_logout' });
    expect(getSelfUserId()).toBeNull();
    expect(isSentQuestion('0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f')).toBe(false);
  });
});

describe('markSeen', () => {
  it('marks one notification seen with markNotificationSeen', async () => {
    const server = fakeServer();
    server.on('PUT', '/notifications/notification/prompt_lib/7/31', { status: 200, body: { ...row, is_seen: true } });
    await expect(markSeen(7, 31, server.client)).resolves.toBe(true);
    expect(server.requests[0]).toMatchObject({ method: 'PUT' });
    server.on('PUT', '/notifications/notification/prompt_lib/7/32', { status: 400, body: { ok: false, error: 'Notification is not found' } });
    await expect(markSeen(7, 32, server.client)).resolves.toBe(false);
  });
});
