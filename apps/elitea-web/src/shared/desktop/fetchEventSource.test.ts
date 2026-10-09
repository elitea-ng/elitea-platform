import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { FetchEventSource, type FetchEventSourceDeps } from './fetchEventSource';

const enc = new TextEncoder();

/** A controllable event-stream response. */
function stream(status = 200, contentType = 'text/event-stream') {
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      controller = c;
    },
  });
  return {
    response: new Response(body, { status, headers: { 'content-type': contentType } }),
    send: (text: string) => controller.enqueue(enc.encode(text)),
    end: () => controller.close(),
  };
}

function setup(fetchImpl: typeof fetch, overrides: Partial<FetchEventSourceDeps['transport']> = {}) {
  const transport = {
    accessToken: vi.fn<() => Promise<string | undefined>>().mockResolvedValue('tok'),
    refresh: vi.fn<(failed: string | undefined) => Promise<'refreshed' | 'ended' | 'unavailable'>>().mockResolvedValue('refreshed'),
    signOut: vi.fn<(reason: 'refresh_failed' | 'device_revoked') => void>(),
    headers: { 'X-Client-Version': '9.9.9' },
    fetch: fetchImpl,
    ...overrides,
  };
  return { transport, deps: { transport, defaultRetryMs: 50 } satisfies FetchEventSourceDeps };
}

beforeEach(() => vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] }));
afterEach(() => vi.useRealTimers());

