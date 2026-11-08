/**
 * The desktop's view socket against a fake host: the channel's numbered
 * events (in and out of order), sends in order after the open, the close,
 * a refused upgrade's status, and the shared view client driven through it
 * (the fixture-site sequence: hello, frames, take over, input, hand back).
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { openBrowserView, type BrowserViewFailure } from '@/shared/api/browserView/client';
import type { BrowserViewFrameHeader } from '@/shared/api/browserView/protocol';
import { setNativeTransport, type NativeTransport } from '@/shared/api/nativeTransport';

import { HostViewSocket, hostViewSocketFactory, tauriViewSocketHost, type ViewSocketHost } from './hostViewSocket';

const URL_ = 'wss://deploy.example/api/v2/browser_sessions/elitea-br-0123456789abcdef01234567/view';

interface Sent {
  readonly messages: string[];
  readonly close: [number, string] | null;
}

/** A host: `browser_view_open` registers the channel; `emit` plays the host's events into it. */
function fakeHost(options: { openError?: Error } = {}) {
  const callbacks = new Map<number, (envelope: { index: number; message?: unknown; end?: true }) => void>();
  const unregistered: number[] = [];
  const sent: Sent[] = [];
  const opened: { id: number; url: string; events: string }[] = [];
  let nextCallback = 1;
  let index = 0;
  let channel: number | undefined;
  const invoke = vi.fn((command: string, args?: Record<string, unknown>): Promise<unknown> => {
    if (command === 'browser_view_open') {
      opened.push(args as { id: number; url: string; events: string });
      channel = Number(String(args?.['events']).replace('__CHANNEL__:', ''));
      return options.openError === undefined ? Promise.resolve(null) : Promise.reject(options.openError);
    }
    if (command === 'browser_view_send') sent.push({ messages: args?.['messages'] as string[], close: args?.['close'] as [number, string] | null });
    return Promise.resolve(null);
  });
  const host: ViewSocketHost = {
    invoke,
    transformCallback: (callback) => {
      const id = nextCallback;
      nextCallback += 1;
      callbacks.set(id, callback as never);
      return id;
    },
    unregisterCallback: (id) => {
      unregistered.push(id);
      callbacks.delete(id);
    },
  };
  /** Deliver `message` as the channel's next event (or at `at`, out of order). */
  const emit = (message: unknown, at?: number): void => {
    const target = channel === undefined ? undefined : callbacks.get(channel);
    const position = at ?? index;
    index = Math.max(index, position + 1);
    target?.({ index: position, message });
  };
  const end = (): void => {
    const target = channel === undefined ? undefined : callbacks.get(channel);
    target?.({ index, end: true });
    index += 1;
  };
  return { host, invoke, sent, opened, emit, end, unregistered };
}

const flush = async (): Promise<void> => {
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
};

afterEach(() => {
  setNativeTransport(undefined);
  vi.unstubAllGlobals();
});

