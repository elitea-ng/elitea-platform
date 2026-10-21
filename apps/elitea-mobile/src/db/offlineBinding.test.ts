import { bindOfflineStore, offlineAllowed } from '@/db/offlineBinding';
import { STRICT_DEFAULTS, type PolicyState } from '@/security/policy';
import type { LockState } from '@/security/lock/lockController';
import { INITIAL_SESSION, type SessionState } from '@/state/session';

const ORIGIN = 'https://ai.example.com';

function source<T>(initial: T) {
  let value = initial;
  const listeners = new Set<() => void>();
  return {
    get: () => value,
    subscribe: (l: () => void) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    set(next: T) {
      value = next;
      for (const l of [...listeners]) l();
    },
  };
}

const WS = 'aaaaaaaaaaaa';
const signedIn: SessionState = {
  ...INITIAL_SESSION,
  hydrated: true,
  serverOrigin: ORIGIN,
  workspaceId: WS,
  credential: { kind: 'oauth' },
};
const offlinePolicy: PolicyState = { source: 'server', policy: { ...STRICT_DEFAULTS, offline_retention_days: 14 } };
const unlocked: LockState = { status: 'unlocked', authInFlight: false, lastFailure: null, bypassed: false };
const locked: LockState = { ...unlocked, status: 'locked' };

function setup(initial: { session?: SessionState; policy?: PolicyState; lock?: LockState; supported?: boolean } = {}) {
  const store = {
    open: jest.fn(async () => null),
    close: jest.fn(async () => {}),
    wipe: jest.fn(async () => {}),
    retain: jest.fn(async () => {}),
    setKeyGate: jest.fn(),
  };
  const session = source(initial.session ?? signedIn);
  const policy = source(initial.policy ?? offlinePolicy);
  const lock = source(initial.lock ?? unlocked);
  let activityListener: ((s: 'active' | 'background') => void) | null = null;
  const unbind = bindOfflineStore({
    store,
    session,
    policy,
    lock,
    supported: initial.supported ?? true,
    activity: {
      addEventListener: (_t, l) => {
        activityListener = l as typeof activityListener;
        return { remove: () => (activityListener = null) };
      },
    },
  });
  return { store, session, policy, lock, unbind, activity: (s: 'active' | 'background') => activityListener?.(s) };
}

describe('offlineAllowed', () => {
  it('needs a server policy with a retention window', () => {
    expect(offlineAllowed(offlinePolicy)).toBe(true);
    expect(offlineAllowed({ source: 'server', policy: STRICT_DEFAULTS })).toBe(false);
    expect(offlineAllowed({ source: 'default', policy: { ...STRICT_DEFAULTS, offline_retention_days: 14 } })).toBe(false);
    expect(offlineAllowed({ source: 'discovery', policy: { ...STRICT_DEFAULTS, offline_retention_days: 14 } })).toBe(false);
  });
});

