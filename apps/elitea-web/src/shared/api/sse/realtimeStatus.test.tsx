import type { ReactNode } from 'react';

import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import {
  aggregateRealtimeStatus,
  createRealtimeStatusStore,
  RealtimeStatusContext,
  type RealtimeStatusStore,
  useRealtimeStatus,
  useReportRealtimeChannel,
  type RealtimeChannelState,
} from './realtimeStatus';

function setOnline(online: boolean): void {
  Object.defineProperty(navigator, 'onLine', { configurable: true, get: () => online });
  window.dispatchEvent(new Event(online ? 'online' : 'offline'));
}

function withStore(store: RealtimeStatusStore) {
  return function Wrapper({ children }: { readonly children: ReactNode }): ReactNode {
    return <RealtimeStatusContext.Provider value={store}>{children}</RealtimeStatusContext.Provider>;
  };
}

afterEach(() => {
  setOnline(true);
});

describe('aggregateRealtimeStatus', () => {
  it('is idle with no channel registered — nothing to claim, so no false "Disconnected"', () => {
    expect(aggregateRealtimeStatus({}, true)).toBe('idle');
  });

  it('is connected as soon as any channel is open', () => {
    expect(aggregateRealtimeStatus({ a: 'open' }, true)).toBe('connected');
    expect(aggregateRealtimeStatus({ a: 'offline', b: 'reconnecting', c: 'open' }, true)).toBe('connected');
  });

  it('prefers reconnecting over connecting when nothing is open', () => {
    expect(aggregateRealtimeStatus({ a: 'connecting' }, true)).toBe('connecting');
    expect(aggregateRealtimeStatus({ a: 'connecting', b: 'reconnecting' }, true)).toBe('reconnecting');
    expect(aggregateRealtimeStatus({ a: 'offline', b: 'reconnecting' }, true)).toBe('reconnecting');
  });

  it('is offline only when every channel gave up', () => {
    expect(aggregateRealtimeStatus({ a: 'offline', b: 'offline' }, true)).toBe('offline');
  });

  it('is offline whenever the browser reports no network, even with an open channel', () => {
    expect(aggregateRealtimeStatus({ a: 'open' }, false)).toBe('offline');
    expect(aggregateRealtimeStatus({}, false)).toBe('offline');
  });
});

describe('createRealtimeStatusStore', () => {
  it('notifies subscribers only on a real change', () => {
    const store = createRealtimeStatusStore();
    let calls = 0;
    const unsubscribe = store.subscribe(() => {
      calls += 1;
    });
    store.report('a', 'open');
    store.report('a', 'open');
    store.report('b', null);
    expect(calls).toBe(1);
    store.report('a', null);
    expect(calls).toBe(2);
    expect(store.getChannels()).toEqual({});
    unsubscribe();
    store.report('a', 'open');
    expect(calls).toBe(2);
  });
});

describe('useRealtimeStatus', () => {
  it('is idle without a provider, and reports are dropped rather than thrown', () => {
    const { result } = renderHook(() => {
      useReportRealtimeChannel('n', 'open');
      return useRealtimeStatus();
    });
    expect(result.current).toBe('idle');
  });

  it('follows channel transitions connecting -> connected -> reconnecting -> offline -> connected', () => {
    const store = createRealtimeStatusStore();
    const { result } = renderHook(() => useRealtimeStatus(), { wrapper: withStore(store) });
    expect(result.current).toBe('idle');

    const sequence: ReadonlyArray<[RealtimeChannelState, string]> = [
      ['connecting', 'connecting'],
      ['open', 'connected'],
      ['reconnecting', 'reconnecting'],
      ['offline', 'offline'],
      ['open', 'connected'],
    ];
    for (const [state, expected] of sequence) {
      act(() => store.report('n', state));
      expect(result.current).toBe(expected);
    }
  });

  it('keeps reporting connected while one of two channels is still open', () => {
    const store = createRealtimeStatusStore();
    const { result } = renderHook(() => useRealtimeStatus(), { wrapper: withStore(store) });
    act(() => {
      store.report('a', 'open');
      store.report('b', 'open');
    });
    act(() => store.report('a', null));
    expect(result.current).toBe('connected');
  });

  it('reacts to the browser going offline and back online', () => {
    const store = createRealtimeStatusStore();
    const { result } = renderHook(() => useRealtimeStatus(), { wrapper: withStore(store) });
    act(() => store.report('n', 'open'));
    act(() => setOnline(false));
    expect(result.current).toBe('offline');
    act(() => setOnline(true));
    expect(result.current).toBe('connected');
  });
});

describe('useReportRealtimeChannel', () => {
  it('reports, follows state changes, and withdraws on null and on unmount', () => {
    const store = createRealtimeStatusStore();
    const { rerender, unmount } = renderHook(
      ({ state }: { state: RealtimeChannelState | null }) => useReportRealtimeChannel('n', state),
      { wrapper: withStore(store), initialProps: { state: 'connecting' as RealtimeChannelState | null } },
    );
    expect(store.getChannels()).toEqual({ n: 'connecting' });
    rerender({ state: 'open' });
    expect(store.getChannels()).toEqual({ n: 'open' });
    rerender({ state: null });
    expect(store.getChannels()).toEqual({});
    rerender({ state: 'open' });
    unmount();
    expect(store.getChannels()).toEqual({});
  });
});
