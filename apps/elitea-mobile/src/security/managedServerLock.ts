import { log } from '@/lib/log';
import { getManagedConfig, subscribeManagedConfig } from '@/security/managedConfig';
import { lockedOrigin } from '@/security/managedServer';
import { subscribeRegistry } from '@/security/workspaceRegistry';
import { getSession, subscribeSession } from '@/state/session';
import { keepOnlyWorkspace } from '@/state/workspace';

/**
 * MDM `lock_server` (ADR-0052 W1b, decision 7): with `server_url` set, the app
 * keeps one workspace. It is applied once the session is hydrated (the
 * resumed wipes are done and the registry is readable) and again on every
 * change of the managed configuration while the app runs.
 *
 * Every other workspace is signed out and wiped like a logout
 * (`keepOnlyWorkspace`). When the MDM removes the key nothing comes back.
 * `lock_server` without a usable `server_url` locks nothing: there is no
 * server to keep, and wiping every workspace on a typo is not "tightening".
 */

export interface ManagedServerLockDeps {
  keepOnly?: (origin: string) => Promise<unknown>;
  allowPrivateHttp?: boolean;
}

let started = false;
let running: Promise<void> | null = null;
let again = false;

function apply(deps: ManagedServerLockDeps): Promise<void> {
  if (running) {
    // Picked up by the run under way, or by a fresh one if it already left its loop.
    again = true;
    return running;
  }
  running = (async () => {
    do {
      again = false;
      if (!getSession().hydrated) continue;
      const origin = lockedOrigin(getManagedConfig(), deps.allowPrivateHttp);
      if (origin === null) continue;
      try {
        // Cheap when nothing is left to remove (one origin read per slot).
        await (deps.keepOnly ?? keepOnlyWorkspace)(origin);
      } catch (error) {
        log.warn('managed server lock not applied', { error });
      }
    } while (again);
  })().finally(() => {
    running = null;
    // A request that arrived after the loop's last check but before this point.
    if (again) void apply(deps);
  });
  return running;
}

/**
 * Starts following `lock_server`: applied once hydrated, on every change of
 * the managed configuration, and on every change of the registry, so a
 * workspace a run could not read (or a wipe left unfinished) is tried again
 * and nothing on another server outlives the lock. `connectWorkspace`
 * refuses another server meanwhile.
 */
export function startManagedServerLock(deps: ManagedServerLockDeps = {}): () => void {
  if (started) return () => {};
  started = true;
  const offManaged = subscribeManagedConfig(() => void apply(deps));
  const offRegistry = subscribeRegistry(() => void apply(deps));
  let wasHydrated = getSession().hydrated;
  const offSession = subscribeSession(() => {
    const hydrated = getSession().hydrated;
    if (hydrated && !wasHydrated) void apply(deps);
    wasHydrated = hydrated;
  });
  void apply(deps);
  return () => {
    offManaged();
    offRegistry();
    offSession();
    started = false;
  };
}

/** Tests only: the run under way (or a settled promise). */
export function managedServerLockSettled(): Promise<void> {
  return running ?? Promise.resolve();
}

/** Test helper. */
export function __resetManagedServerLockForTests(): void {
  started = false;
  running = null;
  again = false;
}
