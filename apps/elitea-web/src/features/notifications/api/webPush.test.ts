/** The web push routes over the real `eliteaFetch` client, MSW at the network boundary (R-M1). */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { server } from '../../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { deletePushSubscription, getPushConfig, postPushSubscription } from './webPush';

const BASE = '/api/v2';

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('web push routes', () => {
  it('reads the push config out of the envelope eliteaFetch returns', async () => {
    server.use(http.get(`${BASE}/notifications/push_config/prompt_lib/7`, () => HttpResponse.json({ enabled: true, public_key: 'BKey' })));
    await expect(getPushConfig(7)).resolves.toEqual({ enabled: true, public_key: 'BKey' });
  });

  it('posts a subscription as JSON and returns its id', async () => {
    let posted: unknown;
    server.use(
      http.post(`${BASE}/notifications/push_subscriptions/prompt_lib/7`, async ({ request }) => {
        posted = await request.json();
        return HttpResponse.json({ id: 's-1' }, { status: 201 });
      }),
    );
    const body = { endpoint: 'https://push.example/e', keys: { p256dh: 'p', auth: 'a' } };
    await expect(postPushSubscription('7', body)).resolves.toEqual({ id: 's-1' });
    expect(posted).toEqual(body);
  });

  it('deletes by endpoint', async () => {
    let deleted: unknown;
    server.use(
      http.delete(`${BASE}/notifications/push_subscriptions/prompt_lib/7`, async ({ request }) => {
        deleted = await request.json();
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await deletePushSubscription('7', 'https://push.example/e');
    expect(deleted).toEqual({ endpoint: 'https://push.example/e' });
  });
});