describe('bindOfflineStore', () => {
  it('opens the store for the workspace once signed in, unlocked and allowed', () => {
    const { store } = setup();
    expect(store.open).toHaveBeenCalledWith(ORIGIN, WS);
    expect(store.wipe).not.toHaveBeenCalled();
  });

  it('installs the lock state as the key gate', () => {
    const { store, lock } = setup();
    const gate = store.setKeyGate.mock.calls[0][0] as () => boolean;
    expect(gate()).toBe(true);
    lock.set(locked);
    expect(gate()).toBe(false);
  });

  it('closes on lock and reopens after unlock', () => {
    const { store, lock } = setup({ lock: { ...unlocked, status: 'deciding' } });
    expect(store.open).not.toHaveBeenCalled();
    lock.set(unlocked);
    expect(store.open).toHaveBeenCalledTimes(1);
    lock.set(locked);
    expect(store.close).toHaveBeenCalled();
    lock.set(unlocked);
    expect(store.open).toHaveBeenCalledTimes(2);
  });

  it('never creates a store when the policy has no offline storage, and wipes one left behind', () => {
    const { store, policy } = setup({ policy: { source: 'server', policy: STRICT_DEFAULTS } });
    expect(store.open).not.toHaveBeenCalled();
    expect(store.wipe).toHaveBeenCalledTimes(1);
    policy.set({ source: 'server', policy: { ...STRICT_DEFAULTS, idle_lock_seconds: 5 } });
    expect(store.wipe).toHaveBeenCalledTimes(1);
  });

  it('wipes when a policy change turns offline storage off', () => {
    const { store, policy } = setup();
    policy.set({ source: 'server', policy: { ...STRICT_DEFAULTS, offline_retention_days: 0 } });
    expect(store.wipe).toHaveBeenCalledTimes(1);
  });

  it('re-applies retention when the policy changes and on resume', () => {
    const { store, policy, activity } = setup();
    policy.set({ source: 'server', policy: { ...STRICT_DEFAULTS, offline_retention_days: 3 } });
    expect(store.retain).toHaveBeenCalledTimes(1);
    activity('active');
    expect(store.retain).toHaveBeenCalledTimes(2);
  });

  it('closes when signed out or while the session is ending (endSession wipes)', () => {
    const { store, session } = setup();
    session.set({ ...signedIn, ending: true });
    expect(store.close).toHaveBeenCalledTimes(1);
    session.set({ ...INITIAL_SESSION, hydrated: true });
    expect(store.close).toHaveBeenCalledTimes(2);
    expect(store.wipe).not.toHaveBeenCalled();
  });

  it('does nothing on a platform without SQLCipher (web)', () => {
    const { store } = setup({ supported: false });
    expect(store.open).not.toHaveBeenCalled();
  });

  it('unbinding closes the key gate', () => {
    const { store, unbind } = setup();
    unbind();
    const gate = store.setKeyGate.mock.calls.at(-1)![0] as () => boolean;
    expect(gate()).toBe(false);
  });

  it('a workspace switch never wipes: the policy reset while the old workspace is put aside closes the store', () => {
    // suspendActiveWorkspace runs the reset hooks (policy back to the strict
    // default) while the session still names the workspace being left.
    const { store, session, policy } = setup();
    session.set({ ...signedIn, switching: true });
    policy.set({ source: 'default', policy: STRICT_DEFAULTS });
    expect(store.wipe).not.toHaveBeenCalled();
    expect(store.close).toHaveBeenCalled();
  });

  it('a policy not yet known (strict default, discovery only) closes the store and never wipes it', () => {
    const { store, policy } = setup({ policy: { source: 'default', policy: STRICT_DEFAULTS } });
    expect(store.open).not.toHaveBeenCalled();
    policy.set({ source: 'discovery', policy: STRICT_DEFAULTS });
    expect(store.wipe).not.toHaveBeenCalled();
    expect(store.close).toHaveBeenCalled();
    policy.set(offlinePolicy);
    expect(store.open).toHaveBeenLastCalledWith(ORIGIN, WS);
  });

  it("opens the other workspace's store after a switch, and wipes per workspace when offline storage is off", () => {
    const { store, session, policy } = setup();
    expect(store.open).toHaveBeenLastCalledWith(ORIGIN, WS);
    session.set({ ...signedIn, credential: null, serverOrigin: null, workspaceId: null });
    expect(store.close).toHaveBeenCalled();
    session.set({ ...signedIn, serverOrigin: 'https://b.example.com', workspaceId: 'bbbbbbbbbbbb' });
    expect(store.open).toHaveBeenLastCalledWith('https://b.example.com', 'bbbbbbbbbbbb');
    policy.set({ source: 'server', policy: STRICT_DEFAULTS });
    expect(store.wipe).toHaveBeenLastCalledWith('bbbbbbbbbbbb');
    session.set(signedIn);
    expect(store.wipe).toHaveBeenLastCalledWith(WS);
    expect(store.wipe).toHaveBeenCalledTimes(2);
  });
});
