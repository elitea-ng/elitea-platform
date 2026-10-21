import type { AppStateStatus } from 'react-native';

import { log } from '@/lib/log';
import type { ClientPolicy } from '@/security/policy';
import type { AuthOutcome, DeviceAuth } from '@/security/lock/deviceAuth';

/**
 * Device lock state machine (no React; `useLockController` binds it).
 *
 * - Launch: locked when `policy.require_device_lock` (the strict default) and
 *   the unlock prompt opens straight away.
 * - Fresh sign-in: not locked (the user just proved who they are), but the
 *   device-security check still applies.
 * - Workspace switch (`noteSwitch` then `start('switch')`): the user's
 *   presence carries over only when the app was unlocked under a policy that
 *   required the lock; otherwise a workspace that requires it locks and
 *   prompts, like a launch.
 * - Resume: 'background' records the time; on 'active' the app locks when
 *   `now − t ≥ idle_lock_seconds`. 0 means it never relocks on resume (the
 *   server's definition, ADR-0025 `native_client_policy`); the launch lock
 *   still applies. 'inactive' is ignored: iOS passes through it for Control
 *   Center, notifications and the Face ID sheet.
 * - Policy change (`policyChanged`): a tightened policy applies at once. When
 *   a lock becomes required while the app is unlocked, the app locks and the
 *   prompt opens now, not at the next launch (DEF-R1); a device without a
 *   passcode is blocked straight away. A policy that no longer requires a
 *   lock releases a lock that is waiting for the user. A changed
 *   `idle_lock_seconds` applies from the next resume (it is read live then).
 *   Ignored while the session is being wiped: the reset to the strict
 *   defaults is the end of the session, not a tightening.
 * - While the unlock prompt is up (`authInFlight`), AppState changes are
 *   ignored, so the prompt itself never causes another lock.
 * - A device without a passcode cannot satisfy the policy: the state is
 *   `no_device_security` (blocking; only "Log out & wipe"), re-checked on
 *   every resume in case the user set one up. Development builds may bypass.
 */

export type LockStatus =
  /** Not decided yet (launch). The app shows an opaque cover. */
  | 'deciding'
  | 'locked'
  | 'unlocked'
  /** Policy requires a lock but the device has no passcode or biometrics. */
  | 'no_device_security';

export interface LockState {
  status: LockStatus;
  /** The unlock prompt is on screen. */
  authInFlight: boolean;
  /** Why the last unlock attempt failed, for a one-line hint. */
  lastFailure: Extract<AuthOutcome, { ok: false }>['reason'] | null;
  /** A development bypass is active (simulators without a passcode). */
  bypassed: boolean;
}

interface AppStateLike {
  addEventListener(type: 'change', listener: (state: AppStateStatus) => void): { remove(): void };
}

export interface LockControllerOptions {
  deviceAuth: DeviceAuth;
  getPolicy: () => Readonly<ClientPolicy>;
  appState: AppStateLike;
  now?: () => number;
  isDev?: boolean;
  /**
   * The session is being ended and wiped (`endSession`). Policy changes are
   * ignored meanwhile: the wipe resets the policy to the strict defaults,
   * which is not a server tightening and must not open an unlock prompt over
   * "Signing out…".
   */
  isSessionEnding?: () => boolean;
  /** Told whenever the app becomes (un)locked; pauses streams and refetch. */
  onLockedChange?: (locked: boolean) => void;
  promptMessage?: string;
}

const INITIAL: LockState = { status: 'deciding', authInFlight: false, lastFailure: null, bypassed: false };

export class LockController {
  private state: LockState = INITIAL;
  private listeners = new Set<() => void>();
  private appSub: { remove(): void } | null = null;
  private backgroundAt: number | null = null;
  /** `require_device_lock` as last applied, to tell a tightening from a re-delivery. */
  private lockRequired = false;
  /** The user proved presence before the current workspace switch (see `noteSwitch`). */
  private presenceCarried = false;
  private generation = 0;
  private readonly now: () => number;

  constructor(private readonly opts: LockControllerOptions) {
    this.now = opts.now ?? Date.now;
  }

  getState = (): LockState => this.state;

  /**
   * True while a guarded session is behind the lock (deciding, locked, or no
   * device security): anything drawn in its own native window (a Modal) or
   * heard (read aloud, dictation) must not outlive it. False when unlocked
   * and when not guarding at all (signed out).
   */
  covers = (): boolean => this.appSub !== null && this.state.status !== 'unlocked';

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  /**
   * Starts guarding a signed-in session. `launch`: the session was restored
   * at app start (lock first). `sign_in`: the user just signed in.
   */
  async start(kind: 'launch' | 'sign_in' | 'switch'): Promise<void> {
    const carried = this.presenceCarried;
    this.presenceCarried = false;
    this.detach();
    const generation = this.generation;
    this.appSub = this.opts.appState.addEventListener('change', (s) => this.onAppState(s));
    this.opts.onLockedChange?.(true);
    this.set({ ...INITIAL });
    const policy = this.opts.getPolicy();
    this.lockRequired = policy.require_device_lock;
    if (!policy.require_device_lock) {
      this.set({ status: 'unlocked' });
      return;
    }
    const secure = await this.deviceIsSecure();
    if (generation !== this.generation) return;
    if (!secure) {
      this.set({ status: 'no_device_security' });
      return;
    }
    if (kind === 'sign_in' || (kind === 'switch' && carried)) {
      this.set({ status: 'unlocked' });
      return;
    }
    this.set({ status: 'locked' });
    await this.unlock();
  }

