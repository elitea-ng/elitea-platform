import { afterEach, describe, expect, it } from 'vitest';

import { setNativeTransport, type NativeTransport } from '@/shared/api/nativeTransport';
import { fakeSocketFactory } from '@/shared/api/browserView/testing';

import { cloudBrowserTabSession } from './cloudBrowserTab';

const NATIVE: NativeTransport = { accessToken: () => Promise.resolve('t'), refresh: () => Promise.resolve('refreshed'), signOut: () => undefined };
const BROWSED = [{ role: 'assistant', executionId: 'run-1', toolActions: [{ name: 'browser_navigate' }] }];

afterEach(() => setNativeTransport(undefined));

describe('the Browser tab of a Local work thread', () => {
  it('is the local pane while the thread is on this computer', () => {
    setNativeTransport({ ...NATIVE, createWebSocket: fakeSocketFactory().factory });
    expect(cloudBrowserTabSession(false, BROWSED)).toBeUndefined();
  });

  it("is the cloud run's browser, streamed, once a run away from this computer used it", () => {
    setNativeTransport({ ...NATIVE, createWebSocket: fakeSocketFactory().factory });
    expect(cloudBrowserTabSession(true, BROWSED)).toMatch(/^elitea-br-[0-9a-f]{24}$/);
    expect(cloudBrowserTabSession(true, [{ role: 'assistant', toolActions: [] }])).toBeNull();
  });

  it('is absent when this build cannot open the view', () => {
    setNativeTransport(NATIVE);
    expect(cloudBrowserTabSession(true, BROWSED)).toBeNull();
  });
});
