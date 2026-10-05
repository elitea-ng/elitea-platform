import type { AppStateStatus } from 'react-native';

import { bindForegroundRefresh } from '@/state/foregroundRefresh';

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
