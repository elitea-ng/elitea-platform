/**
 * notifications.test.ts — contract coverage for the 5 hand-written
 * `/notifications/*` endpoints (manifest API-167..API-171, unit A11).
 *
 * MSW handlers are registered per-test via `server.use()` (never added to
 * the shared `src/test/msw/handlers/` tree, which this unit does not own —
 * same pattern `features/credentials/api/configurations.test.ts` (unit A7)
 * established). Every test asserts the REQUEST the fetcher sent (method,
 * path, query string, body) against the baseline `api/notifications.js`
 * behaviour (API-167..171's acceptance text), not just that a promise
 * resolved.
 */
import { afterEach, describe, expect, it } from 'vitest';
import { http, HttpResponse } from 'msw';

import { server } from '../../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import {
  bulkDeleteNotifications,
  bulkMarkSeenNotifications,
  buildNotificationsListUrl,
  deleteNotification,
  listNotifications,
  readNotification,
} from './notifications';

const BASE = '/api/v2';

interface Captured {
  method: string;
  url: string;
  body: unknown;
}

function capture(): { sink: Captured[] } {
  return { sink: [] };
}

afterEach(() => {
  resetGeneratedClient();
});

function setup(): void {
  configureGeneratedClient({ baseUrl: BASE });
}

describe('buildNotificationsListUrl (API-167 query assembly)', () => {
  it('matches the baseline default query shape (notifications.js:14-24)', () => {
    const url = buildNotificationsListUrl({ projectId: 7 });
    const search = new URL(url, 'http://x').searchParams;
    expect(url.startsWith('/notifications/notifications/prompt_lib/7?')).toBe(true);
    expect(search.get('limit')).toBe('20');
    expect(search.get('offset')).toBe('0');
    expect(search.has('sort_by')).toBe(false);
    expect(search.has('sort_order')).toBe(false);
    expect(search.has('search')).toBe(false);
  });

  it('computes offset from page * pageSize', () => {
    const url = buildNotificationsListUrl({ projectId: 1, page: 2, pageSize: 5 });
    const search = new URL(url, 'http://x').searchParams;
    expect(search.get('offset')).toBe('10');
    expect(search.get('limit')).toBe('5');
  });

  it('spreads extra params (e.g. only_new) before pagination/sort', () => {
    const url = buildNotificationsListUrl({ projectId: 1, params: { only_new: true } });
    const search = new URL(url, 'http://x').searchParams;
    expect(search.get('only_new')).toBe('true');
  });

  it('includes sort_by/sort_order when given', () => {
    const url = buildNotificationsListUrl({ projectId: 1, sortBy: 'created_at', sortOrder: 'desc' });
    const search = new URL(url, 'http://x').searchParams;
    expect(search.get('sort_by')).toBe('created_at');
    expect(search.get('sort_order')).toBe('desc');
  });

  it('omits search when empty, includes it when non-empty', () => {
    expect(new URL(buildNotificationsListUrl({ projectId: 1, search: '' }), 'http://x').searchParams.has('search')).toBe(
      false,
    );
    expect(new URL(buildNotificationsListUrl({ projectId: 1, search: 'hi' }), 'http://x').searchParams.get('search')).toBe(
      'hi',
    );
  });
});

describe('API-167 listNotifications', () => {
  it('GETs the assembled URL and returns { rows, total }', async () => {
    setup();
    const { sink } = capture();
    server.use(
      http.get(`${BASE}/notifications/notifications/prompt_lib/7`, ({ request }) => {
        sink.push({ method: request.method, url: request.url, body: null });
        return HttpResponse.json({ rows: [{ id: 1, event_type: 'rates', created_at: '2026-01-01', is_seen: false }], total: 1 });
      }),
    );
    const result = await listNotifications({ projectId: 7 });
    expect(result.total).toBe(1);
    expect(result.rows).toHaveLength(1);
    expect(sink[0]?.method).toBe('GET');
    expect(sink[0]?.url).toContain('/notifications/notifications/prompt_lib/7');
  });
});

describe('API-168 readNotification (no baseline UI call site — implemented for parity)', () => {
  it('PUTs /notifications/notification/prompt_lib/{projectId}/{id} with no body', async () => {
    setup();
    const { sink } = capture();
    server.use(
      http.put(`${BASE}/notifications/notification/prompt_lib/7/42`, async ({ request }) => {
        sink.push({ method: request.method, url: request.url, body: await request.text() });
        return HttpResponse.json({});
      }),
    );
    await readNotification(7, 42);
    expect(sink[0]?.method).toBe('PUT');
    expect(sink[0]?.body).toBe('');
  });
});

describe('API-169 deleteNotification (no baseline UI call site — implemented for parity)', () => {
  it('DELETEs /notifications/notification/prompt_lib/{projectId}/{id}', async () => {
    setup();
    const { sink } = capture();
    server.use(
      http.delete(`${BASE}/notifications/notification/prompt_lib/7/42`, ({ request }) => {
        sink.push({ method: request.method, url: request.url, body: null });
        return HttpResponse.json({});
      }),
    );
    await deleteNotification(7, 42);
    expect(sink[0]?.method).toBe('DELETE');
  });
});

describe('API-170 / ACT-053 bulkDeleteNotifications', () => {
  it('DELETEs the collection endpoint with { ids } in the body', async () => {
    setup();
    const { sink } = capture();
    server.use(
      http.delete(`${BASE}/notifications/notifications/prompt_lib/7`, async ({ request }) => {
        sink.push({ method: request.method, url: request.url, body: await request.json() });
        return HttpResponse.json({});
      }),
    );
    await bulkDeleteNotifications(7, ['11', '12']);
    expect(sink[0]?.method).toBe('DELETE');
    expect(sink[0]?.body).toEqual({ ids: [11, 12] });
  });
});

describe('API-171 / ACT-054 bulkMarkSeenNotifications', () => {
  it('PUTs the collection endpoint with { ids, is_seen } in the body', async () => {
    setup();
    const { sink } = capture();
    server.use(
      http.put(`${BASE}/notifications/notifications/prompt_lib/7`, async ({ request }) => {
        sink.push({ method: request.method, url: request.url, body: await request.json() });
        return HttpResponse.json({});
      }),
    );
    await bulkMarkSeenNotifications(7, ['11', '12'], true);
    expect(sink[0]?.body).toEqual({ ids: [11, 12], is_seen: true });
  });

  it("accepts the 'all' sentinel (NotificationList.jsx:50-54 parity)", async () => {
    setup();
    const { sink } = capture();
    server.use(
      http.put(`${BASE}/notifications/notifications/prompt_lib/7`, async ({ request }) => {
        sink.push({ method: request.method, url: request.url, body: await request.json() });
        return HttpResponse.json({});
      }),
    );
    await bulkMarkSeenNotifications(7, 'all', true);
    expect(sink[0]?.body).toEqual({ ids: 'all', is_seen: true });
  });
});
