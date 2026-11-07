import { afterEach, describe, expect, it, vi } from 'vitest';

import { setNativeTransport } from '@/shared/api/nativeTransport';

import { browserViewAvailable, browserViewUrl, openBrowserView, type BrowserViewHandlers } from './client';
import { clipUtf8, parseDown } from './protocol';
import { fakeSocketFactory } from './testing';

const SESSION = 'elitea-br-0123456789abcdef01234567';

function open(handlers: BrowserViewHandlers = {}, diagnose = vi.fn(() => Promise.resolve({ failure: 'not_found' as const, retryable: false }))) {
  const fake = fakeSocketFactory();
  const connection = openBrowserView(SESSION, handlers, { socketFactory: fake.factory, url: 'ws://host/api/v2/x', diagnose });
  return { ...fake, connection, diagnose };
}

afterEach(() => setNativeTransport(undefined));

describe('browserViewUrl', () => {
  it('builds the ws(s) URL of the view under the API base', () => {
    expect(browserViewUrl(SESSION, '/api/v2', 'https://elitea.example/app/chat/1')).toBe(`wss://elitea.example/api/v2/browser_sessions/${SESSION}/view`);
    expect(browserViewUrl(SESSION, 'http://api.local:8084/api/v2/', 'https://web/')).toBe(`ws://api.local:8084/api/v2/browser_sessions/${SESSION}/view`);
  });

  it("resolves against the desktop host's deployment origin", () => {
    setNativeTransport({ accessToken: () => Promise.resolve('t'), refresh: () => Promise.resolve('refreshed'), signOut: () => undefined, origin: 'https://deploy.example' });
    expect(browserViewUrl(SESSION, '/api/v2', 'tauri://localhost/')).toBe(`wss://deploy.example/api/v2/browser_sessions/${SESSION}/view`);
  });
});

describe('browserViewAvailable', () => {
  it('is on in a browser, and on the desktop only with a host socket', () => {
    expect(browserViewAvailable()).toBe(true);
    const base = { accessToken: () => Promise.resolve('t'), refresh: () => Promise.resolve('refreshed' as const), signOut: () => undefined };
    setNativeTransport(base);
    expect(browserViewAvailable()).toBe(false);
    setNativeTransport({ ...base, createWebSocket: fakeSocketFactory().factory });
    expect(browserViewAvailable()).toBe(true);
  });
});

