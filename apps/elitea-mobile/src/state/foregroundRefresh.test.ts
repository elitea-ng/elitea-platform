import type { AppStateStatus } from 'react-native';

import { bindForegroundRefresh, bindReconnectRefresh } from '@/state/foregroundRefresh';

function harness(connected = true) {
  const listeners = new Set<(s: AppStateStatus) => void>();
  const refresh = jest.fn(async () => undefined);
  const unbind = bindForegroundRefresh({
    appState: {
      currentState: 'active',
      addEventListener: (_type, listener) => {
        listeners.add(listener);
        return { remove: () => listeners.delete(listener) };
      },
    },
    refresh,
    isConnected: () => connected,
  });
  return { refresh, unbind, emit: (s: AppStateStatus) => [...listeners].forEach((l) => l(s)), listeners };
}

// DEF-R1 / DEF-R4: the public policy and the minimum version are re-read on
// every return from the background, not only at launch.
describe('bindForegroundRefresh', () => {
  it('re-reads discovery each time the app returns from the background', () => {
    const h = harness();
    h.emit('background');
    h.emit('active');
    expect(h.refresh).toHaveBeenCalledTimes(1);
    h.emit('background');
    h.emit('inactive');
    h.emit('active');
    expect(h.refresh).toHaveBeenCalledTimes(2);
  });

  it('ignores inactive → active (Control Center, the Face ID sheet)', () => {
    const h = harness();
    h.emit('inactive');
    h.emit('active');
    h.emit('active');
    expect(h.refresh).not.toHaveBeenCalled();
  });

  it('does nothing while no workspace is connected, and stops when unbound', () => {
    const idle = harness(false);
    idle.emit('background');
    idle.emit('active');
    expect(idle.refresh).not.toHaveBeenCalled();

    const h = harness();
    h.unbind();
    expect(h.listeners.size).toBe(0);
  });
});

// F1: discovery is kept in memory only. A launch without network leaves it
// unknown (contract-gated Settings rows hidden, swipe-to-dismiss off), so it is
// re-read when the connection comes back, not only on the next foreground.
describe('bindReconnectRefresh', () => {
  function reconnectHarness(connected = true) {
    const listeners = new Set<(online: boolean) => void>();
    const refresh = jest.fn(async () => undefined);
    const unbind = bindReconnectRefresh({
      network: {
        subscribe: (listener) => {
          listeners.add(listener);
          return () => listeners.delete(listener);
        },
      },
      refresh,
      isConnected: () => connected,
    });
    return { refresh, unbind, emit: (online: boolean) => [...listeners].forEach((l) => l(online)), listeners };
  }

  it('re-reads discovery each time the device comes back online', () => {
    const h = reconnectHarness();
    h.emit(false);
    expect(h.refresh).not.toHaveBeenCalled();
    h.emit(true);
    expect(h.refresh).toHaveBeenCalledTimes(1);
    h.emit(false);
    h.emit(true);
    expect(h.refresh).toHaveBeenCalledTimes(2);
  });

  it('does nothing while no workspace is connected, and stops when unbound', () => {
    const idle = reconnectHarness(false);
    idle.emit(true);
    expect(idle.refresh).not.toHaveBeenCalled();

    const h = reconnectHarness();
    h.unbind();
    expect(h.listeners.size).toBe(0);
  });
});
