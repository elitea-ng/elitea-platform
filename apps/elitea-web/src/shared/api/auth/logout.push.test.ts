/**
 * Sign-out removes this browser's web push subscription (ADR-0045 decision 6):
 * unsubscribe, DELETE on the server while the session is still valid, and
 * unregister the push worker — best effort, bounded, never blocking the
 * hand-off to the server logout. A fake service worker container; MSW for
 * the DELETE (R-M1).
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../../test/setup';
import { STORAGE_NAMESPACE } from '../../lib/storage';
import { configureGeneratedClient, resetGeneratedClient } from '../generated/mutator';

import { performLogout } from './logout';

const BASE = '/api/v2';
const SUBSCRIPTIONS = `${BASE}/notifications/push_subscriptions/prompt_lib/7`;

interface FakeWorkerState {
  readonly calls: string[];
}

function registration(state: FakeWorkerState, scriptURL: string, endpoint: string | null): ServiceWorkerRegistration {
  const subscription =
    endpoint === null
      ? null
      : {
          endpoint,
          unsubscribe: () => {
            state.calls.push(`unsubscribe ${endpoint}`);
            return Promise.resolve(true);
          },
        };
  return {
    active: { scriptURL },
    waiting: null,
    installing: null,
    pushManager: { getSubscription: () => Promise.resolve(subscription) },
    unregister: () => {
      state.calls.push(`unregister ${scriptURL}`);
      return Promise.resolve(true);
    },
  } as unknown as ServiceWorkerRegistration;
}

function serviceWorker(registrations: ServiceWorkerRegistration[]): ServiceWorkerContainer {
  return { getRegistrations: () => Promise.resolve(registrations) } as unknown as ServiceWorkerContainer;
}

let deleted: unknown[];

beforeEach(() => {
  deleted = [];
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => resetGeneratedClient());

describe('performLogout and web push', () => {
  it('unsubscribes, deletes the endpoint on the server and unregisters the push worker, then hands off', async () => {
    server.use(
      http.delete(SUBSCRIPTIONS, async ({ request }) => {
        deleted.push(await request.json());
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const state: FakeWorkerState = { calls: [] };
    const push = registration(state, 'https://app.example/app/push-sw.js?api=%2Fapi%2Fv2&project=7', 'https://push.example/e1');
    const other = registration(state, 'https://app.example/app/other-sw.js', 'https://push.example/not-ours');
    const redirect = vi.fn();

    await performLogout({ redirect, origin: 'https://app.example', serviceWorker: serviceWorker([push, other]) });

    expect(deleted).toEqual([{ endpoint: 'https://push.example/e1' }]);
    expect(state.calls).toEqual(['unsubscribe https://push.example/e1', 'unregister https://app.example/app/push-sw.js?api=%2Fapi%2Fv2&project=7']);
    expect(redirect).toHaveBeenCalledTimes(1);
  });

  it('still signs out, and still undoes the browser side, when the DELETE fails', async () => {
    server.use(http.delete(SUBSCRIPTIONS, () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    const state: FakeWorkerState = { calls: [] };
    const redirect = vi.fn();

    await performLogout({ redirect, serviceWorker: serviceWorker([registration(state, 'https://app.example/app/push-sw.js?project=7', 'https://push.example/e1')]) });

    expect(state.calls).toEqual(['unsubscribe https://push.example/e1', 'unregister https://app.example/app/push-sw.js?project=7']);
    expect(redirect).toHaveBeenCalledTimes(1);
  });

  it('waits for a hung teardown at most the bound, then signs out anyway', async () => {
    const hung = { getRegistrations: () => new Promise<never>(() => undefined) } as unknown as ServiceWorkerContainer;
    const redirect = vi.fn();
    vi.useFakeTimers();
    try {
      const done = performLogout({ redirect, serviceWorker: hung, pushTeardownWaitMs: 2000 });
      await vi.advanceTimersByTimeAsync(1999);
      expect(redirect).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1);
      await done;
      expect(redirect).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('sweeps the namespace again right before the hand-off, so keys written during the teardown do not survive', async () => {
    const late = `${STORAGE_NAMESPACE}late.key`;
    let resolveList: (list: ServiceWorkerRegistration[]) => void = () => undefined;
    const slow = { getRegistrations: () => new Promise<ServiceWorkerRegistration[]>((resolve) => (resolveList = resolve)) } as unknown as ServiceWorkerContainer;
    let atRedirect: string | null = 'unset';
    const redirect = vi.fn(() => {
      atRedirect = window.localStorage.getItem(late);
    });
    const done = performLogout({ redirect, serviceWorker: slow });
    window.localStorage.setItem(late, 'written after the first sweep');
    resolveList([]);
    await done;
    expect(redirect).toHaveBeenCalledTimes(1);
    expect(atRedirect).toBeNull();
  });

  it('removes a push worker without a subscription and makes no request', async () => {
    const state: FakeWorkerState = { calls: [] };
    const redirect = vi.fn();
    await performLogout({ redirect, serviceWorker: serviceWorker([registration(state, 'https://app.example/push-sw.js?project=7', null)]) });
    expect(deleted).toEqual([]);
    expect(state.calls).toEqual(['unregister https://app.example/push-sw.js?project=7']);
    expect(redirect).toHaveBeenCalledTimes(1);
  });
});
