import { abortAll } from '@/lib/abortRegistry';
import { log } from '@/lib/log';
import { wipeCachesDirectory } from '@/security/cacheWipe';
import {
  SECURE_KEYS,
  deleteAllSecureItems,
  deleteSecureItem,
  getSecureItem,
  setSecureItem,
} from '@/security/secureStorage';
import { getSession, resetSession, setSession, type EndReason } from '@/state/session';

/**
 * THE logout-and-wipe orchestrator: the one exit path from a session.
 *
 * Callers: Settings "Log out & wipe", a 401 `device_revoked`, a rejected PAT,
 * and (B3/B4) server switch, policy breach and "can't unlock → reset".
 *
 * Every step is attempted even when an earlier one fails; errors are collected
 * and returned, never thrown. Concurrent callers share one run, so ten parallel
 * 401s produce one wipe.
 *
 * Order (each step is attempted; a failure is recorded and the next one runs):
 *   1. crash-resume marker `az.wipe.pending`, then `session.ending`;
 *   2. abort every in-flight REST call and stream (abort registry);
 *   3. best-effort server revoke, capped at `REVOKE_TIMEOUT_MS` (B4 registers
 *      the native refresh-family revoke; a PAT belongs to the user and is never
 *      deleted server-side, so nothing is registered for it);
 *   4. wipe hooks: query cache, live turns, (B4) the encrypted store; then
 *      the app's caches directory, which holds the platform URL cache
 *      (`Library/Caches/<bundle id>/Cache.db` on iOS, DEF-R2);
 *   5. every registered secure-storage key except the marker;
 *   6. the in-memory session;
 *   7. the marker, last, and only when every secure delete in step 5
 *      succeeded.
 *
 * A crash anywhere before step 7, or a secure delete that failed, leaves the
 * marker behind; launch hydration finds it and calls `resumePendingWipe()`,
 * which runs the whole thing again before anything is restored.
 */

export interface EndSessionOptions {
  reason: EndReason;
}

export interface EndSessionResult {
  reason: EndReason;
  /** One entry per failed step; empty when everything succeeded. */
  errors: { step: string; error: unknown }[];
  /**
   * False when a secure-storage item could not be deleted. The wipe-pending
   * marker is then kept, so the next launch retries the wipe; until then a
   * credential may still be stored and nothing may be connected over it.
   */
  storageWiped: boolean;
}

/** True when the step failure belongs to the secure-storage wipe (step 5). */
function isStorageFailure(step: string): boolean {
  return step === 'delete_secure_items' || step.startsWith('delete:');
}

/** Extra wipe work registered by other modules (e.g. the query cache in B2). */
export type WipeHook = () => void | Promise<void>;
const wipeHooks = new Map<string, WipeHook>();

/** Registers `hook` under `name` (re-registering replaces it). Returns an unregister function. */
export function registerWipeHook(name: string, hook: WipeHook): () => void {
  wipeHooks.set(name, hook);
  return () => {
    if (wipeHooks.get(name) === hook) wipeHooks.delete(name);
  };
}

/** Upper bound for the best-effort server revoke; a wipe never waits longer for the network. */
export const REVOKE_TIMEOUT_MS = 3000;

/**
 * Server-side revoke of the current credential. It runs while the credential
 * is still available, after in-flight work is aborted and before anything is
 * deleted. The signal aborts when the cap is reached.
 */
export type RevokeHook = (signal: AbortSignal) => Promise<void>;
let revokeHook: RevokeHook | null = null;

/** Sets (or with null clears) the server revoke. Returns an unregister function. */
export function registerRevokeHook(hook: RevokeHook | null): () => void {
  revokeHook = hook;
  return () => {
    if (revokeHook === hook) revokeHook = null;
  };
}

async function revokeWithCap(hook: RevokeHook): Promise<void> {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | null = null;
  const cap = new Promise<never>((_, reject) => {
    timer = setTimeout(() => {
      controller.abort(new Error('revoke_timeout'));
      reject(new Error('revoke_timeout'));
    }, REVOKE_TIMEOUT_MS);
  });
  try {
    await Promise.race([hook(controller.signal), cap]);
  } finally {
    if (timer !== null) clearTimeout(timer);
  }
}

let inFlight: Promise<EndSessionResult> | null = null;

export function endSession(options: EndSessionOptions): Promise<EndSessionResult> {
  if (inFlight) return inFlight;
  inFlight = run(options).finally(() => {
    inFlight = null;
  });
  return inFlight;
}

