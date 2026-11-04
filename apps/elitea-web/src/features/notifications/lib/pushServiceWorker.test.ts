/**
 * `public/push-sw.js` run with a fake `self`: the push, click and
 * subscription-change logic of the web push service worker (U3 contract §4).
 */
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { runInNewContext } from 'node:vm';

import { describe, expect, it, vi } from 'vitest';

const SOURCE = readFileSync(resolve(import.meta.dirname, '../../../../public/push-sw.js'), 'utf8');
const SCOPE = 'https://elitea.example/app/';

interface FakeClient {
  url: string;
  focused: boolean;
  focus: ReturnType<typeof vi.fn>;
  navigate: ReturnType<typeof vi.fn>;
}

type Listener = (event: Record<string, unknown>) => void;

function client(url: string, focused = false): FakeClient {
  const fake: FakeClient = { url, focused, focus: vi.fn(), navigate: vi.fn().mockResolvedValue(undefined) };
  fake.focus.mockResolvedValue(fake);
  return fake;
}

function worker(clients: FakeClient[] = []) {
  const listeners = new Map<string, Listener>();
  const fake = {
    location: { href: `${SCOPE}push-sw.js?api=%2Fapi%2Fv2&project=7` },
    addEventListener: (type: string, listener: Listener) => listeners.set(type, listener),
    skipWaiting: vi.fn(),
    fetch: vi.fn().mockResolvedValue({ ok: true }),
    clients: {
      matchAll: vi.fn().mockResolvedValue(clients),
      openWindow: vi.fn().mockResolvedValue(null),
      claim: vi.fn().mockResolvedValue(undefined),
    },
    registration: {
      scope: SCOPE,
      showNotification: vi.fn().mockResolvedValue(undefined),
      pushManager: { subscribe: vi.fn() },
    },
  };
  // A plain script: its free names are `self` and the web globals it uses.
  runInNewContext(SOURCE, { self: fake, URL }, { filename: 'push-sw.js' });
  const fire = async (type: string, event: Record<string, unknown>): Promise<void> => {
    let work: Promise<unknown> = Promise.resolve();
    listeners.get(type)?.({ ...event, waitUntil: (promise: Promise<unknown>) => (work = promise) });
    await work;
  };
  const push = (payload: unknown) => fire('push', { data: { json: () => payload } });
  return { fake, fire, push };
}

describe('push-sw.js: push', () => {
  it.each([
    ['completed', 'Run finished', 'A run is done.'],
    ['failed', 'Run failed', 'A run stopped with an error.'],
    ['waiting', 'Run needs you', 'A run waits for your answer.'],
    ['limit_reached', 'Run stopped at its limit', 'A run reached its chain limit.'],
  ])('run_finished %s shows "%s"', async (outcome, title, body) => {
    const { fake, push } = worker();
    await push({ type: 'run_finished', notification_id: 123, project_id: 1, conversation_uuid: 'c-1', outcome });
    expect(fake.registration.showNotification).toHaveBeenCalledWith(title, {
      body,
      tag: 'elitea-123',
      data: { url: `${SCOPE}1/chat?conversation=c-1` },
    });
  });

  it('shows "Run needs you" for an approval request', async () => {
    const { fake, push } = worker();
    await push({ type: 'agent_approval_required', notification_id: 5, project_id: 1, conversation_uuid: 'c-2' });
    expect(fake.registration.showNotification).toHaveBeenCalledWith('Run needs you', expect.objectContaining({ body: 'A run waits for your answer.', tag: 'elitea-5' }));
  });

  it('shows nothing while a focused window shows that conversation', async () => {
    for (const url of [`${SCOPE}chat/c-1`, `${SCOPE}1/chat?conversation=c-1`]) {
      const { fake, push } = worker([client(url, true)]);
      await push({ type: 'run_finished', notification_id: 1, project_id: 1, conversation_uuid: 'c-1', outcome: 'completed' });
      expect(fake.registration.showNotification).not.toHaveBeenCalled();
    }
  });

  it('still shows it when that window is not focused, or shows another conversation', async () => {
    const { fake, push } = worker([client(`${SCOPE}chat/c-1`, false), client(`${SCOPE}chat/c-9`, true)]);
    await push({ type: 'run_finished', notification_id: 1, project_id: 1, conversation_uuid: 'c-1', outcome: 'completed' });
    expect(fake.registration.showNotification).toHaveBeenCalledTimes(1);
  });

  it('ignores an unknown type and a payload that is not JSON, and never shows fetched content', async () => {
    const { fake, push, fire } = worker();
    await push({ type: 'something_else', title: 'secret' });
    await fire('push', {
      data: {
        json: () => {
          throw new SyntaxError('not json');
        },
      },
    });
    await fire('push', { data: null });
    expect(fake.registration.showNotification).not.toHaveBeenCalled();
    expect(fake.fetch).not.toHaveBeenCalled();
  });

  it('opens the app root when the payload has no conversation', async () => {
    const { fake, push } = worker();
    await push({ type: 'run_finished', notification_id: 2, outcome: 'completed' });
    expect(fake.registration.showNotification).toHaveBeenCalledWith('Run finished', expect.objectContaining({ data: { url: SCOPE } }));
  });
});