describe('the host view socket', () => {
  it('opens through the host with a channel and relays text and binary messages', () => {
    const host = fakeHost();
    const socket = new HostViewSocket(URL_, host.host);
    const seen: unknown[] = [];
    socket.onopen = () => seen.push('open');
    socket.onmessage = (event) => seen.push(event.data);
    expect(socket.readyState).toBe(0);
    expect(host.opened).toEqual([{ id: expect.any(Number) as number, url: URL_, events: expect.stringMatching(/^__CHANNEL__:\d+$/) as string }]);
    expect(() => socket.send('{}')).toThrow(DOMException);

    host.emit({ t: 'open' });
    host.emit({ t: 'text', d: '{"type":"hello"}' });
    host.emit(new Uint8Array([0xff, 0xd8]).buffer);
    expect(socket.readyState).toBe(1);
    expect(seen[0]).toBe('open');
    expect(seen[1]).toBe('{"type":"hello"}');
    expect(seen[2]).toBeInstanceOf(Blob);
    expect((seen[2] as Blob).type).toBe('image/jpeg');

    socket.binaryType = 'arraybuffer';
    host.emit(new Uint8Array([1]).buffer);
    expect(seen[3]).toBeInstanceOf(ArrayBuffer);
  });

  it('puts events that arrive out of order back in order', () => {
    const host = fakeHost();
    const socket = new HostViewSocket(URL_, host.host);
    const seen: unknown[] = [];
    socket.onmessage = (event) => seen.push(event.data);
    host.emit({ t: 'text', d: 'second' }, 2);
    host.emit({ t: 'text', d: 'first' }, 1);
    expect(seen).toEqual([]);
    host.emit({ t: 'open' }, 0);
    expect(seen).toEqual(['first', 'second']);
  });

  it('sends in order, batched, one host call at a time, then the close', async () => {
    const host = fakeHost();
    const socket = new HostViewSocket(URL_, host.host);
    host.emit({ t: 'open' });
    socket.send('a');
    socket.send('b');
    socket.close(4000, 'done');
    socket.send('ignored after close');
    socket.close();
    await flush();
    expect(host.sent).toEqual([{ messages: ['a', 'b'], close: [4000, 'done'] }]);
    expect(socket.readyState).toBe(2);
    const closes: CloseEvent[] = [];
    socket.onclose = (event) => closes.push(event);
    host.emit({ t: 'close', code: 4000, reason: 'done', status: null });
    expect(socket.readyState).toBe(3);
    expect(closes.map((event) => [event.code, event.reason, event.wasClean])).toEqual([[4000, 'done', true]]);
    expect(socket.refusedStatus).toBeUndefined();
    // The callback stays until the channel's end, which Tauri sends after the last message.
    expect(host.unregistered).toHaveLength(0);
    host.end();
    expect(host.unregistered).toHaveLength(1);
    // Nothing after the close.
    host.emit({ t: 'close', code: 1000, reason: '', status: null });
    expect(closes).toHaveLength(1);
  });

  it('splits a long burst into batches the host accepts', async () => {
    const host = fakeHost();
    const socket = new HostViewSocket(URL_, host.host);
    host.emit({ t: 'open' });
    for (let i = 0; i < 300; i += 1) socket.send(String(i));
    await flush();
    expect(host.sent.map((call) => call.messages.length)).toEqual([256, 44]);
    expect(host.sent.flatMap((call) => call.messages)).toEqual(Array.from({ length: 300 }, (_, i) => String(i)));
  });

  it('closes before the open with nothing but the close', async () => {
    const host = fakeHost();
    const socket = new HostViewSocket(URL_, host.host);
    socket.close();
    await flush();
    expect(host.sent).toEqual([{ messages: [], close: [1000, ''] }]);
    // An open that arrives after the close is ignored.
    host.emit({ t: 'open' });
    expect(socket.readyState).toBe(2);
  });

  it("reports a refused upgrade's status", () => {
    const host = fakeHost();
    const socket = new HostViewSocket(URL_, host.host);
    const closes: number[] = [];
    socket.onclose = (event) => closes.push(event.code);
    host.emit({ t: 'close', code: 1006, reason: '', status: 404 });
    expect(socket.refusedStatus).toBe(404);
    expect(closes).toEqual([1006]);
  });

  it('reads a malformed close and a channel that ended as abnormal', () => {
    const host = fakeHost();
    const socket = new HostViewSocket(URL_, host.host);
    const closes: [number, string][] = [];
    socket.onclose = (event) => closes.push([event.code, event.reason]);
    host.emit({ t: 'open' });
    host.emit({ t: 'text', d: 7 });
    host.emit(null);
    host.emit({ t: 'close', code: 'x', reason: 1 });
    expect(closes).toEqual([[1006, '']]);
    expect(socket.refusedStatus).toBeUndefined();

    const other = fakeHost();
    const second = new HostViewSocket(URL_, other.host);
    const ended: number[] = [];
    second.onclose = (event) => ended.push(event.code);
    other.end();
    expect(ended).toEqual([1006]);
    // A binary message on a socket that is not open is dropped.
    other.emit(new ArrayBuffer(1));
  });

  it.each([403, 429])('keeps the status of a view the host refused (%i), though the channel ends before the call settles', async (status) => {
    // Tauri's order for a refused open: the close on the channel, the
    // channel's end, then the call resolves.
    const host = fakeHost();
    const socket = new HostViewSocket(URL_, host.host);
    const closes: number[] = [];
    socket.onclose = (event) => closes.push(event.code);
    host.emit({ t: 'close', code: 1006, reason: '', status });
    host.end();
    await flush();
    expect(closes).toEqual([1006]);
    expect(socket.refusedStatus).toBe(status);
  });

  it('reads an open the IPC itself rejected as unreachable', async () => {
    const refused = fakeHost({ openError: Object.assign(new Error('no'), { code: 'invalid_request' }) });
    const other = new HostViewSocket(URL_, refused.host);
    await flush();
    expect(other.readyState).toBe(3);
    expect(other.refusedStatus).toBeUndefined();
    expect(refused.unregistered).toHaveLength(1);
  });

  it('ignores a send whose socket the host already closed', async () => {
    const host = fakeHost();
    const socket = new HostViewSocket(URL_, host.host);
    host.emit({ t: 'open' });
    host.invoke.mockImplementation(() => Promise.reject(new Error('unknown_view')));
    socket.send('x');
    await flush();
    expect(socket.readyState).toBe(1);
  });

  it('drops what was queued when the host closes first, and a frame before the open', async () => {
    const host = fakeHost();
    const socket = new HostViewSocket(URL_, host.host);
    const seen: unknown[] = [];
    socket.onmessage = (event) => seen.push(event.data);
    host.emit(new ArrayBuffer(2));
    host.emit({ t: 'open' });
    socket.send('queued');
    host.emit({ t: 'close', code: 1001, reason: '', status: null });
    await flush();
    expect(seen).toEqual([]);
    expect(host.sent).toEqual([]);
  });

  it('closes once when the host refuses the open after its close event', async () => {
    const host = fakeHost({ openError: new Error('late') });
    const socket = new HostViewSocket(URL_, host.host);
    const closes: number[] = [];
    socket.onclose = (event) => closes.push(event.code);
    host.emit({ t: 'close', code: 1006, reason: '', status: 503 });
    await flush();
    expect(closes).toEqual([1006]);
    expect(socket.refusedStatus).toBe(503);
  });

  it('works without an unregisterCallback', () => {
    const host = fakeHost();
    const bare: ViewSocketHost = { invoke: host.invoke, transformCallback: (callback) => host.host.transformCallback(callback) };
    const socket = new HostViewSocket(URL_, bare);
    host.emit({ t: 'close', code: 1000, reason: '', status: null });
    expect(socket.readyState).toBe(3);
  });
});

