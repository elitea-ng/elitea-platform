/** The opt-in switch: a fake browser (PushManager, Notification) and MSW for Main's push routes (R-M1). */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';

import { server } from '../../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { createFakePushBrowser, type FakePushBrowser } from '../lib/webPush.fake';
import { WebPushToggle } from './WebPushToggle';

const BASE = '/api/v2';
const CONFIG = `${BASE}/notifications/push_config/prompt_lib/7`;
const SUBSCRIPTIONS = `${BASE}/notifications/push_subscriptions/prompt_lib/7`;

const KEY = 'BAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4fICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8';

function mount(browser: FakePushBrowser): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <div data-testid="host">
        <WebPushToggle
          projectId="7"
          env={browser.env}
        />
      </div>
    </QueryClientProvider>,
  );
}

const toggle = (): HTMLInputElement => screen.getByRole('switch', { name: /Web push in this browser/ });

interface Seen {
  configReads: number;
  posted: unknown[];
  deleted: unknown[];
}

let seen: Seen;

function serve(config: { enabled: boolean; public_key: string }, post: () => Response = () => HttpResponse.json({ id: 's-1' }, { status: 201 })): void {
  server.use(
    http.get(CONFIG, () => {
      seen.configReads += 1;
      return HttpResponse.json(config);
    }),
    http.post(SUBSCRIPTIONS, async ({ request }) => {
      seen.posted.push(await request.json());
      return post();
    }),
    http.delete(SUBSCRIPTIONS, async ({ request }) => {
      seen.deleted.push(await request.json());
      return new HttpResponse(null, { status: 204 });
    }),
  );
}

beforeEach(() => {
  seen = { configReads: 0, posted: [], deleted: [] };
  configureGeneratedClient({ baseUrl: BASE });
  serve({ enabled: true, public_key: KEY });
});
afterEach(() => resetGeneratedClient());

describe('WebPushToggle', () => {
  it('is hidden when the deployment has web push off', async () => {
    serve({ enabled: false, public_key: '' });
    mount(createFakePushBrowser());
    await waitFor(() => expect(seen.configReads).toBe(1));
    expect(screen.getByTestId('host')).toBeEmptyDOMElement();
  });

  it('is hidden in a browser without push (unsupported)', async () => {
    mount(createFakePushBrowser({ supported: false }));
    await waitFor(() => expect(seen.configReads).toBe(1));
    expect(screen.getByTestId('host')).toBeEmptyDOMElement();
  });

  it('shows the blocked state, disabled, when the browser refused notifications', async () => {
    const browser = createFakePushBrowser();
    browser.permission = 'denied';
    mount(browser);
    await waitFor(() => expect(toggle()).toBeDisabled());
    expect(toggle()).not.toBeChecked();
    expect(screen.getByText(/Your browser blocks notifications from this site/)).toBeInTheDocument();
  });

  it('turns on: asks the browser, subscribes and posts the subscription', async () => {
    const browser = createFakePushBrowser();
    mount(browser);
    await waitFor(() => expect(toggle()).not.toBeChecked());
    fireEvent.click(toggle());
    await waitFor(() => expect(toggle()).toBeChecked());
    expect(browser.calls).toEqual(['requestPermission', 'register', 'subscribe']);
    expect(seen.posted).toEqual([{ endpoint: 'https://push.example/ep-1', keys: { p256dh: 'p-key', auth: 'a-key' } }]);
  });

  it('turns off: deletes the endpoint and unsubscribes', async () => {
    const browser = createFakePushBrowser();
    browser.permission = 'granted';
    browser.registered = { url: '/push-sw.js', scope: '/' };
    browser.subscription = { endpoint: 'https://push.example/x', key: null };
    mount(browser);
    await waitFor(() => expect(toggle()).toBeChecked());
    fireEvent.click(toggle());
    await waitFor(() => expect(toggle()).not.toBeChecked());
    expect(seen.deleted).toEqual([{ endpoint: 'https://push.example/x' }]);
    expect(browser.subscription).toBeNull();
  });

  it('says so when Main refuses the subscription, and stays off', async () => {
    serve({ enabled: true, public_key: KEY }, () => HttpResponse.json({ error: 'endpoint must be https' }, { status: 422 }));
    mount(createFakePushBrowser());
    await waitFor(() => expect(toggle()).not.toBeChecked());
    fireEvent.click(toggle());
    await waitFor(() => expect(screen.getByText('Web push did not change. Try again.')).toBeInTheDocument());
    expect(toggle()).not.toBeChecked();
  });

  it('is the browser only: hidden in the desktop app', async () => {
    (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ = { invoke: vi.fn() };
    try {
      mount(createFakePushBrowser());
      await Promise.resolve();
      expect(screen.getByTestId('host')).toBeEmptyDOMElement();
      expect(seen.configReads).toBe(0);
    } finally {
      delete (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
    }
  });
});