describe('push-sw.js: notification click', () => {
  const notification = () => ({ close: vi.fn(), data: { url: `${SCOPE}1/chat?conversation=c-1` } });

  it('focuses an open app window and navigates it to the conversation', async () => {
    const open = client(`${SCOPE}agents`);
    const { fake, fire } = worker([client('https://other.example/'), open]);
    const clicked = notification();
    await fire('notificationclick', { notification: clicked });
    expect(clicked.close).toHaveBeenCalled();
    expect(open.focus).toHaveBeenCalled();
    expect(open.navigate).toHaveBeenCalledWith(`${SCOPE}1/chat?conversation=c-1`);
    expect(fake.clients.openWindow).not.toHaveBeenCalled();
  });

  it('opens a new window when no app window is open', async () => {
    const { fake, fire } = worker([]);
    await fire('notificationclick', { notification: notification() });
    expect(fake.clients.openWindow).toHaveBeenCalledWith(`${SCOPE}1/chat?conversation=c-1`);
  });

  it('opens a new window when the open one cannot be navigated', async () => {
    const open = client(`${SCOPE}chat`);
    open.navigate.mockRejectedValue(new TypeError('not controlled'));
    const { fake, fire } = worker([open]);
    await fire('notificationclick', { notification: notification() });
    expect(fake.clients.openWindow).toHaveBeenCalledWith(`${SCOPE}1/chat?conversation=c-1`);
  });
});

describe('push-sw.js: pushsubscriptionchange', () => {
  const subscription = (endpoint: string) => ({ endpoint, options: { userVisibleOnly: true }, toJSON: () => ({ endpoint, keys: { p256dh: 'p', auth: 'a' } }) });

  it('subscribes again with the old options, posts the new endpoint and deletes the old one', async () => {
    const { fake, fire } = worker();
    fake.registration.pushManager.subscribe.mockResolvedValue(subscription('https://push.example/new'));
    await fire('pushsubscriptionchange', { oldSubscription: subscription('https://push.example/old') });
    expect(fake.registration.pushManager.subscribe).toHaveBeenCalledWith({ userVisibleOnly: true });
    const url = '/api/v2/notifications/push_subscriptions/prompt_lib/7';
    expect(fake.fetch.mock.calls).toEqual([
      [url, expect.objectContaining({ method: 'POST', credentials: 'include', body: JSON.stringify({ endpoint: 'https://push.example/new', keys: { p256dh: 'p', auth: 'a' } }) })],
      [url, expect.objectContaining({ method: 'DELETE', body: JSON.stringify({ endpoint: 'https://push.example/old' }) })],
    ]);
  });

  it('uses the new subscription the browser hands over', async () => {
    const { fake, fire } = worker();
    await fire('pushsubscriptionchange', { newSubscription: subscription('https://push.example/given') });
    expect(fake.registration.pushManager.subscribe).not.toHaveBeenCalled();
    expect(fake.fetch).toHaveBeenCalledTimes(1);
  });
});