describe('FetchEventSource', () => {
  it('sends the bearer, accepts event-stream, opens, and delivers named events', async () => {
    const s = stream();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(s.response);
    const { deps } = setup(fetchMock);
    const es = new FetchEventSource('https://h.example/api/v2/stream', deps);
    const opened = vi.fn();
    const got: MessageEvent[] = [];
    es.addEventListener('open', opened);
    es.addEventListener('ping', (e) => got.push(e));

    await vi.advanceTimersByTimeAsync(0);
    s.send('id: 3\nevent: ping\ndata: {"a":1}\n\n: hb\n\n');
    await vi.advanceTimersByTimeAsync(0);

    const init = fetchMock.mock.calls[0]?.[1];
    const headers = new Headers(init?.headers);
    expect(headers.get('Authorization')).toBe('Bearer tok');
    expect(headers.get('Accept')).toBe('text/event-stream');
    expect(headers.get('X-Client-Version')).toBe('9.9.9');
    expect(init?.credentials).toBe('omit');
    expect(init?.redirect).toBe('error');
    expect(es.readyState).toBe(FetchEventSource.OPEN);
    expect(opened).toHaveBeenCalledTimes(1);
    expect(got).toHaveLength(1);
    expect(got[0]?.data).toBe('{"a":1}');
    expect(got[0]?.lastEventId).toBe('3');
    es.close();
  });

  it('a non-200 fails the connection permanently: error, CLOSED, no retry', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response('busy', { status: 429 }));
    const es = new FetchEventSource('https://h.example/s', setup(fetchMock).deps);
    const onError = vi.fn();
    es.addEventListener('error', onError);

    await vi.advanceTimersByTimeAsync(10_000);

    expect(es.readyState).toBe(FetchEventSource.CLOSED);
    expect(onError).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('a wrong content type is also permanent', async () => {
    const s = stream(200, 'text/html');
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(s.response);
    const es = new FetchEventSource('https://h.example/s', setup(fetchMock).deps);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(es.readyState).toBe(FetchEventSource.CLOSED);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('a clean end reconnects as CONNECTING with Last-Event-ID', async () => {
    const first = stream();
    const second = stream();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValueOnce(first.response).mockResolvedValueOnce(second.response);
    const es = new FetchEventSource('https://h.example/s', setup(fetchMock).deps);
    const onError = vi.fn<(e: Event) => void>();
    es.addEventListener('error', onError);

    await vi.advanceTimersByTimeAsync(0);
    first.send('id: 41\ndata: x\n\n');
    first.end();
    await vi.advanceTimersByTimeAsync(0);
    expect(es.readyState).toBe(FetchEventSource.CONNECTING);
    expect(onError).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(60);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(new Headers(fetchMock.mock.calls[1]?.[1]?.headers).get('Last-Event-ID')).toBe('41');
    es.close();
  });

  it('never sends an empty Last-Event-ID header', async () => {
    const first = stream();
    const second = stream();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValueOnce(first.response).mockResolvedValueOnce(second.response);
    const es = new FetchEventSource('https://h.example/s', setup(fetchMock).deps);
    await vi.advanceTimersByTimeAsync(0);
    first.send('id\ndata: x\n\n'); // `id` with an empty value resets the cursor to ""
    first.end();
    await vi.advanceTimersByTimeAsync(60);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(new Headers(fetchMock.mock.calls[1]?.[1]?.headers).has('Last-Event-ID')).toBe(false);
    es.close();
  });

  it('refuses to attach the bearer to a URL outside the deployment origin', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(stream().response);
    const { deps } = setup(fetchMock, { origin: 'https://h.example' });
    const es = new FetchEventSource('https://evil.example/s', deps);
    await vi.advanceTimersByTimeAsync(0);
    expect(fetchMock).not.toHaveBeenCalled();
    expect(es.readyState).toBe(FetchEventSource.CLOSED);
  });

  it('retries a network failure instead of failing', async () => {
    const s = stream();
    const fetchMock = vi.fn<typeof fetch>().mockRejectedValueOnce(new TypeError('offline')).mockResolvedValueOnce(s.response);
    const es = new FetchEventSource('https://h.example/s', setup(fetchMock).deps);
    await vi.advanceTimersByTimeAsync(0);
    expect(es.readyState).toBe(FetchEventSource.CONNECTING);
    await vi.advanceTimersByTimeAsync(60);
    expect(es.readyState).toBe(FetchEventSource.OPEN);
    es.close();
  });

  it('a 401 refreshes once and reconnects at once with the new token', async () => {
    const s = stream();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValueOnce(new Response('', { status: 401 })).mockResolvedValueOnce(s.response);
    const accessToken = vi.fn<() => Promise<string | undefined>>().mockResolvedValueOnce('old').mockResolvedValue('new');
    const { deps, transport } = setup(fetchMock, { accessToken });
    const es = new FetchEventSource('https://h.example/s', deps);

    await vi.advanceTimersByTimeAsync(0);

    expect(transport.refresh).toHaveBeenCalledWith('old');
    expect(new Headers(fetchMock.mock.calls[1]?.[1]?.headers).get('Authorization')).toBe('Bearer new');
    expect(es.readyState).toBe(FetchEventSource.OPEN);
    expect(transport.signOut).not.toHaveBeenCalled();
    es.close();
  });

  it('a 401 after a failed refresh signs out and fails permanently', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response('', { status: 401 }));
    const { deps, transport } = setup(fetchMock, { refresh: vi.fn().mockResolvedValue('ended') });
    const es = new FetchEventSource('https://h.example/s', deps);
    await vi.advanceTimersByTimeAsync(0);
    expect(es.readyState).toBe(FetchEventSource.CLOSED);
    expect(transport.signOut).toHaveBeenCalledWith('refresh_failed');
  });

  it('a 401 after a SUCCESSFUL refresh fails permanently without signing out', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockImplementation(() => Promise.resolve(new Response('', { status: 401 })));
    const { deps, transport } = setup(fetchMock);
    const es = new FetchEventSource('https://h.example/s', deps);
    await vi.advanceTimersByTimeAsync(0);
    expect(transport.refresh).toHaveBeenCalledTimes(1);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(es.readyState).toBe(FetchEventSource.CLOSED);
    expect(transport.signOut).not.toHaveBeenCalled();
  });

  it('a 401 whose refresh is merely unavailable retries later instead of signing out', async () => {
    const s = stream();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValueOnce(new Response('', { status: 401 })).mockResolvedValueOnce(s.response);
    const { deps, transport } = setup(fetchMock, { refresh: vi.fn().mockResolvedValue('unavailable') });
    const es = new FetchEventSource('https://h.example/s', deps);
    await vi.advanceTimersByTimeAsync(0);
    expect(es.readyState).toBe(FetchEventSource.CONNECTING);
    expect(transport.signOut).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(60);
    expect(es.readyState).toBe(FetchEventSource.OPEN);
    es.close();
  });

  it('close() aborts the request and stops reconnecting', async () => {
    const first = stream();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(first.response);
    const es = new FetchEventSource('https://h.example/s', setup(fetchMock).deps);
    await vi.advanceTimersByTimeAsync(0);
    const signal = fetchMock.mock.calls[0]?.[1]?.signal;
    es.close();
    expect(signal?.aborted).toBe(true);
    expect(es.readyState).toBe(FetchEventSource.CLOSED);
    await vi.advanceTimersByTimeAsync(1000);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