describe('openBrowserView', () => {
  it('pairs each frame header with the binary JPEG after it, and acks only when asked', () => {
    const onFrame = vi.fn();
    const onHello = vi.fn();
    const { last, connection } = open({ onFrame, onHello });
    last().hello(true);
    expect(onHello).toHaveBeenCalledWith({ type: 'hello', session_id: 'elitea-br-000000000000000000000000', can_control: true, lingering: false });
    last().frame(7, { url: 'https://a.test/', title: 'A', control: 'user' });
    expect(onFrame).toHaveBeenCalledOnce();
    const [header, jpeg] = onFrame.mock.calls[0] as [Record<string, unknown>, Blob];
    expect(header).toMatchObject({ seq: 7, url: 'https://a.test/', title: 'A', control: 'user', width: 1280, height: 800 });
    expect(jpeg.size).toBe(4);
    expect(last().sent).toEqual([]);
    connection.ack(7);
    expect(last().sent).toEqual([{ type: 'ack', seq: 7 }]);
  });

  it('drops a binary message no header announced, and a header a text message superseded', () => {
    const onFrame = vi.fn();
    const { last } = open({ onFrame });
    last().hello(false);
    last().onmessage?.(new MessageEvent('message', { data: new Blob([new Uint8Array([1])]) }));
    last().text({ type: 'frame', seq: 1, width: 1, height: 1, bytes: 1, url: '', title: '', control: 'agent' });
    last().text({ type: 'notice', code: 'session_lingering' });
    last().onmessage?.(new MessageEvent('message', { data: new Blob([new Uint8Array([1])]) }));
    expect(onFrame).not.toHaveBeenCalled();
  });

  it('sends nothing before hello', () => {
    const { last, connection } = open();
    connection.takeOver();
    connection.ack(1);
    expect(last().sent).toEqual([]);
  });

  it('sends input, take over and hand back in the contract shape, within its bounds', () => {
    const { last, connection } = open();
    last().hello(true);
    connection.pointer('down', 10.5, 20, 1, 1, 8);
    connection.wheel(1, 2, 0, -120, 0);
    connection.key('char', 'é'.repeat(40), 'KeyE', 'é', 0);
    connection.takeOver();
    connection.handBack('x'.repeat(3000));
    const [pointer, wheel, key, take, hand] = last().sent;
    expect(pointer).toEqual({ type: 'pointer', kind: 'down', x: 10.5, y: 20, button: 1, click_count: 1, modifiers: 8 });
    expect(wheel).toEqual({ type: 'wheel', x: 1, y: 2, delta_x: 0, delta_y: -120, modifiers: 0 });
    expect(new TextEncoder().encode(String(key?.['key'])).length).toBeLessThanOrEqual(32);
    expect(take).toEqual({ type: 'take_over' });
    expect(String(hand?.['note'])).toHaveLength(2048);
  });

  it('reports notices, refusals and the end', () => {
    const onNotice = vi.fn();
    const onRefused = vi.fn();
    const onEnd = vi.fn();
    const onFailure = vi.fn();
    const { last } = open({ onNotice, onRefused, onEnd, onFailure });
    last().hello(true);
    last().text({ type: 'notice', code: 'control_revoked' });
    last().text({ type: 'refused', code: 'input_refused' });
    last().text({ type: 'end', code: 'linger_elapsed' });
    last().serverClose(1000, 'linger_elapsed');
    expect(onNotice).toHaveBeenCalledWith('control_revoked');
    expect(onRefused).toHaveBeenCalledWith('input_refused');
    expect(onEnd).toHaveBeenCalledWith('linger_elapsed');
    expect(onFailure).not.toHaveBeenCalled();
  });

  it('tells a revoked viewer from a dropped connection and a re-check outage', () => {
    const onFailure = vi.fn();
    let fake = open({ onFailure });
    fake.last().hello(false);
    fake.last().serverClose(4403, 'access_revoked');
    expect(onFailure).toHaveBeenLastCalledWith('access_revoked', false);
    fake = open({ onFailure });
    fake.last().hello(false);
    fake.last().serverClose(1013);
    expect(onFailure).toHaveBeenLastCalledWith('unavailable', true);
    fake = open({ onFailure });
    fake.last().hello(false);
    fake.last().serverClose(1006);
    expect(onFailure).toHaveBeenLastCalledWith('disconnected', true);
  });

  it('asks the route why a socket closed before hello', async () => {
    const onFailure = vi.fn();
    const { last, diagnose } = open({ onFailure }, vi.fn(() => Promise.resolve({ failure: 'ended' as const, retryable: false })));
    last().serverClose(1006);
    await vi.waitFor(() => expect(onFailure).toHaveBeenCalledWith('ended', false));
    expect(diagnose).toHaveBeenCalledWith(SESSION);
  });

  it('closes quietly when the viewer closes it', () => {
    const onFailure = vi.fn();
    const { last, connection } = open({ onFailure });
    last().hello(false);
    connection.close();
    last().serverClose(1000);
    expect(last().closedWith).toBe(1000);
    expect(onFailure).not.toHaveBeenCalled();
  });
});

describe('protocol', () => {
  it('parses what it knows and drops the rest', () => {
    expect(parseDown('{"type":"frame","seq":1}')).toBeUndefined();
    expect(parseDown('not json')).toBeUndefined();
    expect(parseDown('{"type":"later"}')).toBeUndefined();
    expect(parseDown('{"type":"frame","seq":2,"width":3,"height":4,"bytes":5,"control":"robot"}')).toMatchObject({ control: 'agent', url: '', title: '' });
  });

  it('clips on a character boundary', () => {
    expect(clipUtf8('aé', 2)).toBe('a');
    expect(clipUtf8('abc', 3)).toBe('abc');
  });
});