export function isEndingSession(): boolean {
  return inFlight !== null;
}

async function run({ reason }: EndSessionOptions): Promise<EndSessionResult> {
  const errors: EndSessionResult['errors'] = [];
  const step = async (name: string, fn: () => unknown) => {
    try {
      await fn();
    } catch (error) {
      errors.push({ step: name, error });
    }
  };

  const originBefore = getSession().serverOrigin;

  // 1. Crash-resume marker first, then refuse new work and show "Signing out…".
  await step('mark_pending', () =>
    setSecureItem(SECURE_KEYS.wipePending, JSON.stringify({ reason, startedAt: Date.now() })),
  );
  await step('mark_ending', () => setSession({ ending: true, endReason: reason }));

  // 2. Stop everything in flight.
  await step('abort_in_flight', () => abortAll(`session_ended:${reason}`));

  // 3. Server-side revoke, best effort and capped. Never for a revocation the
  //    server itself announced (device_revoked): the credential is already dead.
  const revoke = revokeHook;
  if (revoke && reason !== 'device_revoked') {
    await step('server_revoke', () => revokeWithCap(revoke));
  }

  // 4. Caches and other registered local state.
  for (const [name, hook] of [...wipeHooks]) {
    await step(`hook:${name}`, hook);
  }
  // Plaintext HTTP responses (tokens, transcripts) a URL cache may have kept.
  await step('clear_caches', () => wipeCachesDirectory());

  // 5. Every registered secure-storage key except the marker.
  await step('delete_secure_items', async () => {
    const failures = await deleteAllSecureItems([SECURE_KEYS.wipePending]);
    for (const failure of failures) {
      errors.push({ step: `delete:${failure.key}`, error: failure.error });
    }
  });

  // 6. In-memory session. A rejected credential (not a revocation) keeps a
  //    non-secret origin hint so signing in again is quick.
  await step('reset_session', () =>
    resetSession({
      endReason: reason,
      lastOriginHint: reason === 'credential_rejected' ? originBefore : null,
    }),
  );

  // 7. Marker last, and only once every secure item is gone: a crash before
  //    this point, or a key that could not be deleted, re-runs the wipe at
  //    launch before any stored credential is read.
  const storageWiped = !errors.some((e) => isStorageFailure(e.step));
  if (storageWiped) {
    await step('clear_pending', () => deleteSecureItem(SECURE_KEYS.wipePending));
  }

  if (errors.length > 0) {
    log.warn('endSession finished with errors', {
      reason,
      steps: errors.map((e) => e.step),
    });
  }
  return { reason, errors, storageWiped };
}

const END_REASONS: readonly EndReason[] = [
  'user_logout',
  'device_revoked',
  'credential_rejected',
  'server_switch',
  'policy',
  'reset',
];

/** A resumed wipe that still could not delete every secure item; the marker is kept. */
export class WipeIncompleteError extends Error {
  constructor() {
    super('interrupted wipe could not delete every secure item');
    this.name = 'WipeIncompleteError';
  }
}

/**
 * Launch-time half of the crash-resume marker: when a previous wipe did not
 * finish (`az.wipe.pending` is still stored), run it again before anything is
 * read or shown. Returns true when a wipe was resumed.
 *
 * A marker that cannot be parsed still triggers the wipe (reason `reset`):
 * its presence alone means a wipe was under way. A marker that cannot be READ
 * (keychain unavailable) is reported as an error to the caller, which retries
 * later instead of guessing. A resumed wipe that still could not delete every
 * secure item rejects with `WipeIncompleteError`: the marker stays, and the
 * caller must not restore anything; the next launch tries the wipe again.
 */
export async function resumePendingWipe(): Promise<boolean> {
  const raw = await getSecureItem(SECURE_KEYS.wipePending);
  if (raw === null) return false;
  let reason: EndReason = 'reset';
  try {
    const parsed: unknown = JSON.parse(raw);
    const candidate = (parsed as { reason?: unknown } | null)?.reason;
    if (typeof candidate === 'string' && (END_REASONS as readonly string[]).includes(candidate)) {
      reason = candidate as EndReason;
    }
  } catch {
    // keep 'reset'
  }
  log.info('resuming an interrupted wipe', { reason });
  const result = await endSession({ reason });
  if (!result.storageWiped) {
    throw new WipeIncompleteError();
  }
  return true;
}
