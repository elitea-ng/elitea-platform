import type { AppStateStatus } from 'react-native';

import type { AuthOutcome, DeviceAuth, DeviceSecurity } from '@/security/lock/deviceAuth';
import { LockController } from '@/security/lock/lockController';
import { STRICT_DEFAULTS, type ClientPolicy } from '@/security/policy';

function harness(
  opts: { security?: DeviceSecurity; policy?: Partial<ClientPolicy>; isDev?: boolean; ending?: () => boolean } = {},
) {
  const listeners = new Set<(s: AppStateStatus) => void>();
  const appState = {
    addEventListener: (_: 'change', l: (s: AppStateStatus) => void) => {
      listeners.add(l);
      return {
        remove: () => {
          listeners.delete(l);
        },
      };
    },
  };
  let clock = 1_000_000;
  let security: DeviceSecurity = opts.security ?? 'biometric';
  const pending: ((o: AuthOutcome) => void)[] = [];
  const deviceAuth: DeviceAuth = {
    security: jest.fn(async () => security),
    authenticate: jest.fn(
      () =>
        new Promise<AuthOutcome>((resolve) => {
          pending.push(resolve);
        }),
    ),
  };
  let policy: ClientPolicy = { ...STRICT_DEFAULTS, ...opts.policy };
  const lockedChanges: boolean[] = [];
  const controller = new LockController({
    deviceAuth,
    getPolicy: () => policy,
    appState,
    now: () => clock,
    isDev: opts.isDev ?? false,
    isSessionEnding: opts.ending,
    onLockedChange: (l) => lockedChanges.push(l),
  });
  const flush = () => new Promise((r) => setTimeout(r, 0));
  return {
    controller,
    deviceAuth,
    lockedChanges,
    emit: (s: AppStateStatus) => [...listeners].forEach((l) => l(s)),
    advance: (ms: number) => {
      clock += ms;
    },
    setSecurity: (s: DeviceSecurity) => {
      security = s;
    },
    setPolicy: (patch: Partial<ClientPolicy>) => {
      policy = { ...policy, ...patch };
    },
    /** Answers the open prompt. */
    answer: async (o: AuthOutcome) => {
      const resolve = pending.shift();
      if (!resolve) throw new Error('no prompt open');
      resolve(o);
      await flush();
    },
    promptsOpen: () => pending.length,
    flush,
  };
}

async function unlockedAfterLaunch(h: ReturnType<typeof harness>) {
  void h.controller.start('launch');
  await h.flush();
  await h.answer({ ok: true });
  expect(h.controller.getState().status).toBe('unlocked');
}

