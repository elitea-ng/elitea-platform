import { CLIENT_VERSION_HEADER } from '@/api/clientVersion';
import { ApiError } from '@/api/errors';
import type { SseEvent } from '@/api/sse/parser';
import { ResumableStream, backoffDelay, type ResumableStreamOptions, type StreamStatusInfo } from '@/api/sse/stream';
import { abortAll, inFlightCount } from '@/lib/abortRegistry';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { fakeStreamTransport, type ScriptedAnswer } from '@/test/fakeStreamTransport';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const ORIGIN = 'https://ai.example.com';
const URL_ = `${ORIGIN}/api/v2/executions/7/exec-1/events`;

function fakeAppState() {
  let listener: ((s: 'active' | 'background' | 'inactive') => void) | null = null;
  return {
    currentState: 'active' as const,
    addEventListener: jest.fn((_type: 'change', l: (s: 'active' | 'background' | 'inactive') => void) => {
      listener = l;
      return { remove: () => (listener = null) };
    }),
    emit: (s: 'active' | 'background' | 'inactive') => listener?.(s),
  };
}

const flush = () => jest.advanceTimersByTimeAsync(0);

function setup(
  script?: (i: number) => ScriptedAnswer,
  extra: Partial<ResumableStreamOptions> = {},
) {
  const { transport, connections } = fakeStreamTransport(script);
  const events: SseEvent[] = [];
  const statuses: StreamStatusInfo[] = [];
  const appState = fakeAppState();
  let clock = 0;
  const stream = new ResumableStream({
    url: URL_,
    transport,
    appState,
    random: () => 0.5,
    now: () => clock,
    onEvent: (e) => {
      events.push(e);
    },
    onStatus: (s) => statuses.push(s),
    ...extra,
  });
  const done = stream.start();
  return {
    stream,
    done,
    transport,
    connections,
    events,
    statuses,
    appState,
    tick: (ms: number) => {
      clock += ms;
      return jest.advanceTimersByTimeAsync(ms);
    },
    last: () => statuses[statuses.length - 1],
  };
}

beforeEach(() => {
  jest.useFakeTimers();
  jest.spyOn(console, 'debug').mockImplementation(() => {});
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  __resetSessionForTests();
  abortAll();
  setSession({ hydrated: true, serverOrigin: ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 'tok' });
});

afterEach(() => {
  jest.useRealTimers();
  jest.restoreAllMocks();
});

describe('backoffDelay', () => {
  it('stays within full-jitter bounds and is capped', () => {
    for (let attempt = 0; attempt < 12; attempt++) {
      const ceiling = Math.min(15_000, 500 * 2 ** attempt);
      expect(backoffDelay(attempt, () => 0)).toBe(0);
      expect(backoffDelay(attempt, () => 0.999999)).toBeLessThan(ceiling);
      expect(backoffDelay(attempt, () => 0.999999)).toBeGreaterThanOrEqual(ceiling - 1);
    }
    expect(backoffDelay(50, () => 0.5)).toBe(7500);
  });
});

