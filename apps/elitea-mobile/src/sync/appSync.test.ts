import { bindSync } from '@/sync/appSync';
import { INITIAL_SESSION, type SessionState } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

function setup(open = false) {
  let status: 'open' | 'closed' | 'unavailable' = open ? 'open' : 'closed';
  const storeListeners = new Set<() => void>();
  let session: SessionState = { ...INITIAL_SESSION, projectId: 7 };
  const sessionListeners = new Set<() => void>();
  let activity: ((s: 'active' | 'background') => void) | null = null;
  const manager = { requestSync: jest.fn() };
  bindSync({
    store: {
      get status() {
        return status;
      },
      subscribe: (l) => {
        storeListeners.add(l);
        return () => storeListeners.delete(l);
      },
    },
    session: {
      get: () => session,
      subscribe: (l) => {
        sessionListeners.add(l);
        return () => sessionListeners.delete(l);
      },
    },
    activity: {
      addEventListener: (_t, l) => {
        activity = l as typeof activity;
        return { remove: () => {} };
      },
    },
    manager,
  });
  return {
    manager,
    setStore(next: typeof status) {
      status = next;
      for (const l of [...storeListeners]) l();
    },
    setSession(patch: Partial<SessionState>) {
      session = { ...session, ...patch };
      for (const l of [...sessionListeners]) l();
    },
    activity: (s: 'active' | 'background') => activity?.(s),
  };
}

describe('bindSync', () => {
  it('syncs when the store opens (after unlock), not on every store event', () => {
    const t = setup();
    expect(t.manager.requestSync).not.toHaveBeenCalled();
    t.setStore('open');
    t.setStore('open');
    expect(t.manager.requestSync).toHaveBeenCalledTimes(1);
    expect(t.manager.requestSync).toHaveBeenCalledWith(7);
  });

  it('syncs at once when bound to an open store, on a project switch and on foreground', () => {
    const t = setup(true);
    expect(t.manager.requestSync).toHaveBeenCalledTimes(1);
    t.setSession({ projectId: 9 });
    expect(t.manager.requestSync).toHaveBeenLastCalledWith(9);
    t.setSession({ ending: false });
    expect(t.manager.requestSync).toHaveBeenCalledTimes(2);
    t.activity('background');
    t.activity('active');
    expect(t.manager.requestSync).toHaveBeenCalledTimes(3);
  });
});
