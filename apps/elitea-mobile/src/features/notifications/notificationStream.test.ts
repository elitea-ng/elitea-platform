import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import { fetchUnseenCount, markAllSeen, notificationTitle } from '@/api/queries/notifications';
import { abortAll, inFlightCount } from '@/lib/abortRegistry';
import {
  isNotificationForProject,
  notificationsStreamUrl,
  openNotificationsStream,
} from '@/features/notifications/notificationStream';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { fakeStreamTransport } from '@/test/fakeStreamTransport';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const ORIGIN = 'https://ai.example.com';
const frame = (projectId: number) =>
  JSON.stringify({ id: 4, uuid: 'n-4', is_seen: false, project_id: projectId, user_id: 2, meta: {}, created_at: 'x', updated_at: null, event_type: 'agent_finished' });

beforeEach(() => {
  __resetSessionForTests();
  abortAll();
  setSession({ hydrated: true, serverOrigin: ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'debug').mockImplementation(() => {});
});

afterEach(() => jest.restoreAllMocks());

describe('notifications stream', () => {
  it('builds the per-project stream URL', () => {
    expect(notificationsStreamUrl(ORIGIN, 7)).toBe(`${ORIGIN}/api/v2/notifications/events/prompt_lib/7`);
    expect(notificationsStreamUrl(ORIGIN, 0)).toBeNull();
    expect(notificationsStreamUrl('not a url', 7)).toBeNull();
  });

  it('accepts only valid notify frames for this project', () => {
    expect(isNotificationForProject({ id: '4', event: 'notifications_notify', data: frame(7) }, 7)).toBe(true);
    expect(isNotificationForProject({ id: '4', event: 'notifications_notify', data: frame(8) }, 7)).toBe(false);
    expect(isNotificationForProject({ id: '3', event: 'notifications_ready', data: '{"cursor":3}' }, 7)).toBe(false);
    expect(isNotificationForProject({ id: '4', event: 'notifications_notify', data: '{oops' }, 7)).toBe(false);
  });

  it('starts at the high-water mark, refreshes on notify, resumes from the cursor and is project-scoped', async () => {
    const { transport, connections } = fakeStreamTransport();
    const onNotification = jest.fn();
    const stream = openNotificationsStream(ORIGIN, 7, onNotification, { transport, appState: null });
    await new Promise((r) => setTimeout(r, 0));
    expect(connections[0].headers['Last-Event-ID']).toBeUndefined();
    connections[0].push(`retry: 2000\nid: 3\nevent: notifications_ready\ndata: {"cursor":3}\n\n`);
    connections[0].push(`id: 4\nevent: notifications_notify\ndata: ${frame(7)}\n\n`);
    await new Promise((r) => setTimeout(r, 0));
    expect(onNotification).toHaveBeenCalledTimes(1);
    expect(stream?.lastEventId).toBe('4');
    expect(inFlightCount()).toBe(1);
    stream?.close();
  });
});

describe('notifications REST', () => {
  it('asks only for the unseen total and marks all as seen', async () => {
    const calls: { url: string; method: string; body: string }[] = [];
    const fetch: BaseFetch = async (input) => {
      calls.push({ url: input.url, method: input.method, body: input.method === 'PUT' ? await input.clone().text() : '' });
      const body = input.method === 'PUT' ? { updated: 2 } : { total: 5 };
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
    };
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    await expect(fetchUnseenCount(7, client)).resolves.toBe(5);
    expect(calls[0].url).toBe(`${ORIGIN}/api/v2/notifications/notifications/prompt_lib/7?only_new=true&only_total=true`);
    await expect(markAllSeen(7, client)).resolves.toBe(2);
    expect(JSON.parse(calls[1].body)).toEqual({ ids: 'all', is_seen: true });
  });

  it('labels event types', () => {
    expect(notificationTitle('agent_finished')).toBe('Agent finished');
    expect(notificationTitle('')).toBe('Notification');
  });
});