  /**
   * The app is about to switch to another connected workspace: remembers
   * whether the user has proved presence (unlocked under a lock-requiring
   * policy, not a development bypass), for the `start('switch')` that follows.
   */
  private noteSwitch(): void {
    this.presenceCarried = this.state.status === 'unlocked' && this.lockRequired && !this.state.bypassed;
  }

  private switchNoted = false;

  /** Marks the next `start` as a switch (the binding asks `consumeSwitch`). */
  markSwitch(): void {
    this.noteSwitch();
    this.switchNoted = true;
  }

  consumeSwitch(): boolean {
    const noted = this.switchNoted;
    this.switchNoted = false;
    return noted;
  }

  /** Stops guarding (signed out). The state returns to 'deciding'. */
  stop(): void {
    this.detach();
    this.state = { ...INITIAL };
    // Nothing to protect while signed out (connect and sign-in screens).
    this.opts.onLockedChange?.(false);
    for (const listener of [...this.listeners]) listener();
  }

  private detach(): void {
    this.generation++;
    this.appSub?.remove();
    this.appSub = null;
    this.backgroundAt = null;
  }

  /** Shows the unlock prompt. Concurrent calls while it is up are ignored. */
  async unlock(): Promise<void> {
    if (this.state.status !== 'locked' || this.state.authInFlight) return;
    const generation = this.generation;
    this.set({ authInFlight: true, lastFailure: null });
    let outcome: AuthOutcome;
    try {
      outcome = await this.opts.deviceAuth.authenticate(
        this.opts.promptMessage ?? 'Unlock your workspace',
      );
    } catch {
      outcome = { ok: false, reason: 'failed' };
    }
    if (generation !== this.generation) return;
    if (outcome.ok) {
      this.backgroundAt = null;
      this.set({ status: 'unlocked', authInFlight: false, lastFailure: null });
    } else {
      this.set({ authInFlight: false, lastFailure: outcome.reason });
    }
  }

  /** The client policy changed (server or discovery). Applies a tightening at once. */
  policyChanged(): void {
    if (this.appSub === null || this.state.status === 'deciding') return;
    if (this.opts.isSessionEnding?.()) return;
    const policy = this.opts.getPolicy();
    const tightened = policy.require_device_lock && !this.lockRequired;
    this.lockRequired = policy.require_device_lock;
    if (this.state.authInFlight) return;
    if (!policy.require_device_lock) {
      if (this.state.status === 'locked' || this.state.status === 'no_device_security') {
        this.set({ status: 'unlocked', lastFailure: null });
      }
      return;
    }
    if (this.state.status !== 'unlocked' || this.state.bypassed) return;
    const generation = this.generation;
    if (tightened) {
      // Lock now (synchronously, so nothing renders unguarded meanwhile); the
      // device-security check then either prompts or blocks.
      this.set({ status: 'locked' });
    }
    void this.deviceIsSecure().then((secure) => {
      if (generation !== this.generation) return;
      if (!secure) {
        if (this.state.status === 'unlocked' || (tightened && this.state.status === 'locked')) {
          this.set({ status: 'no_device_security' });
        }
        return;
      }
      if (tightened && this.state.status === 'locked') void this.unlock();
    });
  }

  /** Development builds only: continue without device security (simulators). */
  bypassForDevelopment(): void {
    if (!this.opts.isDev) return;
    if (this.state.status !== 'no_device_security' && this.state.status !== 'locked') return;
    log.warn('device lock bypassed (development build)');
    this.set({ status: 'unlocked', bypassed: true, authInFlight: false });
  }

  private async deviceIsSecure(): Promise<boolean> {
    try {
      const security = await this.opts.deviceAuth.security();
      if (security === 'unsupported') return true; // web: development surface, nothing stored
      return security !== 'none';
    } catch (error) {
      log.warn('device security level unavailable', { error });
      return false;
    }
  }

  private onAppState(next: AppStateStatus): void {
    if (this.state.authInFlight) return;
    if (next === 'background') {
      if (this.state.status === 'unlocked' && this.backgroundAt === null) this.backgroundAt = this.now();
      return;
    }
    if (next !== 'active') return;

    if (this.state.status === 'no_device_security') {
      const generation = this.generation;
      void this.deviceIsSecure().then((secure) => {
        if (generation === this.generation && secure && this.state.status === 'no_device_security') {
          this.set({ status: 'locked' });
          void this.unlock();
        }
      });
      return;
    }

    const since = this.backgroundAt;
    this.backgroundAt = null;
    if (since === null || this.state.status !== 'unlocked' || this.state.bypassed) return;
    const policy = this.opts.getPolicy();
    if (!policy.require_device_lock || policy.idle_lock_seconds === 0) return;
    const idleMs = policy.idle_lock_seconds * 1000;
    if (this.now() - since >= idleMs) {
      // Synchronous: appActivity forwards 'active' a microtask later and must
      // already see the lock.
      this.set({ status: 'locked' });
      void this.unlock();
    }
  }

  private set(patch: Partial<LockState>): void {
    const next = { ...this.state, ...patch };
    const wasLocked = this.state.status !== 'unlocked';
    this.state = next;
    const isLocked = next.status !== 'unlocked';
    if (wasLocked !== isLocked) this.opts.onLockedChange?.(isLocked);
    for (const listener of [...this.listeners]) listener();
  }
}