describe('LockController', () => {
  it('locks on launch and opens the prompt straight away', async () => {
    const h = harness();
    void h.controller.start('launch');
    expect(h.controller.getState().status).toBe('deciding');
    await h.flush();
    expect(h.controller.getState()).toMatchObject({ status: 'locked', authInFlight: true });
    expect(h.deviceAuth.authenticate).toHaveBeenCalledTimes(1);
    await h.answer({ ok: true });
    expect(h.controller.getState()).toMatchObject({ status: 'unlocked', authInFlight: false });
    expect(h.lockedChanges).toEqual([true, false]);
  });

  it('stays locked with a hint after a cancelled prompt, and unlock() retries', async () => {
    const h = harness();
    void h.controller.start('launch');
    await h.flush();
    await h.answer({ ok: false, reason: 'cancelled' });
    expect(h.controller.getState()).toMatchObject({ status: 'locked', lastFailure: 'cancelled' });
    void h.controller.unlock();
    void h.controller.unlock(); // ignored while the prompt is up
    expect(h.promptsOpen()).toBe(1);
    await h.answer({ ok: true });
    expect(h.controller.getState().status).toBe('unlocked');
  });

  it('does not lock after a fresh sign-in', async () => {
    const h = harness();
    await h.controller.start('sign_in');
    expect(h.controller.getState().status).toBe('unlocked');
    expect(h.deviceAuth.authenticate).not.toHaveBeenCalled();
  });

  it('does not lock when the policy does not require it', async () => {
    const h = harness({ policy: { require_device_lock: false }, security: 'none' });
    await h.controller.start('launch');
    expect(h.controller.getState().status).toBe('unlocked');
  });

  it('relocks after idle_lock_seconds in the background, not before', async () => {
    const h = harness({ policy: { idle_lock_seconds: 300 } });
    await unlockedAfterLaunch(h);

    h.emit('background');
    h.advance(299_999);
    h.emit('active');
    expect(h.controller.getState().status).toBe('unlocked');

    h.emit('background');
    h.advance(300_000);
    h.emit('active');
    expect(h.controller.getState()).toMatchObject({ status: 'locked', authInFlight: true });
  });

  it('never locks on inactive alone', async () => {
    const h = harness({ policy: { idle_lock_seconds: 0 } });
    await unlockedAfterLaunch(h);
    h.emit('inactive');
    h.advance(10 * 60_000);
    h.emit('active');
    expect(h.controller.getState().status).toBe('unlocked');
  });

  it('idle_lock_seconds 0 never relocks on resume (server semantics); the launch lock stays', async () => {
    const h = harness({ policy: { idle_lock_seconds: 0 } });
    await unlockedAfterLaunch(h);
    h.emit('background');
    h.advance(24 * 3600_000);
    h.emit('active');
    expect(h.controller.getState().status).toBe('unlocked');
  });

  it('ignores AppState changes while the prompt is in flight (no relock loop)', async () => {
    const h = harness({ policy: { idle_lock_seconds: 1 } });
    void h.controller.start('launch');
    await h.flush();
    // The system sheet can background/foreground the app on some devices.
    h.emit('inactive');
    h.emit('background');
    h.emit('active');
    expect(h.promptsOpen()).toBe(1);
    await h.answer({ ok: true });
    h.emit('active');
    expect(h.controller.getState().status).toBe('unlocked');
    expect(h.deviceAuth.authenticate).toHaveBeenCalledTimes(1);
  });

  it('blocks a device without passcode or biometrics, and recovers once one is set', async () => {
    const h = harness({ security: 'none' });
    await h.controller.start('launch');
    expect(h.controller.getState().status).toBe('no_device_security');
    expect(h.deviceAuth.authenticate).not.toHaveBeenCalled();

    h.emit('background');
    h.emit('active');
    await h.flush();
    expect(h.controller.getState().status).toBe('no_device_security');

    h.setSecurity('passcode');
    h.emit('active');
    await h.flush();
    expect(h.controller.getState()).toMatchObject({ status: 'locked', authInFlight: true });
  });

  it('blocks a fresh sign-in on a device without security too', async () => {
    const h = harness({ security: 'none' });
    await h.controller.start('sign_in');
    expect(h.controller.getState().status).toBe('no_device_security');
  });

  it('allows the development bypass only in development builds', async () => {
    const prod = harness({ security: 'none' });
    await prod.controller.start('launch');
    prod.controller.bypassForDevelopment();
    expect(prod.controller.getState().status).toBe('no_device_security');

    const dev = harness({ security: 'none', isDev: true });
    jest.spyOn(console, 'warn').mockImplementation(() => {});
    await dev.controller.start('launch');
    dev.controller.bypassForDevelopment();
    expect(dev.controller.getState()).toMatchObject({ status: 'unlocked', bypassed: true });
    jest.restoreAllMocks();
  });

  it('stop() forgets the session and reports unlocked to the activity gate', async () => {
    const h = harness();
    await unlockedAfterLaunch(h);
    h.emit('background');
    h.controller.stop();
    expect(h.controller.getState().status).toBe('deciding');
    expect(h.lockedChanges[h.lockedChanges.length - 1]).toBe(false);
    h.advance(10 * 60_000);
    h.emit('active');
    expect(h.controller.getState().status).toBe('deciding');
  });

  it('a prompt answered after stop() does not unlock a later session', async () => {
    const h = harness();
    void h.controller.start('launch');
    await h.flush();
    h.controller.stop();
    await h.answer({ ok: true });
    expect(h.controller.getState().status).toBe('deciding');
  });

  describe('policy changes', () => {
    it('blocks at once when a lock becomes required on a device without one', async () => {
      const h = harness({ policy: { require_device_lock: false }, security: 'none' });
      await h.controller.start('launch');
      expect(h.controller.getState().status).toBe('unlocked');
      h.setPolicy({ require_device_lock: true });
      h.controller.policyChanged();
      await h.flush();
      expect(h.controller.getState().status).toBe('no_device_security');
    });

    // DEF-R1 (Agent Zefir E2E run 2): with the app open and unlocked, the
    // workspace turned require_device_lock on. Discovery and the token
    // response delivered it, but the app stayed unlocked until a relaunch.
    it('locks at once and opens the prompt when a lock becomes required while unlocked', async () => {
      const h = harness({ policy: { require_device_lock: false } });
      await h.controller.start('launch');
      expect(h.controller.getState().status).toBe('unlocked');
      h.setPolicy({ require_device_lock: true, idle_lock_seconds: 60 });
      h.controller.policyChanged();
      expect(h.controller.getState().status).toBe('locked');
      expect(h.lockedChanges.at(-1)).toBe(true);
      await h.flush();
      expect(h.promptsOpen()).toBe(1);
      await h.answer({ ok: true });
      expect(h.controller.getState().status).toBe('unlocked');
    });

    it('does not relock when the same policy is delivered again (every token refresh)', async () => {
      const h = harness({ policy: { require_device_lock: true } });
      await unlockedAfterLaunch(h);
      h.controller.policyChanged();
      h.controller.policyChanged();
      await h.flush();
      expect(h.controller.getState().status).toBe('unlocked');
      expect(h.promptsOpen()).toBe(0);
    });

    it('locks again on a second tightening after a loosening', async () => {
      const h = harness({ policy: { require_device_lock: true } });
      await unlockedAfterLaunch(h);
      h.setPolicy({ require_device_lock: false });
      h.controller.policyChanged();
      h.setPolicy({ require_device_lock: true });
      h.controller.policyChanged();
      expect(h.controller.getState().status).toBe('locked');
    });

    it('keeps a fresh sign-in unlocked when its own token response requires the lock', async () => {
      const h = harness({ policy: { require_device_lock: true } });
      await h.controller.start('sign_in');
      h.controller.policyChanged();
      await h.flush();
      expect(h.controller.getState().status).toBe('unlocked');
    });

    it('releases the no-security block when the policy stops requiring a lock', async () => {
      const h = harness({ security: 'none' });
      await h.controller.start('launch');
      expect(h.controller.getState().status).toBe('no_device_security');
      h.setPolicy({ require_device_lock: false });
      h.controller.policyChanged();
      expect(h.controller.getState().status).toBe('unlocked');
    });

    it('applies a shorter idle timeout from the next resume', async () => {
      const h = harness({ policy: { idle_lock_seconds: 300 } });
      await unlockedAfterLaunch(h);
      h.setPolicy({ idle_lock_seconds: 30 });
      h.controller.policyChanged();
      h.emit('background');
      h.advance(30_000);
      h.emit('active');
      expect(h.controller.getState().status).toBe('locked');
    });

    it('does not lock or prompt when the session is being wiped (the reset to strict defaults is no tightening)', async () => {
      let ending = false;
      const h = harness({ policy: { require_device_lock: false }, ending: () => ending });
      await h.controller.start('launch');
      expect(h.controller.getState().status).toBe('unlocked');
      // endSession: "Signing out…", then the policy wipe hook resets to STRICT_DEFAULTS.
      ending = true;
      h.setPolicy(STRICT_DEFAULTS);
      h.controller.policyChanged();
      await h.flush();
      expect(h.controller.getState().status).toBe('unlocked');
      expect(h.promptsOpen()).toBe(0);
      expect(h.deviceAuth.security).toHaveBeenCalledTimes(0);
    });

    it('does nothing while not guarding a session', () => {
      const h = harness({ security: 'none' });
      h.controller.policyChanged();
      expect(h.controller.getState().status).toBe('deciding');
    });
  });
});