describe('the Tauri host', () => {
  it('is the webview internals, bound; absent outside the webview', () => {
    expect(tauriViewSocketHost()).toBeUndefined();
    const internals = {
      invoke: vi.fn(() => Promise.resolve(null)),
      transformCallback: vi.fn(() => 7),
      unregisterCallback: vi.fn(),
    };
    vi.stubGlobal('__TAURI_INTERNALS__', internals);
    const host = tauriViewSocketHost();
    expect(host).toBeDefined();
    const socket = hostViewSocketFactory(host as ViewSocketHost)(URL_);
    expect(internals.invoke).toHaveBeenCalledWith('browser_view_open', { id: expect.any(Number) as number, url: URL_, events: '__CHANNEL__:7' });
    expect(socket.readyState).toBe(0);

    vi.stubGlobal('__TAURI_INTERNALS__', { invoke: vi.fn(), transformCallback: vi.fn() });
    expect(tauriViewSocketHost()).not.toHaveProperty('unregisterCallback');
  });
});

describe('the shared view client on the host socket', () => {
  const NATIVE: NativeTransport = { accessToken: () => Promise.resolve('t'), refresh: () => Promise.resolve('refreshed'), signOut: () => undefined };

  function client(host: ReturnType<typeof fakeHost>) {
    const frames: BrowserViewFrameHeader[] = [];
    const failures: [BrowserViewFailure, boolean][] = [];
    const ends: string[] = [];
    const onUpgradeRequired = vi.fn();
    setNativeTransport({ ...NATIVE, origin: 'https://deploy.example', createWebSocket: hostViewSocketFactory(host.host), onUpgradeRequired });
    const view = openBrowserView(
      'elitea-br-0123456789abcdef01234567',
      { onFrame: (header) => frames.push(header), onFailure: (failure, retryable) => failures.push([failure, retryable]), onEnd: (code) => ends.push(code) },
      { url: URL_ },
    );
    return { view, frames, failures, ends, onUpgradeRequired };
  }

  it('runs the fixture-site sequence: watch, take over, type, hand back', async () => {
    const host = fakeHost();
    const { view, frames, ends } = client(host);
    host.emit({ t: 'open' });
    host.emit({ t: 'text', d: JSON.stringify({ type: 'hello', session_id: 's', can_control: true, lingering: false }) });
    host.emit({ t: 'text', d: JSON.stringify({ type: 'frame', seq: 1, width: 1280, height: 800, bytes: 2, url: 'https://fixture.test/form', title: 'Form', control: 'agent' }) });
    host.emit(new Uint8Array([0xff, 0xd8]).buffer);
    expect(frames).toEqual([expect.objectContaining({ seq: 1, url: 'https://fixture.test/form', control: 'agent' })]);
    view.ack(1);
    view.takeOver();
    view.pointer('down', 10, 20, 1, 1, 0);
    view.key('char', 'a', 'KeyA', 'a', 0);
    view.handBack('signed in, continue');
    host.emit({ t: 'text', d: JSON.stringify({ type: 'end', code: 'session_destroyed' }) });
    await flush();
    const up = host.sent.flatMap((call) => call.messages).map((message) => JSON.parse(message) as { type: string });
    expect(up.map((message) => message.type)).toEqual(['ack', 'take_over', 'pointer', 'key', 'hand_back']);
    expect(up[4]).toEqual({ type: 'hand_back', note: 'signed in, continue' });
    expect(ends).toEqual(['session_destroyed']);
  });

  it.each<[number, BrowserViewFailure, boolean]>([
    [403, 'forbidden', false],
    [404, 'not_found', false],
    [410, 'ended', false],
    [429, 'busy', true],
    [502, 'unavailable', true],
    [401, 'signed_out', false],
    [426, 'upgrade_required', false],
  ])('reads a refused upgrade (%i) from the host, without asking the route again', (status, failure, retryable) => {
    const host = fakeHost();
    const fetchSpy = vi.fn();
    vi.stubGlobal('fetch', fetchSpy);
    const result = client(host);
    host.emit({ t: 'close', code: 1006, reason: '', status });
    expect(result.failures).toEqual([[failure, retryable]]);
    expect(result.onUpgradeRequired).toHaveBeenCalledTimes(status === 426 ? 1 : 0);
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('reads a view the host refused as final (403) or busy (429), in the order Tauri delivers it', async () => {
    for (const [status, failure, retryable] of [
      [403, 'forbidden', false],
      [429, 'busy', true],
    ] as const) {
      const host = fakeHost();
      const fetchSpy = vi.fn();
      vi.stubGlobal('fetch', fetchSpy);
      const result = client(host);
      host.emit({ t: 'close', code: 1006, reason: '', status });
      host.end();
      await flush();
      expect(result.failures).toEqual([[failure, retryable]]);
      expect(fetchSpy).not.toHaveBeenCalled();
    }
  });

  it('reads an unreachable deployment as transient', async () => {
    const host = fakeHost();
    const result = client(host);
    host.emit({ t: 'close', code: 1006, reason: '', status: null });
    await vi.waitFor(() => expect(result.failures).toEqual([['unavailable', true]]));
  });
});
