import { AppState, type AppStateStatus } from 'react-native';

/**
 * The app's effective activity: the OS AppState, except that a LOCKED app
 * counts as 'background'. Work that pauses in the background (event streams,
 * react-query refetch-on-focus) listens here instead of AppState, so the lock
 * screen pauses it too and unlocking resumes it.
 *
 * A real 'active' is forwarded on a microtask, after the lock controller has
 * seen the same event and decided whether resuming locks the app; streams
 * therefore never reconnect for an instant behind a lock that is about to
 * close.
 */

type Listener = (state: AppStateStatus) => void;

const APP_STATE_STATUSES: readonly string[] = ['active', 'background', 'inactive', 'extension', 'unknown'];

function isAppStateStatus(value: string | null | undefined): value is AppStateStatus {
  return value != null && APP_STATE_STATUSES.includes(value);
}

interface RealAppState {
  /** React Native 0.88 types this as a plain string; the values are AppStateStatus. */
  currentState: string | null | undefined;
  addEventListener(type: 'change', listener: Listener): { remove(): void };
}

export function createAppActivity(real: RealAppState) {
  const listeners = new Set<Listener>();
  let locked = false;
  let realSub: { remove(): void } | null = null;
  let lastEmitted: AppStateStatus | null = null;

  const effective = (): AppStateStatus => {
    const current = isAppStateStatus(real.currentState) ? real.currentState : 'active';
    return locked && current !== 'background' ? 'background' : current;
  };

  const emit = () => {
    const next = effective();
    if (next === lastEmitted) return;
    lastEmitted = next;
    for (const listener of [...listeners]) listener(next);
  };

  const ensureRealSub = () => {
    if (realSub) return;
    lastEmitted = effective();
    realSub = real.addEventListener('change', (state) => {
      if (state === 'active') queueMicrotask(emit);
      else emit();
    });
  };

  return {
    get currentState(): AppStateStatus {
      return effective();
    },
    addEventListener(_type: 'change', listener: Listener): { remove(): void } {
      ensureRealSub();
      listeners.add(listener);
      return {
        remove: () => {
          listeners.delete(listener);
          if (listeners.size === 0 && realSub) {
            realSub.remove();
            realSub = null;
          }
        },
      };
    },
    /** Called by the lock controller. */
    setLocked(value: boolean): void {
      if (locked === value) return;
      locked = value;
      if (listeners.size > 0) emit();
    },
    get isLocked(): boolean {
      return locked;
    },
  };
}

export type AppActivity = ReturnType<typeof createAppActivity>;

export const appActivity: AppActivity = createAppActivity(AppState);
