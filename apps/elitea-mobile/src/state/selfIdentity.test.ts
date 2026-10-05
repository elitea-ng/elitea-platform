import { fetchNotifications, markSeen } from '@/api/queries/notifications';
import { endSession } from '@/security/endSession';
import { SECURE_KEYS, getSecureItem, setSecureItem } from '@/security/secureStorage';
import {
  fetchSelfIdentity,
  getSelfUserId,
  parseUserId,
  resetSelfIdentity,
  restoreSelfIdentity,
} from '@/state/selfIdentity';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

// getCurrentAuthor as the server answers it (client contract 1.2, `SocialAuthorProfile`).
const author = {
  id: '42',
  name: 'Bob',
  email: 'bob@example.com',
  avatar: '',
  description: '',
  personal_project_id: '7',
  provider_refs: ['oidc:bob'],
};

// A listNotifications row as the server writes it (client contract 1.1).
const row = {
  id: 31,
  uuid: '9a3e0c4b-6d1f-4b2a-8e5c-7f9a0b1c2d3e',
  project_id: 7,
  user_id: 99,
  event_type: 'chat_user_mentioned',
  is_seen: false,
  meta: { conversation_id: '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c', message_id: '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f', sender: { id: 9, name: 'Alice Smith' } },
  created_at: '2026-10-05T10:00:00Z',
  updated_at: null,
};

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

beforeEach(async () => {
  __resetSessionForTests();
  resetSelfIdentity();
  await endSession({ reason: 'user_logout' });
  __resetSessionForTests();
  setSession({ serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 'tok' });
});

describe('self identity ("who am I")', () => {
  it('reads the caller from getCurrentAuthor and caches only the id for the workspace', async () => {
    const server = fakeServer();
    server.on('GET', '/social/author', { status: 200, body: author });
    await expect(fetchSelfIdentity(server.client)).resolves.toBe(42);
    expect(getSelfUserId()).toBe(42);
    expect(server.requests[0]).toMatchObject({ method: 'GET' });
    expect(server.requests[0].url.pathname).toBe('/api/v2/social/author');
    expect(JSON.parse((await getSecureItem(SECURE_KEYS.selfUser))!)).toEqual({ origin: FAKE_ORIGIN, userId: 42 });
  });

  it('no longer learns "me" from a notification recipient', async () => {
    const server = fakeServer();
    server.on('GET', '/notifications/notifications/prompt_lib/7', { status: 200, body: { total: 1, rows: [row] } });
    await fetchNotifications(7, server.client);
    expect(getSelfUserId()).toBeNull();
  });

  it('restores the cached id at launch, for the same workspace only', async () => {
    await setSecureItem(SECURE_KEYS.selfUser, JSON.stringify({ origin: FAKE_ORIGIN, userId: 42 }));
    await restoreSelfIdentity('https://other.example.com');
    expect(getSelfUserId()).toBeNull();
    await restoreSelfIdentity(FAKE_ORIGIN);
    expect(getSelfUserId()).toBe(42);
    // Another workspace never sees it, even in memory.
    setSession({ serverOrigin: 'https://other.example.com' });
    expect(getSelfUserId()).toBeNull();
  });

  it('ignores a malformed cached value', async () => {
    for (const raw of ['nope', JSON.stringify({ origin: FAKE_ORIGIN, userId: -1 }), JSON.stringify({ origin: FAKE_ORIGIN, userId: '42' })]) {
      await setSecureItem(SECURE_KEYS.selfUser, raw);
      await restoreSelfIdentity(FAKE_ORIGIN);
      expect(getSelfUserId()).toBeNull();
    }
  });

  it('keeps the cached id when the read fails, and replaces it when the server names someone else', async () => {
    await setSecureItem(SECURE_KEYS.selfUser, JSON.stringify({ origin: FAKE_ORIGIN, userId: 42 }));
    await restoreSelfIdentity(FAKE_ORIGIN);
    const server = fakeServer();
    server.on('GET', '/social/author', { status: 503, body: {} }, { status: 200, body: { ...author, id: '43' } });
    await expect(fetchSelfIdentity(server.client)).rejects.toThrow();
    expect(getSelfUserId()).toBe(42);
    await expect(fetchSelfIdentity(server.client)).resolves.toBe(43);
    expect(getSelfUserId()).toBe(43);
    expect(JSON.parse((await getSecureItem(SECURE_KEYS.selfUser))!)).toMatchObject({ userId: 43 });
  });

  it('does not record an answer without a usable id', async () => {
    const server = fakeServer();
    server.on('GET', '/social/author', { status: 200, body: { ...author, id: '0' } });
    await expect(fetchSelfIdentity(server.client)).resolves.toBeNull();
    expect(getSelfUserId()).toBeNull();
    expect(await getSecureItem(SECURE_KEYS.selfUser)).toBeNull();
  });

  it('drops an answer that arrives after the workspace changed', async () => {
    const server = fakeServer();
    server.on('GET', '/social/author', { status: 200, body: author });
    const pending = fetchSelfIdentity(server.client);
    setSession({ serverOrigin: 'https://other.example.com' });
    await pending;
    setSession({ serverOrigin: FAKE_ORIGIN });
    expect(getSelfUserId()).toBeNull();
    expect(await getSecureItem(SECURE_KEYS.selfUser)).toBeNull();
  });

  it('stores nothing for a session that is ending', async () => {
    const server = fakeServer();
    server.on('GET', '/social/author', { status: 200, body: author });
    const pending = fetchSelfIdentity(server.client);
    // The wipe starts while the read is in flight: its answer must not be stored.
    setSession({ ending: true });
    await expect(pending).resolves.toBe(42);
    expect(server.requests).toHaveLength(1);
    expect(getSelfUserId()).toBeNull();
    expect(await getSecureItem(SECURE_KEYS.selfUser)).toBeNull();
  });

  it('parses only positive integer ids', () => {
    expect(parseUserId('42')).toBe(42);
    expect(parseUserId(42)).toBe(42);
    for (const id of ['0', '-3', '1.5', '', '042', 'abc', null, undefined, 1.5, '99999999999999999']) expect(parseUserId(id)).toBeNull();
  });

  it('is wiped with the session, in memory and in secure storage', async () => {
    const server = fakeServer();
    server.on('GET', '/social/author', { status: 200, body: author });
    await fetchSelfIdentity(server.client);
    await flush();
    expect(getSelfUserId()).toBe(42);
    await endSession({ reason: 'user_logout' });
    setSession({ serverOrigin: FAKE_ORIGIN });
    expect(getSelfUserId()).toBeNull();
    expect(await getSecureItem(SECURE_KEYS.selfUser)).toBeNull();
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
