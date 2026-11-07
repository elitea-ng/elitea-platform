/**
 * `openBrowserView` with its defaults: the real refusal diagnosis over the
 * network boundary (MSW answers the plain GET), the browser's own
 * `WebSocket`, the configured URL and the desktop host's socket. The pairing
 * and bounds are in client.test.ts.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { setNativeTransport, type NativeTransport } from '@/shared/api/nativeTransport';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { openBrowserView, type BrowserViewFailure } from './client';
import { FakeBrowserViewSocket, fakeSocketFactory } from './testing';

const SESSION = 'elitea-br-0123456789abcdef01234567';
const BASE = '/api/v2';
const VIEW_PATH = `/browser_sessions/${SESSION}/view`;
const VIEW_ROUTE = `${BASE}${VIEW_PATH}`;
const NATIVE: NativeTransport = { accessToken: () => Promise.resolve('t'), refresh: () => Promise.resolve('refreshed'), signOut: () => undefined };
const globals = globalThis as unknown as Record<string, unknown>;

type Diagnosis = { failure: BrowserViewFailure; retryable: boolean };

/** Opens a view on a fake socket, closes it before `hello`, and answers what the client concluded. */
async function refusedOpen(): Promise<Diagnosis> {
  const fake = fakeSocketFactory();
  const failures: Diagnosis[] = [];
  openBrowserView(SESSION, { onFailure: (failure, retryable) => failures.push({ failure, retryable }) }, { socketFactory: fake.factory, url: 'ws://host/view' });
  fake.last().serverClose(1006);
  await vi.waitFor(() => expect(failures).toHaveLength(1));
  return failures[0] as Diagnosis;
}

/** The view route answers a plain GET with `status`; counts the requests. */
function answer(status: number): { readonly requests: Request[] } {
  const requests: Request[] = [];
  server.use(
    http.get(VIEW_ROUTE, ({ request }) => {
      requests.push(request);
      return new HttpResponse(status === 426 ? 'a browser view is a WebSocket' : 'refused', { status, headers: { 'Content-Type': 'text/plain' } });
    }),
  );
  return { requests };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
  setNativeTransport(undefined);
  vi.unstubAllGlobals();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

describe('the refusal diagnosis', () => {
  it.each<[number, Diagnosis]>([
    [401, { failure: 'signed_out', retryable: false }],
    [403, { failure: 'forbidden', retryable: false }],
    [404, { failure: 'not_found', retryable: false }],
    [410, { failure: 'ended', retryable: false }],
    [429, { failure: 'busy', retryable: true }],
    [502, { failure: 'unavailable', retryable: true }],
    [503, { failure: 'unavailable', retryable: true }],
  ])('reads %i as its failure', async (status, expected) => {
    const { requests } = answer(status);
    expect(await refusedOpen()).toEqual(expected);
    expect(requests.map((request) => request.method)).toEqual(['GET']);
  });

  it('reads an answered GET (426: the view would be admitted) as transient', async () => {
    answer(426);
    expect(await refusedOpen()).toEqual({ failure: 'unavailable', retryable: true });
  });

  it('reads a network failure as transient', async () => {
    server.use(http.get(VIEW_ROUTE, () => HttpResponse.error()));
    expect(await refusedOpen()).toEqual({ failure: 'unavailable', retryable: true });
  });

  it('reads a failure that is not an API error as transient', async () => {
    // No client configured: eliteaFetch throws a plain Error, not an EliteaApiError.
    resetGeneratedClient();
    expect(await refusedOpen()).toEqual({ failure: 'unavailable', retryable: true });
  });

  it('never asks over HTTP on the desktop, whose host reports its own refusal', async () => {
    const { requests } = answer(404);
    setNativeTransport({ ...NATIVE, createWebSocket: fakeSocketFactory().factory });
    expect(await refusedOpen()).toEqual({ failure: 'unavailable', retryable: true });
    expect(requests).toHaveLength(0);
  });
});

describe('the default socket and URL', () => {
  it("opens the browser's WebSocket on the configured API base", () => {
    const made: FakeBrowserViewSocket[] = [];
    vi.stubGlobal('WebSocket', function WebSocket(url: string) {
      const socket = new FakeBrowserViewSocket(url);
      made.push(socket);
      return socket;
    });
    resetConfigForTests();
    globals['elitea_ui_config'] = { vite_server_url: 'https://api.example/api/v2', vite_base_uri: '/', vite_public_project_id: '1' };
    openBrowserView(SESSION, {});
    expect(made.map((socket) => socket.url)).toEqual([`wss://api.example/api/v2${VIEW_PATH}`]);
  });

  it('falls back to /api/v2 on the page origin without a config', () => {
    const fake = fakeSocketFactory();
    resetConfigForTests();
    delete globals['elitea_ui_config'];
    openBrowserView(SESSION, {}, { socketFactory: fake.factory });
    const expected = new URL(`/api/v2${VIEW_PATH}`, globalThis.location.href);
    expected.protocol = expected.protocol === 'https:' ? 'wss:' : 'ws:';
    expect(fake.last().url).toBe(expected.toString());
  });

  it("uses the desktop host's socket", () => {
    const fake = fakeSocketFactory();
    setNativeTransport({ ...NATIVE, origin: 'https://deploy.example', createWebSocket: fake.factory });
    openBrowserView(SESSION, {}, { url: 'wss://deploy.example/view' });
    expect(fake.last().url).toBe('wss://deploy.example/view');
  });
});

describe('the fake view socket', () => {
  it('fills a frame from its defaults, and says when no socket was opened', () => {
    const fake = fakeSocketFactory();
    expect(() => fake.last()).toThrow('no socket was opened');
    const headers: unknown[] = [];
    openBrowserView(SESSION, { onFrame: (header) => headers.push(header) }, { socketFactory: fake.factory, url: 'ws://host/view' });
    fake.last().hello(true);
    fake.last().frame(1, { url: 'https://shop.test/', title: 'Shop', control: 'user', width: 640, height: 400 });
    fake.last().frame(2);
    expect(headers).toEqual([
      expect.objectContaining({ seq: 1, url: 'https://shop.test/', title: 'Shop', control: 'user', width: 640, height: 400 }),
      expect.objectContaining({ seq: 2, url: 'https://example.test/', title: 'Example', control: 'agent', width: 1280, height: 800 }),
    ]);
  });
});

describe('a binary frame', () => {
  it('wraps an ArrayBuffer JPEG in a Blob', () => {
    const fake = fakeSocketFactory();
    const frames: Blob[] = [];
    openBrowserView(SESSION, { onFrame: (_header, jpeg) => frames.push(jpeg) }, { socketFactory: fake.factory, url: 'ws://host/view' });
    const socket = fake.last();
    socket.hello(false);
    socket.text({ type: 'frame', seq: 1, width: 2, height: 2, bytes: 4, url: '', title: '', control: 'agent' });
    socket.onmessage?.(new MessageEvent('message', { data: new Uint8Array([0xff, 0xd8, 0xff, 0xd9]).buffer }));
    expect(frames).toHaveLength(1);
    expect(frames[0]).toBeInstanceOf(Blob);
    expect(frames[0]?.type).toBe('image/jpeg');
    expect(frames[0]?.size).toBe(4);
  });
});