describe('ResumableStream', () => {
  it('sends stream headers and resumes with Last-Event-ID = the last id seen, including ignored and data-less events', async () => {
    const t = setup();
    await flush();
    const first = t.connections[0];
    expect(first.url).toBe(URL_);
    expect(first.headers.Accept).toBe('text/event-stream');
    expect(first.headers.Authorization).toBe('Bearer tok');
    expect(first.headers[CLIENT_VERSION_HEADER]).toBeTruthy();
    expect(first.headers['Last-Event-ID']).toBeUndefined();

    first.push('id: 1\nevent: execution.node_event\ndata: {}\n\nid: 2\nevent: something.else\ndata: {}\n\nid: 3\nevent: x\n\n');
    await flush();
    expect(t.events.map((e) => e.id)).toEqual(['1', '2']);
    expect(t.stream.lastEventId).toBe('3');

    first.end();
    await flush();
    await t.tick(10_000);
    const second = t.connections[1];
    expect(second.headers['Last-Event-ID']).toBe('3');
    expect(second.url).not.toContain('cursor=');
    t.stream.close();
    await t.done;
  });

  it('honours Retry-After on 503 and 429', async () => {
    const t = setup((i) => (i === 0 ? { status: 503, headers: { 'Retry-After': '3' } } : i === 1 ? { status: 429, headers: { 'Retry-After': '2' } } : {}));
    await flush();
    expect(t.last()).toEqual({ status: 'retrying', retryInMs: 3000 });
    await t.tick(2999);
    expect(t.transport).toHaveBeenCalledTimes(1);
    await t.tick(1);
    expect(t.transport).toHaveBeenCalledTimes(2);
    expect(t.last()).toEqual({ status: 'retrying', retryInMs: 2000 });
    await t.tick(2000);
    expect(t.transport).toHaveBeenCalledTimes(3);
    t.stream.close();
    await t.done;
  });

  it.each([400, 403, 404])('does not retry a %s', async (status) => {
    const t = setup(() => ({ status }));
    await t.done;
    expect(t.transport).toHaveBeenCalledTimes(1);
    expect(t.last().status).toBe('closed');
    expect(t.last().error).toMatchObject({ kind: 'http', status });
  });

  it('delegates 401 to the session classification with the body', async () => {
    const classifyFailure = jest.fn(async (response: { status: number; json?: () => Promise<unknown> }) => {
      expect(await response.json?.()).toEqual({ error: 'device_revoked' });
      return new ApiError('session_invalid', 'revoked', { status: 401, reason: 'device_revoked' });
    });
    const t = setup(() => ({ status: 401, body: '{"error":"device_revoked"}' }), { classifyFailure });
    await t.done;
    expect(classifyFailure).toHaveBeenCalledWith(expect.objectContaining({ status: 401 }), {
      token: 'tok',
      sessionAuth: true,
    });
    expect(t.transport).toHaveBeenCalledTimes(1);
    expect(t.last().error).toMatchObject({ reason: 'device_revoked' });
  });

  it('fires the watchdog after 30 s without bytes and reconnects', async () => {
    const t = setup();
    await flush();
    const first = t.connections[0];
    await t.tick(20_000);
    first.push(': heartbeat\n\n');
    await flush();
    await t.tick(20_000);
    expect(first.signal.aborted).toBe(false);
    await t.tick(10_000);
    expect(first.signal.aborted).toBe(true);
    expect(t.last().status).toBe('retrying');
    await t.tick(15_000);
    expect(t.transport).toHaveBeenCalledTimes(2);
    t.stream.close();
    await t.done;
  });

  it('pauses in the background and resumes from the cursor in the foreground', async () => {
    const t = setup();
    await flush();
    t.connections[0].push('id: 41\ndata: {}\n\n');
    await flush();

    t.appState.emit('inactive');
    await flush();
    expect(t.connections[0].signal.aborted).toBe(false);

    t.appState.emit('background');
    await flush();
    expect(t.connections[0].signal.aborted).toBe(true);
    expect(t.last().status).toBe('paused');
    await t.tick(60_000);
    expect(t.transport).toHaveBeenCalledTimes(1);

    t.appState.emit('active');
    await flush();
    expect(t.transport).toHaveBeenCalledTimes(2);
    expect(t.connections[1].headers['Last-Event-ID']).toBe('41');
    t.stream.close();
    await t.done;
  });

  it('closes the transport when the consumer reports a terminal frame', async () => {
    const t = setup(undefined, { onEvent: (e) => (e.data === '"end"' ? 'stop' : undefined) });
    await flush();
    t.connections[0].push('id: 1\ndata: "a"\n\nid: 2\ndata: "end"\n\nid: 3\ndata: "late"\n\n');
    await t.done;
    expect(t.connections[0].signal.aborted).toBe(true);
    expect(t.last().status).toBe('closed');
    expect(t.stream.lastEventId).toBe('2');
    await t.tick(60_000);
    expect(t.transport).toHaveBeenCalledTimes(1);
    expect(inFlightCount()).toBe(0);
  });

  it('gives up after 2 minutes of failed reconnects and retries on demand', async () => {
    const t = setup(() => ({ fail: true }));
    for (let i = 0; i < 40 && t.last()?.status !== 'gave_up'; i++) {
      await t.tick(15_000);
    }
    expect(t.last().status).toBe('gave_up');
    const calls = t.transport.mock.calls.length;
    await t.tick(10 * 60_000);
    expect(t.transport).toHaveBeenCalledTimes(calls);

    t.stream.reconnect();
    await flush();
    expect(t.transport).toHaveBeenCalledTimes(calls + 1);
    t.stream.close();
    await t.done;
  });

  it('treats a connection that opened and received heartbeats as healthy: backoff resets and it never gives up', async () => {
    const t = setup();
    // 10 connections, each 200 + heartbeats only for 20 s, then dropped: 200 s
    // in all, well past the 120 s give-up window.
    for (let i = 0; i < 10; i++) {
      await flush();
      expect(t.transport).toHaveBeenCalledTimes(i + 1);
      const connection = t.connections[i];
      connection.push(': heartbeat\n\n');
      await t.tick(20_000);
      connection.end();
      await flush();
      expect(t.last()).toEqual({ status: 'retrying', retryInMs: 250 });
      await t.tick(250);
    }
    expect(t.statuses.some((s) => s.status === 'gave_up')).toBe(false);
    expect(t.events).toEqual([]);
    t.stream.close();
    await t.done;
  });

  it('backs off and gives up on 200s that send a heartbeat and close at once', async () => {
    const t = setup();
    const delays: number[] = [];
    for (let i = 0; i < 40 && !t.statuses.some((s) => s.status === 'gave_up'); i++) {
      await flush();
      const connection = t.connections[i];
      connection.push(': heartbeat\n\n');
      connection.end();
      await flush();
      const last = t.last();
      if (last.status !== 'retrying') break;
      delays.push(last.retryInMs ?? 0);
      await t.tick(last.retryInMs ?? 0);
    }
    expect(delays.slice(0, 4)).toEqual([250, 500, 1000, 2000]);
    await flush();
    expect(t.last().status).toBe('gave_up');
    t.stream.close();
    await t.done;
  });

  it('still backs off for a 200 that ends without a single byte', async () => {
    const t = setup();
    await flush();
    t.connections[0].end();
    await flush();
    expect(t.last()).toEqual({ status: 'retrying', retryInMs: 250 });
    await t.tick(250);
    t.connections[1].end();
    await flush();
    expect(t.last()).toEqual({ status: 'retrying', retryInMs: 500 });
    t.stream.close();
    await t.done;
  });

  it('uses the server retry: value as the reconnect base delay, across connections', async () => {
    const t = setup((i) => (i === 1 ? { fail: true } : {}));
    await flush();
    t.connections[0].push('retry: 2000\n\n');
    t.connections[0].end();
    await flush();
    // Opened, so attempt 0: full jitter over 2000 ms (random 0.5).
    expect(t.last()).toEqual({ status: 'retrying', retryInMs: 1000 });
    await t.tick(1000);
    // The next connection fails: attempt 1 doubles the server's base.
    await flush();
    expect(t.transport).toHaveBeenCalledTimes(2);
    expect(t.last()).toEqual({ status: 'retrying', retryInMs: 2000 });
    t.stream.close();
    await t.done;
  });

  it('bounds the server retry: value by the maximum delay', async () => {
    const t = setup();
    await flush();
    t.connections[0].push('retry: 600000\n\n');
    t.connections[0].end();
    await flush();
    expect(t.last()).toEqual({ status: 'retrying', retryInMs: 7500 });
    t.stream.close();
    await t.done;
  });

  it('floors a server retry: 0 so a failing network still backs off', async () => {
    const t = setup((i) => (i >= 1 ? { fail: true } : {}));
    await flush();
    t.connections[0].push('retry: 0\n\n');
    t.connections[0].end();
    await flush();
    expect(t.last()).toEqual({ status: 'retrying', retryInMs: 50 });
    t.stream.close();
    await t.done;
  });

  it('lets Retry-After override the server retry: value', async () => {
    const t = setup((i) => (i === 1 ? { status: 503, headers: { 'Retry-After': '3' } } : {}));
    await flush();
    t.connections[0].push('retry: 2000\n\n');
    t.connections[0].end();
    await flush();
    await t.tick(1000);
    expect(t.last()).toEqual({ status: 'retrying', retryInMs: 3000 });
    t.stream.close();
    await t.done;
  });

  it('refuses a URL off the workspace origin without sending the token', async () => {
    const t = setup(undefined, { url: 'https://evil.example/api/v2/executions/7/x/events' });
    await t.done;
    expect(t.transport).not.toHaveBeenCalled();
    expect(t.last().error).toMatchObject({ kind: 'origin_mismatch' });
  });

  it('rejects a response redirected to another origin', async () => {
    const t = setup(() => ({ url: 'https://evil.example/events' }));
    await t.done;
    expect(t.last().error).toMatchObject({ kind: 'origin_mismatch' });
  });

  it('is aborted by endSession through the abort registry', async () => {
    const t = setup();
    await flush();
    expect(inFlightCount()).toBe(1);
    abortAll('session_ended');
    await flush();
    expect(t.connections[0].signal.aborted).toBe(true);
    t.stream.close();
    await t.done;
  });
});
